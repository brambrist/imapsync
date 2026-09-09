// Package syncer - синхронизация одного пользователя: подключение к обоим
// серверам, построение индексов по каждой паре папок, вычисление дельты и
// дописывание недостающих писем в обе стороны (только append).
package syncer

import (
	"context"
	"fmt"
	"time"

	"imapsync/config"
	"imapsync/internal/dedup"
	"imapsync/internal/mailbox"
	"imapsync/internal/stats"
)

// Syncer держит общую конфигурацию и коллектор статистики.
type Syncer struct {
	cfg   *config.Config
	stats *stats.Collector
	logf  stats.Logf
}

// New создаёт синкер.
func New(cfg *config.Config, coll *stats.Collector, logf stats.Logf) *Syncer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Syncer{cfg: cfg, stats: coll, logf: logf}
}

// флаги, которые имеет смысл переносить при копировании письма.
// \Recent проставить нельзя, \Deleted не переносим (удаления не синхронизируем).
var copyableFlags = map[string]bool{
	`\Seen`:     true,
	`\Answered`: true,
	`\Flagged`:  true,
	`\Draft`:    true,
}

// SyncUser обрабатывает пользователя целиком. Ошибки не пробрасываются наружу -
// они логируются с контекстом и попадают в статистику юзера; прерывание по
// ctx фиксируется как ошибка.
func (s *Syncer) SyncUser(ctx context.Context, u config.User) {
	us := s.stats.BeginUser(u.Name)
	defer func() {
		s.stats.EndUser(us)
		s.stats.LogUser(us, s.logf)
	}()

	dial := s.cfg.DialTimeout.Std()

	ca, err := mailbox.Connect(s.cfg.ServerA, u.UserA, dial, s.cfg.InsecureTLS)
	if err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s: %w", u.Name, err))
		return
	}
	defer ca.Logout()

	cb, err := mailbox.Connect(s.cfg.ServerB, u.UserB, dial, s.cfg.InsecureTLS)
	if err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s: %w", u.Name, err))
		return
	}
	defer cb.Logout()

	for _, fp := range s.cfg.Folders {
		if err := ctx.Err(); err != nil {
			s.recordErr(us, fmt.Errorf("юзер %s: обработка прервана: %w", u.Name, err))
			return
		}
		s.syncFolderPair(ctx, u, us, ca, cb, fp)
	}
}

// syncFolderPair синхронизирует одну пару папок в обе стороны.
func (s *Syncer) syncFolderPair(ctx context.Context, u config.User, us *stats.UserStats, ca, cb *mailbox.Client, fp config.FolderPair) {
	stA, err := ca.Select(fp.A)
	if err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s, папка A %q: %w", u.Name, fp.A, err))
		return
	}
	stB, err := cb.Select(fp.B)
	if err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s, папка B %q: %w", u.Name, fp.B, err))
		return
	}

	batch := s.cfg.FetchBatchSize
	hdr := s.cfg.HashHeader

	msgsA, err := ca.FetchHeaders(stA.Messages, batch)
	if err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s, папка A %q: %w", u.Name, fp.A, err))
		return
	}
	msgsB, err := cb.FetchHeaders(stB.Messages, batch)
	if err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s, папка B %q: %w", u.Name, fp.B, err))
		return
	}

	idxA, errsA := dedup.Build(msgsA, hdr)
	idxB, errsB := dedup.Build(msgsB, hdr)
	for _, e := range errsA {
		s.recordErr(us, fmt.Errorf("юзер %s, папка A %q: разбор: %w", u.Name, fp.A, e))
	}
	for _, e := range errsB {
		s.recordErr(us, fmt.Errorf("юзер %s, папка B %q: разбор: %w", u.Name, fp.B, e))
	}

	// Обе дельты считаем ДО каких-либо append, чтобы только что скопированное
	// письмо не поехало обратно в том же цикле.
	missingOnB, missingOnA := dedup.Delta(idxA, idxB)

	// уже присутствующие на другой стороне (не копируем) - в дубли.
	skipped := (idxA.Len() - len(missingOnB)) + (idxB.Len() - len(missingOnA))
	if skipped > 0 {
		us.IncSkippedDup(skipped)
	}

	copiedAB := s.copyMissing(ctx, us, ca, cb, fp.B, missingOnB, "A->B", u.Name, fp.A)
	us.IncCopiedAToB(copiedAB)

	copiedBA := s.copyMissing(ctx, us, cb, ca, fp.A, missingOnA, "B->A", u.Name, fp.B)
	us.IncCopiedBToA(copiedBA)
}

// copyMissing копирует письма entries из src в папку dstFolder на dst.
// Возвращает число успешно скопированных.
func (s *Syncer) copyMissing(ctx context.Context, us *stats.UserStats, src, dst *mailbox.Client, dstFolder string, entries []*dedup.Entry, dir, user, srcFolder string) int {
	n := 0
	for i, e := range entries {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				s.recordErr(us, fmt.Errorf("юзер %s, %s (%q): прервано: %w", user, dir, srcFolder, err))
				return n
			}
		}
		if err := s.copyOne(src, dst, dstFolder, e); err != nil {
			s.recordErr(us, fmt.Errorf("юзер %s, %s (%q -> %q), uid=%d: %w", user, dir, srcFolder, dstFolder, e.Uid, err))
			continue
		}
		n++
	}
	return n
}

// copyOne забирает письмо целиком, проставляет суррогатный хеш в кастомный
// заголовок и дописывает в целевую папку с сохранением флагов и внутренней даты.
func (s *Syncer) copyOne(src, dst *mailbox.Client, dstFolder string, e *dedup.Entry) error {
	raw, err := src.FetchFull(e.Uid)
	if err != nil {
		return err
	}
	// Суррогатный хеш пишем всегда - чтобы на следующих проходах письмо
	// находилось по заголовку даже если Message-ID появится/исчезнет.
	raw = mailbox.InjectHashHeader(raw, s.cfg.HashHeader, mailbox.SurrogateHash(e.Fields))

	date := e.InternalDate
	if date.IsZero() {
		date = time.Time{} // сервер проставит текущую
	}
	return dst.Append(dstFolder, filterFlags(e.Flags), date, raw)
}

// filterFlags оставляет только переносимые флаги.
func filterFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if copyableFlags[f] {
			out = append(out, f)
		}
	}
	return out
}

// recordErr логирует ошибку с контекстом и учитывает её в статистике юзера.
func (s *Syncer) recordErr(us *stats.UserStats, err error) {
	us.AddError(err)
	s.logf("ошибка: %v", err)
}
