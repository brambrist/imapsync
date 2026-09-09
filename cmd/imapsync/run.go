package main

import (
	"flag"
	"fmt"
	"log"

	"imapsync/config"
	"imapsync/internal/store"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "путь к YAML-конфигу")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Source == config.SourceSQLite {
		if err := loadFromSQLite(cfg); err != nil {
			return err
		}
		log.Printf("конфигурация из sqlite %s: юзеров=%d, пар папок=%d", cfg.SQLitePath, len(cfg.Users), len(cfg.Folders))
	} else {
		log.Printf("конфигурация из YAML: юзеров=%d, пар папок=%d", len(cfg.Users), len(cfg.Folders))
	}

	return runDaemon(cfg)
}

// loadFromSQLite дозагружает списки юзеров и папок из БД и валидирует их.
// БД закрывается сразу после чтения - демону она больше не нужна.
func loadFromSQLite(cfg *config.Config) error {
	st, err := store.Open(cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.LoadInto(cfg); err != nil {
		return err
	}
	if err := cfg.ValidateEntities(); err != nil {
		return fmt.Errorf("проверка данных из sqlite %s: %w", cfg.SQLitePath, err)
	}
	return nil
}
