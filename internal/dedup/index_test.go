package dedup

import (
	"fmt"
	"strings"
	"testing"

	"imapsync/internal/endpoint"
)

const hashHdr = "X-Imapsync-Hash"

// hdr assembles a raw header block from key-value pairs.
func hdr(kv ...string) []byte {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "%s: %s\r\n", kv[i], kv[i+1])
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

func msg(id string, raw []byte) endpoint.Message {
	return endpoint.Message{ID: id, Header: raw}
}

func TestDeltaBasic(t *testing.T) {
	m1 := hdr("Message-ID", "<1@c>", "Subject", "one", "From", "a@c", "Date", "Wed, 09 Sep 2026 12:00:00 +0000")
	m2 := hdr("Message-ID", "<2@c>", "Subject", "two", "From", "a@c", "Date", "Wed, 09 Sep 2026 12:01:00 +0000")
	m3 := hdr("Message-ID", "<3@c>", "Subject", "three", "From", "a@c", "Date", "Wed, 09 Sep 2026 12:02:00 +0000")

	a, errs := Build([]endpoint.Message{msg("11", m1), msg("12", m2)}, hashHdr)
	if len(errs) != 0 {
		t.Fatalf("errs A: %v", errs)
	}
	b, errs := Build([]endpoint.Message{msg("22", m2), msg("23", m3)}, hashHdr)
	if len(errs) != 0 {
		t.Fatalf("errs B: %v", errs)
	}

	onB, onA := Delta(a, b)
	if len(onB) != 1 || onB[0].ID != "11" {
		t.Errorf("missingOnB = %+v, expected id=11", onB)
	}
	if len(onA) != 1 || onA[0].ID != "23" {
		t.Errorf("missingOnA = %+v, expected id=23", onA)
	}
}

// Message-ID appeared on A later; B holds our copy with X-Imapsync-Hash.
// It must not be duplicated.
func TestLateMessageIDNoDuplicate(t *testing.T) {
	subj, from, date := "quarterly report", "boss@c", "Wed, 09 Sep 2026 12:00:00 +0000"

	// side A: the message has gained a Message-ID
	aRaw := hdr("Message-ID", "<late@c>", "Subject", subj, "From", from, "Date", date)
	aIdx, _ := Build([]endpoint.Message{msg("1", aRaw)}, hashHdr)

	// the surrogate we wrote into the copy on B on the previous pass
	sur := aIdx.entries[0].Surrogate

	bRaw := hdr("Subject", subj, "From", from, "Date", date, hashHdr, sur)
	bIdx, _ := Build([]endpoint.Message{msg("2", bRaw)}, hashHdr)

	onB, onA := Delta(aIdx, bIdx)
	if len(onB) != 0 {
		t.Errorf("message would be duplicated on B: %+v", onB)
	}
	if len(onA) != 0 {
		t.Errorf("message would be duplicated on A: %+v", onA)
	}
}

func TestInternalDupsCounted(t *testing.T) {
	m := hdr("Message-ID", "<x@c>", "Subject", "s", "From", "a@c", "Date", "Wed, 09 Sep 2026 12:00:00 +0000")
	idx, _ := Build([]endpoint.Message{msg("1", m), msg("2", m), msg("3", m)}, hashHdr)
	if idx.Len() != 1 {
		t.Errorf("Len = %d, expected 1", idx.Len())
	}
	if idx.Dups() != 2 {
		t.Errorf("Dups = %d, expected 2", idx.Dups())
	}
}

func TestNoMessageIDMatchesBySurrogate(t *testing.T) {
	subj, from, date := "no id here", "x@c", "Wed, 09 Sep 2026 12:00:00 +0000"
	raw := hdr("Subject", subj, "From", from, "Date", date)
	a, _ := Build([]endpoint.Message{msg("1", raw)}, hashHdr)
	b, _ := Build([]endpoint.Message{msg("2", raw)}, hashHdr)
	onB, onA := Delta(a, b)
	if len(onB) != 0 || len(onA) != 0 {
		t.Errorf("messages without Message-ID did not match by surrogate: onB=%v onA=%v", onB, onA)
	}
}
