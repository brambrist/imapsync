// Package syncer - синхронизация одного пользователя: подключение к обоим
// серверам, построение индексов по каждой паре папок, вычисление дельты и
// дописывание недостающих писем в обе стороны (только append).
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
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
	state *store.Store // != nil => инкрементальная сверка через кэш и запись user_status
}

// New создаёт синкер с полной сверкой папок каждый цикл.
func New(cfg *config.Config, coll *stats.Collector, logf stats.Logf) *Syncer {
	return NewWithState(cfg, coll, logf, nil)
}

// NewWithState создаёт синкер; если state != nil, включается инкрементальная
// сверка (при cfg.StateCache) и запись статуса юзеров.
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

// incremental - true, если для этого синкера включён режим кэша состояния.
func (s *Syncer) incremental() bool { return s.cfg.StateCache && s.state != nil }

// userStopped - достиг ли юзер лимита ошибок подряд (max_fail_streak).
// Если да - логируем и пропускаем его в этом цикле.
func (s *Syncer) userStopped(name string) bool {
	if s.state == nil || s.cfg.MaxFailStreak <= 0 {
		return false
	}
	st, ok, err := s.state.UserStatusOf(name)
	if err != nil {
		s.logf("юзер %s: не удалось проверить серию ошибок: %v", name, err)
		return false
	}
	if ok && st.FailStreak >= int64(s.cfg.MaxFailStreak) {
		s.logf("юзер %s: синк остановлен - %d ошибок подряд (с %s), сброс: imapsync db-resume-user -name %s",
			name, st.FailStreak, st.FailSince.Format("2006-01-02 15:04:05"), name)
		return true
	}
	return false
}

// SyncUser обрабатывает пользователя целиком. Ошибки не пробрасываются наружу -
// они логируются с контекстом и попадают в статистику юзера; прерывание по
// ctx фиксируется как ошибка.
func (s *Syncer) SyncUser(ctx context.Context, u config.User) {
	if s.userStopped(u.Name) {
		return
	}

	us := s.stats.BeginUser(u.Name)
	defer func() {
		s.stats.EndUser(us)
		s.stats.LogUser(us, s.logf)
		s.persistStatus(u.Name, us)
	}()

	sess := &session{s: s, ctx: ctx, u: u, us: us}
	if !sess.connect() {
		return
	}
	defer sess.close()

	for _, fp := range s.cfg.Folders {
		if err := ctx.Err(); err != nil {
			s.recordErr(us, fmt.Errorf("юзер %s: обработка прервана: %w", u.Name, err))
			return
		}
		err := sess.syncPair(fp)
		if err == nil {
			continue
		}
		s.recordErr(us, err)
		if isConnErr(err) {
			s.logf("юзер %s: соединение потеряно, переподключение", u.Name)
			if !sess.reconnect() {
				return
			}
			if err := sess.syncPair(fp); err != nil {
				s.recordErr(us, err)
			}
		}
	}
}

// session - состояние обработки одного юзера: оба соединения, контекст, счётчики.
type session struct {
	s   *Syncer
	ctx context.Context
	u   config.User
	us  *stats.UserStats
	a   *mailbox.Client
	b   *mailbox.Client
}

func (sess *session) connect() bool {
	a, err := sess.s.dial(sess.ctx, sess.s.cfg.ServerA, sess.u.UserA)
	if err != nil {
		sess.s.recordErr(sess.us, fmt.Errorf("юзер %s: %w", sess.u.Name, err))
		return false
	}
	b, err := sess.s.dial(sess.ctx, sess.s.cfg.ServerB, sess.u.UserB)
	if err != nil {
		a.Logout()
		sess.s.recordErr(sess.us, fmt.Errorf("юзер %s: %w", sess.u.Name, err))
		return false
	}
	sess.a, sess.b = a, b
	return true
}

func (sess *session) reconnect() bool {
	sess.close()
	return sess.connect()
}

func (sess *session) close() {
	if sess.a != nil {
		sess.a.Logout()
		sess.a = nil
	}
	if sess.b != nil {
		sess.b.Logout()
		sess.b = nil
	}
}

// dial подключается к серверу с повторами при транзиентных ошибках.
func (s *Syncer) dial(ctx context.Context, srv config.Server, user string) (*mailbox.Client, error) {
	attempts := max(1, s.cfg.ConnectRetries+1)
	var lastErr error
	for i := range attempts {
		if i > 0 {
			backoff := s.cfg.RetryBackoff.Std() * (1 << (i - 1))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			s.logf("повтор подключения к %s (юзер %s), попытка %d/%d", srv.Addr(), user, i+1, attempts)
		}
		cl, err := mailbox.Connect(ctx, srv, user, s.cfg.DialTimeout.Std(), s.cfg.IOTimeout.Std(), s.cfg.InsecureTLS)
		if err == nil {
			return cl, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// syncPair синхронизирует одну пару папок в обе стороны. Возвращает ошибку
// уровня папки (Resolve/Select/Fetch/UID SEARCH); ошибки отдельных писем
// логируются внутри и не возвращаются.
func (sess *session) syncPair(fp config.FolderPair) error {
	s, u, us := sess.s, sess.u, sess.us

	folderA, err := sess.a.ResolveFolder(fp.A)
	if err != nil {
		return fmt.Errorf("юзер %s, папка A %q: %w", u.Name, fp.A, err)
	}
	folderB, err := sess.b.ResolveFolder(fp.B)
	if err != nil {
		return fmt.Errorf("юзер %s, папка B %q: %w", u.Name, fp.B, err)
	}

	stA, err := sess.a.Select(folderA)
	if err != nil {
		return fmt.Errorf("юзер %s, папка A %q: %w", u.Name, folderA, err)
	}
	stB, err := sess.b.Select(folderB)
	if err != nil {
		return fmt.Errorf("юзер %s, папка B %q: %w", u.Name, folderB, err)
	}

	idxA, err := s.indexFolder(sess, "a", fp, folderA, stA)
	if err != nil {
		return err
	}
	idxB, err := s.indexFolder(sess, "b", fp, folderB, stB)
	if err != nil {
		return err
	}

	// Обе дельты считаем ДО каких-либо append, чтобы только что скопированное
	// письмо не поехало обратно в том же цикле.
	missingOnB, missingOnA := dedup.Delta(idxA, idxB)

	skipped := (idxA.Len() - len(missingOnB)) + (idxB.Len() - len(missingOnA))
	if skipped > 0 {
		us.IncSkippedDup(skipped)
	}

	pair := store.PairKey(fp.A, fp.B)

	copiedAB, cacheB := s.copyMissing(sess, sess.a, sess.b, folderB, missingOnB, "A->B", folderA)
	us.IncCopiedAToB(copiedAB)

	copiedBA, cacheA := s.copyMissing(sess, sess.b, sess.a, folderA, missingOnA, "B->A", folderB)
	us.IncCopiedBToA(copiedBA)

	// Скопированные письма, для которых сервер вернул APPENDUID, сразу кладём в
	// кэш - чтобы не перечитывать их заголовки в следующем цикле.
	if s.incremental() {
		if len(cacheB) > 0 {
			if err := s.state.PutCachedMsgs(u.Name, pair, "b", cacheB); err != nil {
				s.recordErr(us, fmt.Errorf("юзер %s, папка B %q: до-запись кэша: %w", u.Name, folderB, err))
			}
		}
		if len(cacheA) > 0 {
			if err := s.state.PutCachedMsgs(u.Name, pair, "a", cacheA); err != nil {
				s.recordErr(us, fmt.Errorf("юзер %s, папка A %q: до-запись кэша: %w", u.Name, folderA, err))
			}
		}
	}
	return nil
}

// indexFolder строит индекс писем одной стороны папки. side - "a" | "b".
func (s *Syncer) indexFolder(sess *session, side string, fp config.FolderPair, folder string, st *imap.MailboxStatus) (*dedup.Index, error) {
	cl := sess.a
	if side == "b" {
		cl = sess.b
	}
	if s.incremental() {
		return s.indexFolderIncremental(sess, cl, side, fp, folder, st)
	}
	return s.indexFolderFull(sess, cl, side, folder, st)
}

// indexFolderFull забирает и разбирает заголовки всех писем папки.
func (s *Syncer) indexFolderFull(sess *session, cl *mailbox.Client, side, folder string, st *imap.MailboxStatus) (*dedup.Index, error) {
	msgs, err := cl.FetchHeaders(st.Messages, s.cfg.FetchBatchSize)
	if err != nil {
		return nil, fmt.Errorf("юзер %s, папка %s %q: %w", sess.u.Name, side, folder, err)
	}
	idx, errs := dedup.Build(msgs, s.cfg.HashHeader)
	for _, e := range errs {
		s.recordErr(sess.us, fmt.Errorf("юзер %s, папка %s %q: разбор: %w", sess.u.Name, side, folder, e))
	}
	return idx, nil
}

// indexFolderIncremental берёт список UID через UID SEARCH, фетчит заголовки
// только для новых писем, остальное поднимает из кэша sqlite; кэш при этом
// обновляется. Периодически (full_resync_every) делает полный пере-скан.
func (s *Syncer) indexFolderIncremental(sess *session, cl *mailbox.Client, side string, fp config.FolderPair, folder string, st *imap.MailboxStatus) (*dedup.Index, error) {
	u, us := sess.u, sess.us
	pair := store.PairKey(fp.A, fp.B)
	wrap := func(err error) error {
		return fmt.Errorf("юзер %s, папка %s %q: %w", u.Name, side, folder, err)
	}

	ep, cached, err := s.state.LoadEndpoint(u.Name, pair, side)
	if err != nil {
		return nil, wrap(err)
	}

	resyncAt := ep.FullResyncAt
	reset := func(reason string) error {
		if len(cached) > 0 {
			s.logf("юзер %s, папка %s %q: %s, полный пере-скан", u.Name, side, folder, reason)
		}
		if err := s.state.ResetEndpoint(u.Name, pair, side); err != nil {
			return wrap(fmt.Errorf("сброс кэша: %w", err))
		}
		cached = map[uint32]store.CachedMsg{}
		resyncAt = time.Now()
		return nil
	}

	switch {
	case ep.Exists && ep.UIDValidity != st.UidValidity:
		if err := reset(fmt.Sprintf("UIDVALIDITY изменился (%d -> %d)", ep.UIDValidity, st.UidValidity)); err != nil {
			return nil, err
		}
	case s.cfg.FullResyncEvery.Std() > 0 && time.Since(ep.FullResyncAt) >= s.cfg.FullResyncEvery.Std():
		if err := reset("плановый полный пере-скан"); err != nil {
			return nil, err
		}
	}

	curUIDs, err := cl.UIDSearchAll()
	if err != nil {
		return nil, wrap(err)
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
		return nil, wrap(err)
	}

	fresh := make([]store.CachedMsg, 0, len(fetched))
	for _, m := range fetched {
		f, perr := mailbox.ParseFields(m.Header, s.cfg.HashHeader)
		if perr != nil {
			s.recordErr(us, wrap(fmt.Errorf("разбор uid=%d: %w", m.Uid, perr)))
			continue
		}
		cm := store.CachedMsg{
			Uid: m.Uid, MsgID: f.MessageID, XHash: f.HashHdr,
			Surrogate: mailbox.SurrogateHash(f), InternalDate: m.InternalDate, Flags: m.Flags,
		}
		fresh = append(fresh, cm)
		cached[m.Uid] = cm
	}

	// Обновляем кэш. Ошибки записи не фатальны - в следующем цикле перечитается.
	if err := s.state.PutCachedMsgs(u.Name, pair, side, fresh); err != nil {
		s.recordErr(us, wrap(fmt.Errorf("запись кэша: %w", err)))
	}
	if len(goneUIDs) > 0 {
		if err := s.state.DeleteCachedMsgs(u.Name, pair, side, goneUIDs); err != nil {
			s.recordErr(us, wrap(fmt.Errorf("чистка кэша: %w", err)))
		}
		for _, uid := range goneUIDs {
			delete(cached, uid)
		}
	}
	if err := s.state.SaveEndpoint(u.Name, pair, side, st.UidValidity, resyncAt); err != nil {
		s.recordErr(us, wrap(fmt.Errorf("запись эндпоинта: %w", err)))
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
			MsgID:        cm.MsgID,
			Surrogate:    cm.Surrogate,
			Keys:         mailbox.MatchKeysFrom(cm.MsgID, cm.XHash, cm.Surrogate),
		})
	}
	if len(newUIDs) > 0 || len(goneUIDs) > 0 {
		s.logf("юзер %s, папка %s %q: инкрементально - новых %d, удалено %d, всего %d",
			u.Name, side, folder, len(newUIDs), len(goneUIDs), len(inputs))
	}
	return dedup.BuildFrom(inputs), nil
}

// copyMissing копирует письма entries из src в папку dstFolder на dst.
// Возвращает число успешно скопированных и (в инкрементальном режиме) их
// закэшированные представления для стороны назначения.
func (s *Syncer) copyMissing(sess *session, src, dst *mailbox.Client, dstFolder string, entries []*dedup.Entry, dir, srcFolder string) (int, []store.CachedMsg) {
	n := 0
	var cache []store.CachedMsg
	for i, e := range entries {
		if i%64 == 0 {
			if err := sess.ctx.Err(); err != nil {
				s.recordErr(sess.us, fmt.Errorf("юзер %s, %s (%q): прервано: %w", sess.u.Name, dir, srcFolder, err))
				return n, cache
			}
		}
		uid, err := s.copyOne(src, dst, dstFolder, e)
		if err != nil {
			s.recordErr(sess.us, fmt.Errorf("юзер %s, %s (%q -> %q), uid=%d: %w", sess.u.Name, dir, srcFolder, dstFolder, e.Uid, err))
			continue
		}
		n++
		if s.incremental() && uid != 0 {
			cache = append(cache, store.CachedMsg{
				Uid: uid, MsgID: e.MsgID, XHash: e.Surrogate, Surrogate: e.Surrogate,
				InternalDate: e.InternalDate, Flags: filterFlags(e.Flags),
			})
		}
	}
	return n, cache
}

// copyOne забирает письмо целиком, проставляет суррогатный хеш в кастомный
// заголовок и дописывает в целевую папку с сохранением флагов и внутренней даты.
// Тело письма не копируется лишний раз: заголовок добавляется потоково.
// Возвращает присвоенный UID (0, если сервер не поддерживает APPENDUID).
func (s *Syncer) copyOne(src, dst *mailbox.Client, dstFolder string, e *dedup.Entry) (uint32, error) {
	body, err := src.FetchFullLiteral(e.Uid)
	if err != nil {
		return 0, err
	}
	// Суррогатный хеш пишем всегда - чтобы на следующих проходах письмо
	// находилось по заголовку даже если Message-ID появится/исчезнет.
	msg := mailbox.WithHashHeader(body, s.cfg.HashHeader, e.Surrogate)

	return dst.AppendLiteral(dstFolder, filterFlags(e.Flags), e.InternalDate, msg)
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

// isConnErr - похоже ли на потерю соединения (повод переподключиться).
func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	msg := err.Error()
	for _, sub := range []string{
		"connection reset", "broken pipe", "use of closed", "connection closed",
		"connection refused", "unexpected EOF", "i/o timeout", "TLS handshake",
		"imap: connection closed",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// recordErr логирует ошибку с контекстом и учитывает её в статистике юзера.
func (s *Syncer) recordErr(us *stats.UserStats, err error) {
	us.AddError(err)
	s.logf("ошибка: %v", err)
}

// persistStatus сохраняет итог прохода по юзеру в таблицу user_status (если
// открыт store).
func (s *Syncer) persistStatus(name string, us *stats.UserStats) {
	if s.state == nil {
		return
	}
	r := us.Report()
	run := store.RunResult{
		At:         time.Now(),
		Status:     "ok",
		CopiedAToB: r.CopiedAToB,
		CopiedBToA: r.CopiedBToA,
		SkippedDup: r.SkippedDup,
		Errors:     r.Errors,
		LastError:  r.LastErr,
	}
	if r.Errors > 0 {
		run.Status = "error"
	}
	if err := s.state.RecordRun(name, run); err != nil {
		s.logf("юзер %s: не удалось записать статус в БД: %v", name, err)
	}
}
