package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"imapsync/config"
	"imapsync/internal/stats"
	"imapsync/internal/store"
	"imapsync/internal/syncer"
)

// runDaemon runs the endless sync loop and shuts down cleanly on SIGINT/SIGTERM.
func runDaemon(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logf := log.Printf

	// The DB is needed for incremental reconciliation, user statuses and the
	// error-streak limit. Exclusive lock: two daemons on one DB would corrupt
	// the state.
	needDB := cfg.StateCache || cfg.Source == config.SourceSQLite ||
		(cfg.MaxFailStreak > 0 && cfg.SQLitePath != "")
	var st *store.Store
	if needDB {
		var err error
		st, err = store.OpenExclusive(cfg.SQLitePath)
		if err != nil {
			return err
		}
		defer st.Close()
		if cfg.StateCache {
			logf("incremental reconciliation enabled, cache: %s (full_resync every %s)",
				cfg.SQLitePath, cfg.FullResyncEvery.Std())
		}
		if cfg.MaxFailStreak > 0 {
			logf("a user's sync stops after %d consecutive errors (reset: imapsync db-resume-user)", cfg.MaxFailStreak)
		}
	} else if cfg.MaxFailStreak > 0 {
		logf("warning: max_fail_streak=%d is set but there is no DB (needs sqlite_path or state_cache) - the limit has no effect", cfg.MaxFailStreak)
	}

	coll := stats.New()
	pool := syncer.NewPool(cfg, coll, logf, st)

	logf("starting: A=%s B=%s, users=%d, workers=%d, cycle interval=%s",
		cfg.ServerA.Addr(), cfg.ServerB.Addr(), len(cfg.Users), cfg.Workers, cfg.SyncInterval.Std())

	pool.Run(ctx)

	if ctx.Err() != nil {
		logf("shutdown signal received, daemon stopped")
	}
	return nil
}
