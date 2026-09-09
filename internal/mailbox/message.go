package mailbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	gomessage "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // side-effect: registers charset decoders
)

// Fields are the message fields relevant to deduplication.
type Fields struct {
	MessageID string    // normalized Message-ID (without <>), "" if absent
	Date      time.Time // sent date, zero if not parsed
	Subject   string    // decoded subject
	From      string    // normalized sender address
	HashHdr   string    // X-Imapsync-Hash value if we already tagged this message
}

// ParseFields parses a raw header block (RFC 822) into Fields.
// hashHeader is the name of the custom surrogate-hash header (from config).
func ParseFields(rawHeader []byte, hashHeader string) (Fields, error) {
	// net/mail expects headers, a blank line and a body; the body may be empty.
	buf := rawHeader
	if !bytes.HasSuffix(buf, []byte("\n\n")) && !bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
		buf = append(append([]byte{}, buf...), '\r', '\n')
	}
	msg, err := mail.ReadMessage(bytes.NewReader(buf))
	if err != nil {
		return Fields{}, fmt.Errorf("parsing message headers: %w", err)
	}
	h := msg.Header

	var f Fields
	f.MessageID = NormalizeMessageID(h.Get("Message-Id"))
	f.Subject = strings.TrimSpace(decodeHeader(h.Get("Subject")))
	f.From = NormalizeAddr(h.Get("From"))
	f.HashHdr = strings.TrimSpace(h.Get(hashHeader))

	if ds := strings.TrimSpace(h.Get("Date")); ds != "" {
		if t, derr := mail.ParseDate(ds); derr == nil {
			f.Date = t
		}
	}
	return f, nil
}

// decodeHeader decodes MIME encoded-words (=?UTF-8?B?...?=), including
// non-standard charsets (via go-message/charset). On error returns the input.
func decodeHeader(s string) string {
	dec := mime.WordDecoder{CharsetReader: gomessage.CharsetReader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// NormalizeMessageID trims spaces and strips the angle brackets.
func NormalizeMessageID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// NormalizeAddr lowercases From and extracts the bare address if it parses;
// otherwise it trims and lowercases the whole string.
func NormalizeAddr(s string) string {
	s = strings.TrimSpace(decodeHeader(s))
	if addr, err := mail.ParseAddress(s); err == nil {
		return strings.ToLower(strings.TrimSpace(addr.Address))
	}
	return strings.ToLower(s)
}

// SurrogateHash computes a stable surrogate key for a message from
// Date+Subject+From. Date is converted to a UTC unix timestamp; a zero date
// gives 0.
func SurrogateHash(f Fields) string {
	var unix int64
	if !f.Date.IsZero() {
		unix = f.Date.UTC().Unix()
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s", unix, f.Subject, f.From)
	return hex.EncodeToString(h.Sum(nil))
}

// Identity returns the final key for comparing a message between folders:
// Message-ID -> previously written X-Imapsync-Hash -> computed surrogate hash.
func Identity(f Fields) string {
	if f.MessageID != "" {
		return "mid:" + f.MessageID
	}
	if f.HashHdr != "" {
		return "hash:" + f.HashHdr
	}
	return "hash:" + SurrogateHash(f)
}

// MatchKeys returns ALL keys by which this message may match a copy on the other
// side. A message is considered identical to another if any pair of keys matches.
//
// The surrogate hash is always present - this guards against duplication in the
// Exchange scenario where Message-ID appeared on one side later: there the
// message already has "mid:", while its copy on the other side has only the
// "hash:<surrogate>" we wrote, and that is what they match on.
func MatchKeys(f Fields) []string {
	return MatchKeysFrom(f.MessageID, f.HashHdr, SurrogateHash(f))
}

// MatchKeysFrom is the same as MatchKeys but takes the already-computed
// surrogate hash directly (e.g. loaded from cache) instead of recomputing it
// from fields.
func MatchKeysFrom(messageID, hashHdr, surrogate string) []string {
	keys := make([]string, 0, 3)
	if messageID != "" {
		keys = append(keys, "mid:"+messageID)
	}
	if hashHdr != "" {
		keys = append(keys, "hash:"+hashHdr)
	}
	sur := "hash:" + surrogate
	if len(keys) == 0 || keys[len(keys)-1] != sur {
		keys = append(keys, sur)
	}
	return keys
}

// HasHeader reports whether the header block of a raw message contains a header
// named name (case-insensitive).
func HasHeader(raw []byte, name string) bool {
	prefixLC := strings.ToLower(name) + ":"
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		headerEnd = len(raw)
	}
	for line := range bytes.SplitSeq(raw[:headerEnd], []byte("\r\n")) {
		if strings.HasPrefix(strings.ToLower(string(line)), prefixLC) {
			return true
		}
	}
	return false
}

// InjectHashHeader prepends a "hashHeader: value" header to the raw message so
// later passes can find the already-copied message by it. If such a header is
// already present the message is returned unchanged.
func InjectHashHeader(raw []byte, hashHeader, value string) []byte {
	if HasHeader(raw, hashHeader) {
		return raw
	}
	hdr := fmt.Appendf(nil, "%s: %s\r\n", hashHeader, value)
	return append(hdr, raw...)
}
