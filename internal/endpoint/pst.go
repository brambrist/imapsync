package endpoint

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/textproto"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pst "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/mooijtech/go-pst/v6/pkg/properties"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"

	"imapsync/config"
)

// pstBackend - a read-only PST/OST archive as a sync endpoint. There is no
// network and no auth: Connect only expands the path template and opens the
// file. Append is unsupported (ReadOnly reports true), so the syncer only ever
// copies away from a PST, never into it - a one-way import.
type pstBackend struct {
	pathTmpl string
}

func newPSTBackend(srv config.Server) *pstBackend {
	return &pstBackend{pathTmpl: srv.Root}
}

func (b *pstBackend) Addr() string { return "pst:" + b.pathTmpl }

func (b *pstBackend) Connect(_ context.Context, user string) (Endpoint, error) {
	path := expandUser(b.pathTmpl, user)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pst %s (user %s): %w", path, user, err)
	}
	pf, err := pst.New(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("pst %s (user %s): %w", path, user, err)
	}
	return &pstEndpoint{path: path, user: user, file: f, pf: pf}, nil
}

// pstEndpoint - an open PST file with one selected folder. Select loads the
// folder's messages once into msgs (keyed by node identifier); ListIDs /
// FetchMeta / Open all read from that map.
type pstEndpoint struct {
	path   string
	user   string
	file   *os.File
	pf     *pst.File
	folder string
	msgs   map[string]*pst.Message
}

// SPECIAL-USE tokens -> candidate PST folder names (Outlook UI language
// dependent; English first). Matched case-insensitively.
var pstSpecial = map[string][]string{
	`\Sent`:    {"Sent Items", "Sent", "Sent Messages"},
	`\Drafts`:  {"Drafts"},
	`\Trash`:   {"Deleted Items", "Trash"},
	`\Junk`:    {"Junk Email", "Junk E-mail", "Junk"},
	`\Archive`: {"Archive", "Archive Items"},
}

func (e *pstEndpoint) Select(name string) (folder, validity string, err error) {
	var folders []*pst.Folder
	if werr := e.pf.WalkFolders(func(f *pst.Folder) error {
		c := *f
		folders = append(folders, &c)
		return nil
	}); werr != nil {
		return "", "", fmt.Errorf("pst %s: walking folders: %w", e.path, werr)
	}

	target := resolvePSTFolder(name, folders)
	if target == nil {
		var names []string
		for _, f := range folders {
			if f.Name != "" {
				names = append(names, f.Name)
			}
		}
		return "", "", fmt.Errorf("pst %s: folder %q not found (have: %s)", e.path, name, strings.Join(names, ", "))
	}

	e.msgs = map[string]*pst.Message{}
	if target.MessageCount > 0 {
		it, ierr := target.GetMessageIterator()
		if ierr != nil && !errors.Is(ierr, pst.ErrMessagesNotFound) {
			return "", "", fmt.Errorf("pst %s, folder %q: %w", e.path, target.Name, ierr)
		}
		for it.Next() {
			m := it.Value()
			e.msgs[strconv.FormatInt(int64(m.Identifier), 10)] = m
		}
		if ierr := it.Err(); ierr != nil {
			return "", "", fmt.Errorf("pst %s, folder %q: iterating messages: %w", e.path, target.Name, ierr)
		}
	}

	e.folder = target.Name
	// A PST has no validity token: node identifiers are stable within one file.
	// If the file is swapped underneath us the periodic full_resync_every rescan
	// heals any drift.
	return target.Name, "pst", nil
}

// resolvePSTFolder matches a folder name: exact -> SPECIAL-USE candidates ->
// "INBOX" alias -> case-insensitive.
func resolvePSTFolder(name string, folders []*pst.Folder) *pst.Folder {
	n := strings.TrimSpace(name)
	for _, f := range folders {
		if f.Name == n {
			return f
		}
	}
	if cands, ok := pstSpecial[n]; ok {
		for _, c := range cands {
			for _, f := range folders {
				if strings.EqualFold(f.Name, c) {
					return f
				}
			}
		}
	}
	if strings.EqualFold(n, "INBOX") {
		for _, f := range folders {
			if strings.EqualFold(f.Name, "Inbox") {
				return f
			}
		}
	}
	for _, f := range folders {
		if strings.EqualFold(f.Name, n) {
			return f
		}
	}
	return nil
}

func (e *pstEndpoint) ListIDs() ([]string, error) {
	ids := make([]string, 0, len(e.msgs))
	for id := range e.msgs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (e *pstEndpoint) FetchMeta(ids []string) ([]Message, error) {
	if ids == nil {
		ids = make([]string, 0, len(e.msgs))
		for id := range e.msgs {
			ids = append(ids, id)
		}
	}
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		m, ok := e.msgs[id]
		if !ok {
			continue
		}
		hdr, date, flags, err := pstMeta(m)
		if err != nil {
			return nil, fmt.Errorf("pst %s, id=%s: %w", e.path, id, err)
		}
		out = append(out, Message{
			ID:           id,
			Flags:        flags,
			InternalDate: date,
			Header:       hdr,
		})
	}
	return out, nil
}

func (e *pstEndpoint) Open(id string) (Literal, error) {
	m, ok := e.msgs[id]
	if !ok {
		return nil, fmt.Errorf("pst: message %q not found in %q", id, e.folder)
	}
	raw, err := pstRender(m)
	if err != nil {
		return nil, fmt.Errorf("pst %s, id=%s: %w", e.path, id, err)
	}
	return bytes.NewBuffer(raw), nil
}

// Append is unsupported: a PST archive is read-only.
func (e *pstEndpoint) Append([]string, time.Time, Literal) (string, error) {
	return "", errors.New("pst: append is not supported (read-only archive)")
}

func (e *pstEndpoint) ReadOnly() bool { return true }

func (e *pstEndpoint) Close() {
	if e.pf != nil {
		e.pf.Cleanup()
		e.pf = nil
	}
	if e.file != nil {
		e.file.Close()
		e.file = nil
	}
	e.msgs = nil
}

// --- MAPI -> RFC 822 ---

// mapiProps is the subset of properties.Message accessors used here. Only the
// IPM.Note class is fully covered; other classes fall back to a minimal message.
type mapiProps interface {
	GetSubject() string
	GetInternetMessageId() string
	GetTransportMessageHeaders() string
	GetClientSubmitTime() int64
	GetMessageDeliveryTime() int64
	GetSenderName() string
	GetSenderEmailAddress() string
	GetSmtpAddress() string
	GetSentRepresentingEmailAddress() string
	GetDisplayTo() string
	GetDisplayCc() string
	GetMessageFlags() int32
	GetBody() string
	GetBodyHtml() string
}

// pstMeta builds just the header block (Message-ID / Date / From / Subject) for
// the deduplication index, plus the internal date and flags.
func pstMeta(m *pst.Message) (header []byte, date time.Time, flags []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while reading message: %v", r)
		}
	}()

	p, ok := m.Properties.(mapiProps)
	if !ok {
		var b strings.Builder
		fmt.Fprintf(&b, "Subject: %s\r\n\r\n", encodeWord(pstSubjectOf(m)))
		return []byte(b.String()), time.Time{}, nil, nil
	}

	date = mapiDate(p)
	flags = mapiFlags(p)

	if raw := strings.TrimSpace(p.GetTransportMessageHeaders()); raw != "" {
		return withBlankLine(strippedTransportHeaders(raw)), date, flags, nil
	}

	var b strings.Builder
	wr := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	wr("Message-ID", strings.TrimSpace(p.GetInternetMessageId()))
	if !date.IsZero() {
		wr("Date", date.Format(time.RFC1123Z))
	}
	wr("From", mapiFrom(p))
	wr("Subject", encodeWord(p.GetSubject()))
	b.WriteString("\r\n")
	return []byte(b.String()), date, flags, nil
}

// pstRender builds the full RFC 822 message: a header block plus a reconstructed
// body (HTML > plaintext > decoded RTF) and, if present, by-value attachments in
// a multipart/mixed container.
func pstRender(m *pst.Message) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while rendering message: %v", r)
		}
	}()

	p, ok := m.Properties.(mapiProps)
	if !ok {
		var b bytes.Buffer
		fmt.Fprintf(&b, "Subject: %s\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n\r\n", encodeWord(pstSubjectOf(m)))
		return b.Bytes(), nil
	}

	ctype, body := pstBody(p, m)
	atts := pstAttachments(m)

	var hdr bytes.Buffer
	wr := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&hdr, "%s: %s\r\n", k, v)
		}
	}

	if raw := strings.TrimSpace(p.GetTransportMessageHeaders()); raw != "" {
		// Received mail: keep the original headers (Received, DKIM, ...) but drop
		// the MIME framing - we rebuild the body from the property bag, so the
		// original Content-Type / boundary no longer apply.
		hdr.Write(strippedTransportHeaders(raw))
	} else {
		wr("Message-ID", strings.TrimSpace(p.GetInternetMessageId()))
		if d := mapiDate(p); !d.IsZero() {
			wr("Date", d.Format(time.RFC1123Z))
		}
		wr("From", mapiFrom(p))
		wr("To", nameList(p.GetDisplayTo()))
		wr("Cc", nameList(p.GetDisplayCc()))
		wr("Subject", encodeWord(p.GetSubject()))
	}
	wr("MIME-Version", "1.0")

	if len(atts) == 0 {
		fmt.Fprintf(&hdr, "Content-Type: %s; charset=utf-8\r\n", ctype)
		hdr.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		return append(hdr.Bytes(), body...), nil
	}

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	tp, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {ctype + "; charset=utf-8"},
		"Content-Transfer-Encoding": {"8bit"},
	})
	tp.Write(body)
	for _, a := range atts {
		ah := textproto.MIMEHeader{
			"Content-Type":              {a.ctype},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename=%q", a.name)},
		}
		if a.cid != "" {
			ah.Set("Content-ID", "<"+a.cid+">")
		}
		pw, _ := mw.CreatePart(ah)
		pw.Write([]byte(base64Lines(a.data)))
	}
	mw.Close()

	fmt.Fprintf(&hdr, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mw.Boundary())
	return append(hdr.Bytes(), mp.Bytes()...), nil
}

func pstSubjectOf(m *pst.Message) string {
	if p, ok := m.Properties.(interface{ GetSubject() string }); ok {
		return p.GetSubject()
	}
	return ""
}

// mapiDate prefers the client submit time (sent) and falls back to the delivery
// time. go-pst returns these as Unix-nanosecond values.
func mapiDate(p mapiProps) time.Time {
	if t := p.GetClientSubmitTime(); t != 0 {
		return time.Unix(0, t).UTC()
	}
	if t := p.GetMessageDeliveryTime(); t != 0 {
		return time.Unix(0, t).UTC()
	}
	return time.Time{}
}

// mapiFlags maps PidTagMessageFlags bits: 0x01 MSGFLAG_READ, 0x08 MSGFLAG_UNSENT.
func mapiFlags(p mapiProps) []string {
	fl := p.GetMessageFlags()
	var out []string
	if fl&0x01 != 0 {
		out = append(out, `\Seen`)
	}
	if fl&0x08 != 0 {
		out = append(out, `\Draft`)
	}
	return out
}

// mapiFrom builds a "Name <addr>" From value. The address is the first candidate
// that looks like SMTP; an Exchange DN (/O=.../CN=...) is not usable as an
// address and is skipped.
func mapiFrom(p mapiProps) string {
	name := oneLine(p.GetSenderName())
	addr := firstSMTP(p.GetSmtpAddress(), p.GetSentRepresentingEmailAddress(), p.GetSenderEmailAddress())
	switch {
	case addr != "" && name != "":
		return fmt.Sprintf("%s <%s>", encodeWord(name), addr)
	case addr != "":
		return addr
	case name != "":
		return encodeWord(name)
	default:
		return ""
	}
}

func firstSMTP(cands ...string) string {
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if strings.Contains(c, "@") && !strings.HasPrefix(strings.ToUpper(c), "/O=") {
			return c
		}
	}
	return ""
}

// PST body property tags. go-pst's generated accessors expect PtypString, but
// Outlook often stores these as PtypBinary - so we read the raw property too.
const (
	pidTagBody     = 0x1000
	pidTagBodyHTML = 0x1013
)

// pstBody returns the best available body: HTML, then plaintext, then decoded
// RTF (still RTF markup - a lossy last resort).
func pstBody(p mapiProps, m *pst.Message) (ctype string, body []byte) {
	if h := strings.TrimSpace(p.GetBodyHtml()); h != "" {
		return "text/html", []byte(h)
	}
	if raw := readRawProp(m, pidTagBodyHTML); len(raw) > 0 {
		return "text/html", []byte(decodeMAPIBytes(raw))
	}
	if t := strings.TrimSpace(p.GetBody()); t != "" {
		return "text/plain", []byte(t)
	}
	if raw := readRawProp(m, pidTagBody); len(raw) > 0 {
		return "text/plain", []byte(decodeMAPIBytes(raw))
	}
	if rtf, err := m.GetBodyRTF(); err == nil && strings.TrimSpace(rtf) != "" {
		return "text/plain", []byte(rtf)
	}
	return "text/plain", nil
}

// readRawProp reads a property's bytes regardless of its declared type. A
// PtypString value is decoded from UTF-16LE; anything else is returned as is.
func readRawProp(m *pst.Message, id uint16) []byte {
	pr, err := m.PropertyContext.GetPropertyReader(id, m.LocalDescriptors)
	if err != nil {
		return nil
	}
	n := pr.Size()
	if n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	if _, err := pr.ReadAt(buf, 0); err != nil {
		return nil
	}
	if pr.Property.Type == pst.PropertyTypeString {
		if s, err := pr.DecodeString(buf); err == nil {
			return []byte(s)
		}
	}
	return buf
}

// decodeMAPIBytes turns a raw MAPI binary body into UTF-8 text: honour a
// UTF-16LE BOM, keep valid UTF-8, otherwise assume Windows-1252.
func decodeMAPIBytes(data []byte) string {
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		if s, err := unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder().Bytes(data); err == nil {
			return string(s)
		}
	}
	if utf8.Valid(data) {
		return string(data)
	}
	if s, err := charmap.Windows1252.NewDecoder().Bytes(data); err == nil {
		return string(s)
	}
	return string(data)
}

type pstAttach struct {
	name  string
	ctype string
	cid   string
	data  []byte
}

// pstAttachments collects by-value attachments (AttachMethod 1). Embedded
// messages and OLE objects are skipped. Any error is treated as "no
// attachments" - a best-effort import.
func pstAttachments(m *pst.Message) []pstAttach {
	all, err := m.GetAllAttachments()
	if err != nil {
		return nil
	}
	var out []pstAttach
	for _, a := range all {
		if a == nil || a.GetAttachMethod() != 1 {
			continue
		}
		var buf bytes.Buffer
		if _, err := a.WriteTo(&buf); err != nil || buf.Len() == 0 {
			continue
		}
		out = append(out, pstAttach{
			name:  firstNonEmpty(a.GetAttachLongFilename(), a.GetAttachFilename(), "attachment"),
			ctype: firstNonEmpty(a.GetAttachMimeTag(), "application/octet-stream"),
			cid:   strings.Trim(a.GetAttachContentId(), "<>"),
			data:  buf.Bytes(),
		})
	}
	return out
}

// --- small helpers ---

var wordEncoder = mime.QEncoding

// encodeWord returns an RFC 2047 encoded-word if s has non-ASCII / special
// characters, otherwise s unchanged.
func encodeWord(s string) string {
	s = oneLine(s)
	if s == "" {
		return ""
	}
	return wordEncoder.Encode("utf-8", s)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// nameList turns a MAPI display list ("John Doe; Jane Roe") into a header value.
// These are display names, not addresses - kept only as informational context.
func nameList(s string) string {
	s = oneLine(s)
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ";")
	for i, p := range parts {
		parts[i] = encodeWord(strings.TrimSpace(p))
	}
	return strings.Join(parts, ", ")
}

// base64Lines encodes data as base64 wrapped at 76 columns with CRLF.
func base64Lines(data []byte) string {
	s := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	b.WriteString("\r\n")
	return b.String()
}

// withBlankLine ensures a header block ends with a blank line (CRLFCRLF).
func withBlankLine(h []byte) []byte {
	if bytes.HasSuffix(h, []byte("\r\n\r\n")) {
		return h
	}
	if bytes.HasSuffix(h, []byte("\r\n")) {
		return append(h, '\r', '\n')
	}
	return append(h, '\r', '\n', '\r', '\n')
}

// strippedTransportHeaders returns the raw header block with MIME framing
// headers (MIME-Version, Content-*) removed - the body is rebuilt separately.
// Output lines end with CRLF; there is no trailing blank line.
func strippedTransportHeaders(raw string) []byte {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	var out, cur []string
	keep := true
	flush := func() {
		if len(cur) > 0 && keep {
			out = append(out, cur...)
		}
		cur, keep = nil, true
	}
	for _, ln := range lines {
		if ln == "" {
			break // end of the header block
		}
		if ln[0] == ' ' || ln[0] == '\t' {
			cur = append(cur, ln) // folded continuation
			continue
		}
		flush()
		name := ln
		if i := strings.IndexByte(ln, ':'); i >= 0 {
			name = ln[:i]
		}
		low := strings.ToLower(strings.TrimSpace(name))
		keep = !(low == "mime-version" || strings.HasPrefix(low, "content-"))
		cur = append(cur, ln)
	}
	flush()

	var b strings.Builder
	for _, ln := range out {
		b.WriteString(ln)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// compile-time checks
var (
	_ Backend   = (*pstBackend)(nil)
	_ Endpoint  = (*pstEndpoint)(nil)
	_ mapiProps = (*properties.Message)(nil)
)
