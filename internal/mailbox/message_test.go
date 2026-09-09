package mailbox

import (
	"strings"
	"testing"
	"time"
)

const rawHdr = "Message-ID: <ABC.123@corp.ru>\r\n" +
	"Date: Wed, 09 Sep 2026 12:00:00 +0300\r\n" +
	"Subject: =?UTF-8?B?0J/RgNC40LLQtdGC?=\r\n" +
	"From: \"Ivanov\" <Ivanov@Corp.RU>\r\n" +
	"X-Imapsync-Hash: deadbeef\r\n\r\n"

func TestParseFields(t *testing.T) {
	f, err := ParseFields([]byte(rawHdr), "X-Imapsync-Hash")
	if err != nil {
		t.Fatal(err)
	}
	if f.MessageID != "ABC.123@corp.ru" {
		t.Errorf("MessageID=%q", f.MessageID)
	}
	if f.Subject != "Привет" {
		t.Errorf("Subject=%q", f.Subject)
	}
	if f.From != "ivanov@corp.ru" {
		t.Errorf("From=%q", f.From)
	}
	if f.HashHdr != "deadbeef" {
		t.Errorf("HashHdr=%q", f.HashHdr)
	}
	if f.Date.UTC().Unix() != time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("Date=%v", f.Date)
	}
}

func TestIdentityPrecedence(t *testing.T) {
	withMID := Fields{MessageID: "x@y", HashHdr: "h", Subject: "s"}
	if got := Identity(withMID); got != "mid:x@y" {
		t.Errorf("ожидали mid, got %q", got)
	}
	withHash := Fields{HashHdr: "abcd"}
	if got := Identity(withHash); got != "hash:abcd" {
		t.Errorf("ожидали записанный hash, got %q", got)
	}
	surrogate := Fields{Subject: "s", From: "a@b", Date: time.Unix(1000, 0)}
	if got := Identity(surrogate); got != "hash:"+SurrogateHash(surrogate) {
		t.Errorf("ожидали суррогатный hash, got %q", got)
	}
}

func TestSurrogateHashStableAcrossZoneAndMID(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	a := Fields{Date: time.Date(2026, 9, 9, 12, 0, 0, 0, msk), Subject: "Тема", From: "a@b"}
	b := Fields{Date: a.Date.UTC(), Subject: "Тема", From: "a@b"}
	if SurrogateHash(a) != SurrogateHash(b) {
		t.Error("хеш зависит от таймзоны Date")
	}
	// появление Message-ID позже не должно менять суррогатный хеш
	c := a
	c.MessageID = "late@corp.ru"
	if SurrogateHash(a) != SurrogateHash(c) {
		t.Error("суррогатный хеш изменился из-за появления Message-ID")
	}
}

func TestInjectHashHeader(t *testing.T) {
	raw := []byte("Subject: hi\r\nFrom: a@b\r\n\r\nbody")
	out := InjectHashHeader(raw, "X-Imapsync-Hash", "cafe")
	if !strings.HasPrefix(string(out), "X-Imapsync-Hash: cafe\r\n") {
		t.Fatalf("заголовок не вставлен: %q", out[:40])
	}
	// повторная вставка - no-op
	again := InjectHashHeader(out, "X-Imapsync-Hash", "other")
	if string(again) != string(out) {
		t.Error("повторная вставка изменила письмо")
	}
}

func TestHasHeader(t *testing.T) {
	raw := []byte("X-Imapsync-Hash: old\r\nSubject: hi\r\n\r\nX-Imapsync-Hash: не в теле")
	if !HasHeader(raw, "x-imapsync-hash") {
		t.Error("заголовок в блоке не найден")
	}
	if HasHeader([]byte("Subject: hi\r\n\r\nX-Imapsync-Hash: body"), "X-Imapsync-Hash") {
		t.Error("нашёл заголовок в теле")
	}
}
