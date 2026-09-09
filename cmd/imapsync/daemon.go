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

// runDaemon запускает бесконечный цикл синхронизации и корректно завершается
// по SIGINT/SIGTERM.
func runDaemon(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logf := log.Printf

	// БД нужна для инкрементальной сверки и/или записи статусов юзеров.
	var st *store.Store
	if cfg.StateCache || cfg.Source == config.SourceSQLite {
		var err error
		st, err = store.Open(cfg.SQLitePath)
		if err != nil {
			return err
		}
		defer st.Close()
		if cfg.StateCache {
			logf("инкрементальная сверка включена, кэш: %s", cfg.SQLitePath)
		}
	}

	coll := stats.New()
	pool := syncer.NewPool(cfg, coll, logf, st)

	logf("запуск: A=%s B=%s, юзеров=%d, воркеров=%d, интервал цикла=%s",
		cfg.ServerA.Addr(), cfg.ServerB.Addr(), len(cfg.Users), cfg.Workers, cfg.SyncInterval.Std())

	pool.Run(ctx)

	if ctx.Err() != nil {
		logf("получен сигнал завершения, демон остановлен")
	}
	return nil
}
