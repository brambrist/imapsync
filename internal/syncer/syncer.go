// Package syncer - синхронизация одного пользователя: подключение к обоим
// концам, построение индексов по каждой паре папок, вычисление дельты и
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

	"imapsync/config"
	"imapsync/internal/dedup"
	"imapsync/internal/endpoint"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

// Syncer держит общую конфигурацию, бэкенды обоих концов и коллектор статистики.
type Syncer struct {
	cfg      *config.Config
	stats    *stats.Collector
	logf     stats.Logf
	state    *store.Store // != nil => инкрементальная сверка через кэш и запись user_status
	backendA endpoint.Backend
	backendB endpoint.Backend
}

// New создаёт синкер с полной сверкой папок каждый цикл.
func New(cfg *config.Config, coll *stats.Collector, logf stats.Logf) *Syncer {
	return NewWithState(cfg, coll, logf, nil)
}

// NewWithState создаёт синкер; если state != nil, включается инкрементальная
// сверка (при cfg.StateCache) и запись статуса юзеров. Паникует, если бэкенд не
// собрать - конфиг должен быть провалидирован заранее.
func NewWithState(cfg *config.Config, coll *stats.Collector, logf stats.Logf, st *store.Store) *Syncer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mk := func(srv config.Server) endpoint.Backend {
		b, err := endpoint.NewBackend(srv, cfg.DialTimeout.Std(), cfg.IOTimeout.Std(), cfg.InsecureTLS, cfg.FetchBatchSize)
		if err != nil {
			panic(err)
		}
		return b
	}
	return &Syncer{
		cfg: cfg, stats: coll, logf: logf, state: st,
		backendA: mk(cfg.ServerA),
		backendB: mk(cfg.ServerB),
	}
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
// они логируются с контекстом и попадают в статистику юзера.
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

// session - состояние обработки одного юзера: оба конца, контекст, счётчики.
type session struct {
	s   *Syncer
	ctx context.Context
	u   config.User
	us  *stats.UserStats
	a   endpoint.Endpoint
	b   endpoint.Endpoint
}

func (sess *session) connect() bool {
	a, err := sess.s.dial(sess.ctx, sess.s.backendA, sess.u.UserA)
	if err != nil {
		sess.s.recordErr(sess.us, fmt.Errorf("юзер %s: %w", sess.u.Name, err))
		return false
	}
	b, err := sess.s.dial(sess.ctx, sess.s.backendB, sess.u.UserB)
	if err != nil {
		a.Close()
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
		sess.a.Close()
		sess.a = nil
	}
	if sess.b != nil {
		sess.b.Close()
		sess.b = nil
	}
}

// dial подключается к концу с повторами при транзиентных ошибках.
func (s *Syncer) dial(ctx context.Context, backend endpoint.Backend, user string) (endpoint.Endpoint, error) {
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
			s.logf("повтор подключения к %s (юзер %s), попытка %d/%d", backend.Addr(), user, i+1, attempts)
		}
		ep, err := backend.Connect(ctx, user)
		if err == nil {
			return ep, nil
		}
		lastErr = err
		if !isConnErr(err) {
			break // ошибка не похожа на транзиентную (нет каталога, отказ авторизации) - без повторов
		}
	}
	return nil, lastErr
}

// syncPair синхронизирует одну пару папок в обе стороны. Возвращает ошибку
// уровня папки (Select/List/Fetch); ошибки отдельных писем логируются внутри.
func (sess *session) syncPair(fp config.FolderPair) error {
	s, u, us := sess.s, sess.u, sess.us

	folderA, validA, err := sess.a.Select(fp.A)
	if err != nil {
		return fmt.Errorf("юзер %s, папка A %q: %w", u.Name, fp.A, err)
	}
	folderB, validB, err := sess.b.Select(fp.B)
	if err != nil {
		return fmt.Errorf("юзер %s, папка B %q: %w", u.Name, fp.B, err)
	}

	idxA, err := s.indexFolder(sess, "a", fp, folderA, validA)
	if err != nil {
		return err
	}
	idxB, err := s.indexFolder(sess, "b", fp, folderB, validB)
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

	copiedAB, cacheB := s.copyMissing(sess, sess.a, sess.b, missingOnB, "A->B", folderA, folderB)
	us.IncCopiedAToB(copiedAB)

	copiedBA, cacheA := s.copyMissing(sess, sess.b, sess.a, missingOnA, "B->A", folderB, folderA)
	us.IncCopiedBToA(copiedBA)

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

func (sess *session) endpoint(side string) endpoint.Endpoint {
	if side == "b" {
		return sess.b
	}
	return sess.a
}

// indexFolder строит индекс писем одной стороны папки. side - "a" | "b".
func (s *Syncer) indexFolder(sess *session, side string, fp config.FolderPair, folder, validity string) (*dedup.Index, error) {
	if s.incremental() {
		return s.indexFolderIncremental(sess, side, fp, folder, validity)
	}
	return s.indexFolderFull(sess, side, folder)
}

// indexFolderFull забирает и разбирает заголовки всех писем папки.
func (s *Syncer) indexFolderFull(sess *session, side, folder string) (*dedup.Index, error) {
	msgs, err := sess.endpoint(side).FetchMeta(nil)
	if err != nil {
		return nil, fmt.Errorf("юзер %s, папка %s %q: %w", sess.u.Name, side, folder, err)
	}
	idx, errs := dedup.Build(msgs, s.cfg.HashHeader)
	for _, e := range errs {
		s.recordErr(sess.us, fmt.Errorf("юзер %s, папка %s %q: разбор: %w", sess.u.Name, side, folder, e))
	}
	return idx, nil
}

// indexFolderIncremental берёт список ID, фетчит метаданные только для новых
// писем, остальное поднимает из кэша sqlite; кэш при этом обновляется.
// Периодически (full_resync_every) делает полный пере-скан.
func (s *Syncer) indexFolderIncremental(sess *session, side string, fp config.FolderPair, folder, validity string) (*dedup.Index, error) {
	u, us := sess.u, sess.us
	ep := sess.endpoint(side)
	pair := store.PairKey(fp.A, fp.B)
	wrap := func(err error) error {
		return fmt.Errorf("юзер %s, папка %s %q: %w", u.Name, side, folder, err)
	}

	saved, cached, err := s.state.LoadEndpoint(u.Name, pair, side)
	if err != nil {
		return nil, wrap(err)
	}

	resyncAt := saved.FullResyncAt
	reset := func(reason string) error {
		if len(cached) > 0 {
			s.logf("юзер %s, папка %s %q: %s, полный пере-скан", u.Name, side, folder, reason)
		}
		if err := s.state.ResetEndpoint(u.Name, pair, side); err != nil {
			return wrap(fmt.Errorf("сброс кэша: %w", err))
		}
		cached = map[string]store.CachedMsg{}
		resyncAt = time.Now()
		return nil
	}

	switch {
	case saved.Exists && saved.Validity != validity:
		if err := reset(fmt.Sprintf("валидность папки изменилась (%q -> %q)", saved.Validity, validity)); err != nil {
			return nil, err
		}
	case s.cfg.FullResyncEvery.Std() > 0 && time.Since(saved.FullResyncAt) >= s.cfg.FullResyncEvery.Std():
		if err := reset("плановый полный пере-скан"); err != nil {
			return nil, err
		}
	}

	curIDs, err := ep.ListIDs()
	if err != nil {
		return nil, wrap(err)
	}
	slices.Sort(curIDs)

	curSet := make(map[string]struct{}, len(curIDs))
	var newIDs []string
	for _, id := range curIDs {
		curSet[id] = struct{}{}
		if _, ok := cached[id]; !ok {
			newIDs = append(newIDs, id)
		}
	}
	var goneIDs []string
	for id := range cached {
		if _, ok := curSet[id]; !ok {
			goneIDs = append(goneIDs, id)
		}
	}

	fetched, err := ep.FetchMeta(newIDs)
	if err != nil {
		return nil, wrap(err)
	}

	fresh := make([]store.CachedMsg, 0, len(fetched))
	for _, m := range fetched {
		in, perr := dedup.ParseMessage(m, s.cfg.HashHeader)
		if perr != nil {
			s.recordErr(us, wrap(perr))
			continue
		}
		cm := store.CachedMsg{
			ID: m.ID, MsgID: in.MsgID, XHash: in.XHash,
			Surrogate: in.Surrogate, InternalDate: m.InternalDate, Flags: m.Flags,
		}
		fresh = append(fresh, cm)
		cached[m.ID] = cm
	}

	if err := s.state.PutCachedMsgs(u.Name, pair, side, fresh); err != nil {
		s.recordErr(us, wrap(fmt.Errorf("запись кэша: %w", err)))
	}
	if len(goneIDs) > 0 {
		if err := s.state.DeleteCachedMsgs(u.Name, pair, side, goneIDs); err != nil {
			s.recordErr(us, wrap(fmt.Errorf("чистка кэша: %w", err)))
		}
		for _, id := range goneIDs {
			delete(cached, id)
		}
	}
	if err := s.state.SaveEndpoint(u.Name, pair, side, validity, resyncAt); err != nil {
		s.recordErr(us, wrap(fmt.Errorf("запись эндпоинта: %w", err)))
	}

	inputs := make([]dedup.Input, 0, len(curIDs))
	for _, id := range curIDs {
		cm, ok := cached[id]
		if !ok {
			continue // разбор нового письма упал
		}
		inputs = append(inputs, dedup.Input{
			ID:           cm.ID,
			Flags:        cm.Flags,
			InternalDate: cm.InternalDate,
			MsgID:        cm.MsgID,
			XHash:        cm.XHash,
			Surrogate:    cm.Surrogate,
			Keys:         dedup.Keys(cm.MsgID, cm.XHash, cm.Surrogate),
		})
	}
	if len(newIDs) > 0 || len(goneIDs) > 0 {
		s.logf("юзер %s, папка %s %q: инкрементально - новых %d, удалено %d, всего %d",
			u.Name, side, folder, len(newIDs), len(goneIDs), len(inputs))
	}
	return dedup.BuildFrom(inputs), nil
}

// copyMissing копирует письма entries из src в текущую папку на dst.
// Возвращает число успешно скопированных и (в инкрементальном режиме) их
// закэшированные представления для стороны назначения.
func (s *Syncer) copyMissing(sess *session, src, dst endpoint.Endpoint, entries []*dedup.Entry, dir, srcFolder, dstFolder string) (int, []store.CachedMsg) {
	n := 0
	var cache []store.CachedMsg
	for i, e := range entries {
		if i%64 == 0 {
			if err := sess.ctx.Err(); err != nil {
				s.recordErr(sess.us, fmt.Errorf("юзер %s, %s (%q): прервано: %w", sess.u.Name, dir, srcFolder, err))
				return n, cache
			}
		}
		newID, err := s.copyOne(src, dst, e)
		if err != nil {
			s.recordErr(sess.us, fmt.Errorf("юзер %s, %s (%q -> %q), id=%s: %w", sess.u.Name, dir, srcFolder, dstFolder, e.ID, err))
			continue
		}
		n++
		if s.incremental() && newID != "" {
			cache = append(cache, store.CachedMsg{
				ID: newID, MsgID: e.MsgID, XHash: e.Surrogate, Surrogate: e.Surrogate,
				InternalDate: e.InternalDate, Flags: filterFlags(e.Flags),
			})
		}
	}
	return n, cache
}

// copyOne забирает письмо целиком, добавляет суррогатный хеш в кастомный
// заголовок (потоково) и дописывает в текущую папку dst с сохранением флагов и
// внутренней даты. Возвращает ID нового письма ("" если транспорт не сообщает).
func (s *Syncer) copyOne(src, dst endpoint.Endpoint, e *dedup.Entry) (string, error) {
	body, err := src.Open(e.ID)
	if err != nil {
		return "", err
	}
	msg := endpoint.WithHeader(body, s.cfg.HashHeader, e.Surrogate)
	return dst.Append(filterFlags(e.Flags), e.InternalDate, msg)
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

// persistStatus сохраняет итог прохода по юзеру в БД (если открыт store).
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
