// Package syncer - синхронизация одного пользователя: подключение к обоим
// серверам, построение индексов по каждой паре папок, вычисление дельты и
// дописывание недостающих писем в обе стороны (только append).
package syncer

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/emersion/go-imap"

	"imapsync/config"
	"imapsync/internal/dedup"
	"imapsync/internal/mailbox"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

// Syncer держит общую конфигурацию и коллектор статистики.
type Syncer struct {
	cfg   *config.Config
	stats *stats.Collector
	logf  stats.Logf
	state *store.Store // != nil => инкрементальная сверка через кэш
}

// New создаёт синкер с полной сверкой папок каждый цикл.
func New(cfg *config.Config, coll *stats.Collector, logf stats.Logf) *Syncer {
	return NewWithState(cfg, coll, logf, nil)
}

// NewWithState создаёт синкер; если state != nil, включается инкрементальная
// сверка (UID SEARCH + кэш разбора).
func NewWithState(cfg *config.Config, coll *stats.Collector, logf stats.Logf, st *store.Store) *Syncer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Syncer{cfg: cfg, stats: coll, logf: logf, state: st}
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
		s.persistStatus(u.Name, us)
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

	idxA, okA := s.indexFolder(us, ca, "a", u, fp, fp.A, stA)
	idxB, okB := s.indexFolder(us, cb, "b", u, fp, fp.B, stB)
	if !okA || !okB {
		return
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

// indexFolder строит индекс писем одной стороны папки. side - "a" | "b".
// Возвращает (индекс, true) при успехе; (nil, false) если сверку по этой паре
// надо пропустить (ошибка уже залогирована).
func (s *Syncer) indexFolder(us *stats.UserStats, cl *mailbox.Client, side string, u config.User, fp config.FolderPair, folder string, st *imap.MailboxStatus) (*dedup.Index, bool) {
	if s.cfg.StateCache && s.state != nil {
		return s.indexFolderIncremental(us, cl, side, u, fp, folder, st)
	}
	return s.indexFolderFull(us, cl, side, u, folder, st)
}

// indexFolderFull забирает и разбирает заголовки всех писем папки.
func (s *Syncer) indexFolderFull(us *stats.UserStats, cl *mailbox.Client, side string, u config.User, folder string, st *imap.MailboxStatus) (*dedup.Index, bool) {
	msgs, err := cl.FetchHeaders(st.Messages, s.cfg.FetchBatchSize)
	if err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s, папка %s %q: %w", u.Name, side, folder, err))
		return nil, false
	}
	idx, errs := dedup.Build(msgs, s.cfg.HashHeader)
	for _, e := range errs {
		s.recordErr(us, fmt.Errorf("юзер %s, папка %s %q: разбор: %w", u.Name, side, folder, e))
	}
	return idx, true
}

// indexFolderIncremental берёт список UID через UID SEARCH, фетчит заголовки
// только для новых писем, остальное поднимает из кэша sqlite; кэш при этом
// обновляется (новые письма, удалённые UID, UIDVALIDITY).
func (s *Syncer) indexFolderIncremental(us *stats.UserStats, cl *mailbox.Client, side string, u config.User, fp config.FolderPair, folder string, st *imap.MailboxStatus) (*dedup.Index, bool) {
	pair := store.PairKey(fp.A, fp.B)
	ctxErr := func(err error) (*dedup.Index, bool) {
		s.recordErr(us, fmt.Errorf("юзер %s, папка %s %q: %w", u.Name, side, folder, err))
		return nil, false
	}

	savedUIDV, cached, err := s.state.LoadEndpoint(u.Name, pair, side)
	if err != nil {
		return ctxErr(err)
	}
	if savedUIDV != 0 && savedUIDV != st.UidValidity {
		s.logf("юзер %s, папка %s %q: UIDVALIDITY изменился (%d -> %d), полный пере-скан", u.Name, side, folder, savedUIDV, st.UidValidity)
		if err := s.state.ResetEndpoint(u.Name, pair, side); err != nil {
			return ctxErr(fmt.Errorf("сброс кэша: %w", err))
		}
		cached = map[uint32]store.CachedMsg{}
	}

	curUIDs, err := cl.UIDSearchAll()
	if err != nil {
		return ctxErr(err)
	}
	slices.Sort(curUIDs)

	curSet := make(map[uint32]struct{}, len(curUIDs))
	var newUIDs []uint32
	for _, uid := range curUIDs {
		curSet[uid] = struct{}{}
		if _, ok := cached[uid]; !ok {
			newUIDs = append(newUIDs, uid)
		}
	}
	var goneUIDs []uint32
	for uid := range cached {
		if _, ok := curSet[uid]; !ok {
			goneUIDs = append(goneUIDs, uid)
		}
	}

	fetched, err := cl.FetchHeadersByUID(newUIDs, s.cfg.FetchBatchSize)
	if err != nil {
		return ctxErr(err)
	}

	fresh := make([]store.CachedMsg, 0, len(fetched))
	for _, m := range fetched {
		f, perr := mailbox.ParseFields(m.Header, s.cfg.HashHeader)
		if perr != nil {
			s.recordErr(us, fmt.Errorf("юзер %s, папка %s %q: разбор uid=%d: %w", u.Name, side, folder, m.Uid, perr))
			continue
		}
		cm := store.CachedMsg{
			Uid: m.Uid, MsgID: f.MessageID, XHash: f.HashHdr,
			Surrogate: mailbox.SurrogateHash(f), InternalDate: m.InternalDate, Flags: m.Flags,
		}
		fresh = append(fresh, cm)
		cached[m.Uid] = cm
	}

	// Обновляем кэш. Ошибки записи не фатальны для сверки в этом цикле - просто
	// в следующем цикле часть писем перечитается заново.
	if err := s.state.PutCachedMsgs(u.Name, pair, side, fresh); err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s, папка %s %q: запись кэша: %w", u.Name, side, folder, err))
	}
	if len(goneUIDs) > 0 {
		if err := s.state.DeleteCachedMsgs(u.Name, pair, side, goneUIDs); err != nil {
			s.recordErr(us, fmt.Errorf("юзер %s, папка %s %q: чистка кэша: %w", u.Name, side, folder, err))
		}
		for _, uid := range goneUIDs {
			delete(cached, uid)
		}
	}
	if err := s.state.SaveEndpoint(u.Name, pair, side, st.UidValidity); err != nil {
		s.recordErr(us, fmt.Errorf("юзер %s, папка %s %q: запись эндпоинта: %w", u.Name, side, folder, err))
	}

	inputs := make([]dedup.Input, 0, len(curUIDs))
	for _, uid := range curUIDs {
		cm, ok := cached[uid]
		if !ok {
			continue // разбор нового письма упал
		}
		inputs = append(inputs, dedup.Input{
			Uid:          cm.Uid,
			Flags:        cm.Flags,
			InternalDate: cm.InternalDate,
			Surrogate:    cm.Surrogate,
			Keys:         mailbox.MatchKeysFrom(cm.MsgID, cm.XHash, cm.Surrogate),
		})
	}
	if len(newUIDs) > 0 || len(goneUIDs) > 0 {
		s.logf("юзер %s, папка %s %q: инкрементально - новых %d, удалено %d, всего %d",
			u.Name, side, folder, len(newUIDs), len(goneUIDs), len(inputs))
	}
	return dedup.BuildFrom(inputs), true
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
	raw = mailbox.InjectHashHeader(raw, s.cfg.HashHeader, e.Surrogate)

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

// persistStatus сохраняет итог прохода по юзеру в таблицу user_status (если
// открыт store). Позволяет видеть по БД, когда и с каким результатом
// отработал синк по каждому пользователю.
func (s *Syncer) persistStatus(name string, us *stats.UserStats) {
	if s.state == nil {
		return
	}
	r := us.Report()
	now := time.Now()
	st := store.UserStatus{
		LastRun:    now,
		Status:     "ok",
		CopiedAToB: r.CopiedAToB,
		CopiedBToA: r.CopiedBToA,
		SkippedDup: r.SkippedDup,
		Errors:     r.Errors,
		LastError:  r.LastErr,
	}
	if r.Errors > 0 {
		st.Status = "error"
	} else {
		st.LastOK = now
	}
	if err := s.state.SaveUserStatus(name, st); err != nil {
		s.logf("юзер %s: не удалось записать статус в БД: %v", name, err)
	}
}
