// Package dedup строит индекс писем папки и вычисляет, каких писем не хватает
// на каждой из сторон (дельту для копирования).
package dedup

import (
	"fmt"
	"time"

	"imapsync/internal/mailbox"
)

// Entry - письмо в индексе папки: метаданные для последующего FETCH тела и
// APPEND на другую сторону, плюс ключи сопоставления.
type Entry struct {
	Uid          uint32
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Fields       mailbox.Fields // может быть нулевым, если письмо поднято из кэша
	MsgID        string         // нормализованный Message-ID ("" если отсутствует)
	Surrogate    string         // суррогатный хеш (для записи в X-Imapsync-Hash при копировании)
	Keys         []string       // все ключи сопоставления (см. mailbox.MatchKeys)
}

// Input - разобранное письмо для BuildFrom: подходит как для свежего FETCH, так
// и для строки кэша состояния.
type Input struct {
	Uid          uint32
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Fields       mailbox.Fields
	MsgID        string
	Surrogate    string
	Keys         []string
}

// Index - индекс писем одной папки.
type Index struct {
	byKey   map[string]*Entry // каждый ключ письма -> письмо
	entries []*Entry          // все письма в порядке прихода
	dups    int               // писем, чей ключ уже был в индексе (внутренние дубли)
}

// Len - число уникальных писем в индексе.
func (idx *Index) Len() int { return len(idx.entries) }

// Dups - число писем, оказавшихся внутренними дублями при построении индекса.
func (idx *Index) Dups() int { return idx.dups }

// has сообщает, есть ли в индексе письмо хотя бы по одному из ключей.
func (idx *Index) has(keys []string) bool {
	for _, k := range keys {
		if _, ok := idx.byKey[k]; ok {
			return true
		}
	}
	return false
}

// Build создаёт индекс из писем, полученных mailbox.Client.FetchHeaders:
// разбирает заголовки и делегирует в BuildFrom.
// hashHeader - имя заголовка с суррогатным хешем (из конфига).
func Build(msgs []mailbox.FetchedMessage, hashHeader string) (*Index, []error) {
	inputs := make([]Input, 0, len(msgs))
	var errs []error
	for _, m := range msgs {
		f, err := mailbox.ParseFields(m.Header, hashHeader)
		if err != nil {
			errs = append(errs, fmt.Errorf("письмо uid=%d: %w", m.Uid, err))
			continue
		}
		sur := mailbox.SurrogateHash(f)
		inputs = append(inputs, Input{
			Uid:          m.Uid,
			Flags:        m.Flags,
			InternalDate: m.InternalDate,
			Size:         m.Size,
			Fields:       f,
			MsgID:        f.MessageID,
			Surrogate:    sur,
			Keys:         mailbox.MatchKeysFrom(f.MessageID, f.HashHdr, sur),
		})
	}
	return BuildFrom(inputs), errs
}

// BuildFrom строит индекс из уже разобранных писем. Ключи в Input.Keys должны
// быть заполнены (см. mailbox.MatchKeys / MatchKeysFrom).
func BuildFrom(inputs []Input) *Index {
	idx := &Index{byKey: make(map[string]*Entry, len(inputs)), entries: make([]*Entry, 0, len(inputs))}
	for _, in := range inputs {
		if idx.has(in.Keys) {
			idx.dups++
			continue
		}
		e := &Entry{
			Uid:          in.Uid,
			Flags:        in.Flags,
			InternalDate: in.InternalDate,
			Size:         in.Size,
			Fields:       in.Fields,
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

// Missing возвращает письма из src, которых нет в dst (по любому из ключей).
func Missing(src, dst *Index) []*Entry {
	var out []*Entry
	for _, e := range src.entries {
		if !dst.has(e.Keys) {
			out = append(out, e)
		}
	}
	return out
}

// Delta вычисляет обе дельты сразу: чего не хватает на стороне B (надо лить A->B)
// и чего не хватает на стороне A (надо лить B->A).
func Delta(a, b *Index) (missingOnB, missingOnA []*Entry) {
	return Missing(a, b), Missing(b, a)
}
