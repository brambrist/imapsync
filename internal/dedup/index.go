// Package dedup builds an index of a folder's messages and computes which
// messages are missing on each side (the delta to copy).
package dedup

import (
	"fmt"
	"time"

	"imapsync/internal/endpoint"
	"imapsync/internal/mailbox"
)

// Entry is a message in a folder index: metadata for the later Open of the body
// and Append to the other side, plus the match keys.
type Entry struct {
	ID           string // opaque message ID on its own side
	Flags        []string
	InternalDate time.Time
	Size         uint32
	MsgID        string   // normalized Message-ID ("" if absent)
	Surrogate    string   // surrogate hash (written into X-Imapsync-Hash on copy)
	Keys         []string // all match keys (see mailbox.MatchKeys)
}

// Input is a parsed message for BuildFrom: works both for a fresh FETCH and for
// a state-cache row.
type Input struct {
	ID           string
	Flags        []string
	InternalDate time.Time
	Size         uint32
	MsgID        string // normalized Message-ID
	XHash        string // X-Imapsync-Hash value if we tagged this message
	Surrogate    string // computed surrogate hash
	Keys         []string
}

// Index is an index of one folder's messages.
type Index struct {
	byKey   map[string]*Entry // every message key -> message
	entries []*Entry          // all messages in arrival order
	dups    int               // messages whose key was already in the index (internal dups)
}

// Len is the number of unique messages in the index.
func (idx *Index) Len() int { return len(idx.entries) }

// Dups is the number of messages that turned out to be internal duplicates
// while building the index.
func (idx *Index) Dups() int { return idx.dups }

// has reports whether the index already holds a message under any of the keys.
func (idx *Index) has(keys []string) bool {
	for _, k := range keys {
		if _, ok := idx.byKey[k]; ok {
			return true
		}
	}
	return false
}

// Build creates an index from message metadata: parses the headers and delegates
// to BuildFrom. hashHeader is the surrogate-hash header name (from config).
func Build(msgs []endpoint.Message, hashHeader string) (*Index, []error) {
	inputs := make([]Input, 0, len(msgs))
	var errs []error
	for _, m := range msgs {
		in, err := ParseMessage(m, hashHeader)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		inputs = append(inputs, in)
	}
	return BuildFrom(inputs), errs
}

// ParseMessage parses one message's headers into an Input.
func ParseMessage(m endpoint.Message, hashHeader string) (Input, error) {
	f, err := mailbox.ParseFields(m.Header, hashHeader)
	if err != nil {
		return Input{}, fmt.Errorf("message id=%s: %w", m.ID, err)
	}
	sur := mailbox.SurrogateHash(f)
	return Input{
		ID:           m.ID,
		Flags:        m.Flags,
		InternalDate: m.InternalDate,
		Size:         m.Size,
		MsgID:        f.MessageID,
		XHash:        f.HashHdr,
		Surrogate:    sur,
		Keys:         mailbox.MatchKeysFrom(f.MessageID, f.HashHdr, sur),
	}, nil
}

// Keys rebuilds the match keys from components (for a cache row).
func Keys(msgID, xhash, surrogate string) []string {
	return mailbox.MatchKeysFrom(msgID, xhash, surrogate)
}

// BuildFrom builds an index from already-parsed messages. Input.Keys must be
// filled in (see mailbox.MatchKeys / MatchKeysFrom).
func BuildFrom(inputs []Input) *Index {
	idx := &Index{byKey: make(map[string]*Entry, len(inputs)), entries: make([]*Entry, 0, len(inputs))}
	for _, in := range inputs {
		if idx.has(in.Keys) {
			idx.dups++
			continue
		}
		e := &Entry{
			ID:           in.ID,
			Flags:        in.Flags,
			InternalDate: in.InternalDate,
			Size:         in.Size,
			MsgID:        in.MsgID,
			Surrogate:    in.Surrogate,
			Keys:         in.Keys,
		}
		idx.entries = append(idx.entries, e)
		for _, k := range e.Keys {
			idx.byKey[k] = e
		}
	}
	return idx
}

// Missing returns the messages from src that are not in dst (by any key).
func Missing(src, dst *Index) []*Entry {
	var out []*Entry
	for _, e := range src.entries {
		if !dst.has(e.Keys) {
			out = append(out, e)
		}
	}
	return out
}

// Delta computes both deltas at once: what is missing on side B (needs A->B) and
// what is missing on side A (needs B->A).
func Delta(a, b *Index) (missingOnB, missingOnA []*Entry) {
	return Missing(a, b), Missing(b, a)
}
