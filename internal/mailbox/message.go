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
	_ "github.com/emersion/go-message/charset" // side-effect: регистрирует декодеры кодировок
)

// Fields - значимые поля письма для дедупликации.
type Fields struct {
	MessageID string    // нормализованный Message-ID (без <>), "" если отсутствует
	Date      time.Time // дата отправки, нулевая если не разобрана
	Subject   string    // декодированная тема
	From      string    // нормализованный адрес отправителя
	HashHdr   string    // значение X-Imapsync-Hash, если письмо уже помечалось нами
}

// ParseFields разбирает сырой блок заголовков (RFC 822) в Fields.
// hashHeader - имя кастомного заголовка с суррогатным хешем (из конфига).
func ParseFields(rawHeader []byte, hashHeader string) (Fields, error) {
	// net/mail ждёт заголовки, пустую строку и тело; тело может быть пустым.
	buf := rawHeader
	if !bytes.HasSuffix(buf, []byte("\n\n")) && !bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
		buf = append(append([]byte{}, buf...), '\r', '\n')
	}
	msg, err := mail.ReadMessage(bytes.NewReader(buf))
	if err != nil {
		return Fields{}, fmt.Errorf("разбор заголовков письма: %w", err)
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

// decodeHeader декодирует MIME encoded-words (=?UTF-8?B?...?=), включая
// нестандартные кодировки (через go-message/charset). При ошибке возвращает
// исходную строку.
func decodeHeader(s string) string {
	dec := mime.WordDecoder{CharsetReader: gomessage.CharsetReader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// NormalizeMessageID тримит пробелы и снимает угловые скобки.
func NormalizeMessageID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// NormalizeAddr приводит From к нижнему регистру и вытаскивает голый адрес,
// если он распарсился; иначе тримит и опускает регистр всей строки.
func NormalizeAddr(s string) string {
	s = strings.TrimSpace(decodeHeader(s))
	if addr, err := mail.ParseAddress(s); err == nil {
		return strings.ToLower(strings.TrimSpace(addr.Address))
	}
	return strings.ToLower(s)
}

// SurrogateHash вычисляет стабильный суррогатный ключ письма по Date+Subject+From.
// Date приводится к UTC unix-времени; нулевая дата даёт 0.
func SurrogateHash(f Fields) string {
	var unix int64
	if !f.Date.IsZero() {
		unix = f.Date.UTC().Unix()
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s", unix, f.Subject, f.From)
	return hex.EncodeToString(h.Sum(nil))
}

// Identity возвращает итоговый ключ письма для сравнения между папками:
// Message-ID -> ранее записанный X-Imapsync-Hash -> вычисленный суррогатный хеш.
func Identity(f Fields) string {
	if f.MessageID != "" {
		return "mid:" + f.MessageID
	}
	if f.HashHdr != "" {
		return "hash:" + f.HashHdr
	}
	return "hash:" + SurrogateHash(f)
}

// MatchKeys возвращает ВСЕ ключи, по которым это письмо может совпасть с копией
// на другой стороне. Письмо считается идентичным другому, если совпадает хотя бы
// одна пара ключей.
//
// Суррогатный хеш присутствует всегда - это защищает от задвоения в сценарии
// Exchange, когда Message-ID появился на одной стороне позже: там письмо уже
// имеет "mid:", а его копия на другой стороне - только записанный нами
// "hash:<суррогат>", и именно по нему они сматчатся.
func MatchKeys(f Fields) []string {
	return MatchKeysFrom(f.MessageID, f.HashHdr, SurrogateHash(f))
}

// MatchKeysFrom - то же, что MatchKeys, но принимает уже вычисленный суррогатный
// хеш напрямую (например поднятый из кэша), а не пересчитывает его из полей.
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

// InjectHashHeader вставляет заголовок hashHeader: value в начало сырого письма,
// чтобы при следующих проходах находить уже скопированное письмо по нему.
// Если такой заголовок уже есть - письмо возвращается без изменений.
func InjectHashHeader(raw []byte, hashHeader, value string) []byte {
	prefixLC := strings.ToLower(hashHeader) + ":"
	// проверяем только блок заголовков (до первой пустой строки)
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		headerEnd = len(raw)
	}
	for line := range bytes.SplitSeq(raw[:headerEnd], []byte("\r\n")) {
		if strings.HasPrefix(strings.ToLower(string(line)), prefixLC) {
			return raw
		}
	}
	hdr := fmt.Appendf(nil, "%s: %s\r\n", hashHeader, value)
	return append(hdr, raw...)
}
