package syncer

import (
	"context"
	"sync"
	"time"

	"imapsync/config"
	"imapsync/internal/stats"
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
}

// NewPool собирает пул с продакшн-синкером.
func NewPool(cfg *config.Config, coll *stats.Collector, logf stats.Logf) *Pool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Pool{
		cfg:   cfg,
		sync:  New(cfg, coll, logf),
		stats: coll,
		logf:  logf,
	}
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
