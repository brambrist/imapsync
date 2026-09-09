package syncer

import (
	"context"
	"sync"
	"time"

	"imapsync/config"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

// userSyncer - то, что пул применяет к каждому юзеру. Продакшн-реализация -
// *Syncer; в тестах подменяется заглушкой.
type userSyncer interface {
	SyncUser(ctx context.Context, u config.User)
}

// Pool - worker-pool: N воркеров (N < числа юзеров) разбирают очередь юзеров.
// Один юзер обрабатывается одним воркером целиком.
type Pool struct {
	cfg   *config.Config
	sync  userSyncer
	stats *stats.Collector
	logf  stats.Logf

	// reloadSrc != nil => перед каждым циклом перечитываем списки юзеров и папок
	// из БД (актуально при source: sqlite - изменения через db-* подхватываются
	// без рестарта демона).
	reloadSrc *store.Store
}

// NewPool собирает пул с продакшн-синкером. Если st != nil, он передаётся
// синкеру (инкрементальная сверка при cfg.StateCache и запись user_status);
// при source: sqlite он же используется для горячей перезагрузки списков.
func NewPool(cfg *config.Config, coll *stats.Collector, logf stats.Logf, st *store.Store) *Pool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &Pool{
		cfg:   cfg,
		sync:  NewWithState(cfg, coll, logf, st),
		stats: coll,
		logf:  logf,
	}
	if cfg.Source == config.SourceSQLite && st != nil {
		p.reloadSrc = st
	}
	return p
}

// reload перечитывает списки юзеров и папок из БД (между циклами, когда воркеров
// нет - гонки не возникает). Ошибку чтения игнорируем, оставляя прежние списки.
func (p *Pool) reload() {
	if p.reloadSrc == nil {
		return
	}
	users, err := p.reloadSrc.ListUsers()
	if err != nil {
		p.logf("не удалось перечитать юзеров из БД: %v (оставляю прежний список)", err)
		return
	}
	folders, err := p.reloadSrc.ListFolderPairs()
	if err != nil {
		p.logf("не удалось перечитать папки из БД: %v (оставляю прежний список)", err)
		return
	}
	if pu, pf := len(p.cfg.Users), len(p.cfg.Folders); pu != len(users) || pf != len(folders) {
		p.logf("списки из БД обновлены: юзеров %d->%d, пар папок %d->%d", pu, len(users), pf, len(folders))
	}
	p.cfg.Users, p.cfg.Folders = users, folders
}

// workers - фактическое число воркеров: не больше числа юзеров.
func (p *Pool) workers() int {
	w := p.cfg.Workers
	if n := len(p.cfg.Users); w > n {
		w = n
	}
	if w < 1 {
		w = 1
	}
	return w
}

// Run гоняет полные циклы синхронизации с паузой sync_interval между ними, пока
// не отменят ctx. Параллельно раз в stats_interval выводится сводка.
func (p *Pool) Run(ctx context.Context) {
	stop := p.stats.StartReporter(ctx, p.cfg.StatsInterval.Std(), p.logf)
	defer stop()

	for {
		started := time.Now()
		p.reload()
		p.logf("цикл синхронизации начат: юзеров=%d, воркеров=%d", len(p.cfg.Users), p.workers())
		p.RunCycle(ctx)
		p.stats.LogSummary(p.logf)
		p.logf("цикл завершён за %s", time.Since(started).Round(time.Second))

		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.cfg.SyncInterval.Std()):
		}
	}
}

// RunCycle обрабатывает всех юзеров ровно один раз. Возвращается, когда очередь
// разобрана или ctx отменён (уже начатые юзеры при этом доводятся до конца -
// SyncUser сам реагирует на отмену на своих контрольных точках).
func (p *Pool) RunCycle(ctx context.Context) {
	p.stats.BeginCycle()

	queue := make(chan config.User)
	var wg sync.WaitGroup
	for range p.workers() {
		wg.Go(func() {
			for u := range queue {
				p.processUser(ctx, u)
			}
		})
	}

feed:
	for _, u := range p.cfg.Users {
		select {
		case <-ctx.Done():
			break feed
		case queue <- u:
		}
	}
	close(queue)
	wg.Wait()
}

// processUser обрабатывает одного юзера с ограничением per_user_timeout.
func (p *Pool) processUser(ctx context.Context, u config.User) {
	uctx, cancel := context.WithTimeout(ctx, p.cfg.PerUserTimeout.Std())
	defer cancel()
	p.sync.SyncUser(uctx, u)
}
