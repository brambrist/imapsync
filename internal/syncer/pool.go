package syncer

import (
	"context"
	"sync"
	"time"

	"imapsync/config"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

// userSyncer - what the pool applies to each user. The production implementation
// is *Syncer; in tests it is replaced by a stub.
type userSyncer interface {
	SyncUser(ctx context.Context, u config.User)
}

// Pool - a worker pool: N workers (N < number of users) drain the user queue.
// One user is processed by one worker end to end.
type Pool struct {
	cfg   *config.Config
	sync  userSyncer
	stats *stats.Collector
	logf  stats.Logf

	// reloadSrc != nil => re-read the user and folder lists from the DB before
	// each cycle (relevant with source: sqlite - changes via db-* are picked up
	// without restarting the daemon).
	reloadSrc *store.Store
}

// NewPool assembles a pool with the production syncer. If st != nil it is passed
// to the syncer (incremental reconciliation with cfg.StateCache and user_status
// writes); with source: sqlite it is also used to hot-reload the lists.
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

// reload re-reads the user and folder lists from the DB (between cycles, when
// there are no workers - no race). A read error is ignored, keeping the previous
// lists.
func (p *Pool) reload() {
	if p.reloadSrc == nil {
		return
	}
	users, err := p.reloadSrc.ListUsers()
	if err != nil {
		p.logf("could not re-read users from the DB: %v (keeping the previous list)", err)
		return
	}
	folders, err := p.reloadSrc.ListFolderPairs()
	if err != nil {
		p.logf("could not re-read folders from the DB: %v (keeping the previous list)", err)
		return
	}
	if pu, pf := len(p.cfg.Users), len(p.cfg.Folders); pu != len(users) || pf != len(folders) {
		p.logf("lists updated from the DB: users %d->%d, folder pairs %d->%d", pu, len(users), pf, len(folders))
	}
	p.cfg.Users, p.cfg.Folders = users, folders
}

// workers - the effective worker count: no more than the number of users.
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

// Run runs full sync cycles with a sync_interval pause between them until ctx is
// cancelled. In parallel a summary is printed every stats_interval.
func (p *Pool) Run(ctx context.Context) {
	stop := p.stats.StartReporter(ctx, p.cfg.StatsInterval.Std(), p.logf)
	defer stop()

	for {
		started := time.Now()
		p.reload()
		p.logf("sync cycle started: users=%d, workers=%d", len(p.cfg.Users), p.workers())
		p.RunCycle(ctx)
		p.stats.LogSummary(p.logf)
		p.logf("cycle finished in %s", time.Since(started).Round(time.Second))

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

// RunCycle processes every user exactly once. Returns when the queue is drained
// or ctx is cancelled (users already started are then finished - SyncUser
// reacts to cancellation at its own checkpoints).
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

// processUser processes one user with the per_user_timeout limit.
func (p *Pool) processUser(ctx context.Context, u config.User) {
	uctx, cancel := context.WithTimeout(ctx, p.cfg.PerUserTimeout.Std())
	defer cancel()
	p.sync.SyncUser(uctx, u)
}
