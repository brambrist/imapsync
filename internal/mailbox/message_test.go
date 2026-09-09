package mailbox

import (
	"strings"
	"testing"
	"time"
)

// Subject is a MIME encoded-word that decodes to "Café" (non-ASCII).
const rawHdr = "Message-ID: <ABC.123@corp.ru>\r\n" +
	"Date: Wed, 09 Sep 2026 12:00:00 +0300\r\n" +
	"Subject: =?UTF-8?B?Q2Fmw6k=?=\r\n" +
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
	if f.Subject != "Café" {
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
		t.Errorf("expected mid, got %q", got)
	}
	withHash := Fields{HashHdr: "abcd"}
	if got := Identity(withHash); got != "hash:abcd" {
		t.Errorf("expected the stored hash, got %q", got)
	}
	surrogate := Fields{Subject: "s", From: "a@b", Date: time.Unix(1000, 0)}
	if got := Identity(surrogate); got != "hash:"+SurrogateHash(surrogate) {
		t.Errorf("expected the surrogate hash, got %q", got)
	}
}

func TestSurrogateHashStableAcrossZoneAndMID(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	a := Fields{Date: time.Date(2026, 9, 9, 12, 0, 0, 0, msk), Subject: "Subject", From: "a@b"}
	b := Fields{Date: a.Date.UTC(), Subject: "Subject", From: "a@b"}
	if SurrogateHash(a) != SurrogateHash(b) {
		t.Error("hash depends on the Date timezone")
	}
	// Message-ID appearing later must not change the surrogate hash
	c := a
	c.MessageID = "late@corp.ru"
	if SurrogateHash(a) != SurrogateHash(c) {
		t.Error("surrogate hash changed because Message-ID appeared")
	}
}

func TestInjectHashHeader(t *testing.T) {
	raw := []byte("Subject: hi\r\nFrom: a@b\r\n\r\nbody")
	out := InjectHashHeader(raw, "X-Imapsync-Hash", "cafe")
	if !strings.HasPrefix(string(out), "X-Imapsync-Hash: cafe\r\n") {
		t.Fatalf("header not inserted: %q", out[:40])
	}
	// second insert - no-op
	again := InjectHashHeader(out, "X-Imapsync-Hash", "other")
	if string(again) != string(out) {
		t.Error("the second insert changed the message")
	}
}

func TestHasHeader(t *testing.T) {
	raw := []byte("X-Imapsync-Hash: old\r\nSubject: hi\r\n\r\nX-Imapsync-Hash: not in the block")
	if !HasHeader(raw, "x-imapsync-hash") {
		t.Error("header in the block not found")
	}
	if HasHeader([]byte("Subject: hi\r\n\r\nX-Imapsync-Hash: body"), "X-Imapsync-Hash") {
		t.Error("found a header in the body")
	}
}
