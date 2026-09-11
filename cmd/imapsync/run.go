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
	cfgPath := fs.String("config", "config.yaml", "path to the YAML config")
	debug := fs.Bool("debug", false, "verbose IMAP/EWS protocol logging (same as config: debug: true). "+
		"WARNING: IMAP debug logs the AUTHENTICATE exchange, which carries credentials - treat the log as a secret")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *debug {
		cfg.Debug = true
	}

	if cfg.Source == config.SourceSQLite {
		if err := loadFromSQLite(cfg); err != nil {
			return err
		}
		log.Printf("config from sqlite %s: users=%d, folder pairs=%d", cfg.SQLitePath, len(cfg.Users), len(cfg.Folders))
	} else {
		log.Printf("config from YAML: users=%d, folder pairs=%d", len(cfg.Users), len(cfg.Folders))
	}

	return runDaemon(cfg)
}

// loadFromSQLite loads the user and folder lists from the DB and validates them.
// The DB is closed right after reading - the daemon does not need it anymore.
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
		return fmt.Errorf("checking data from sqlite %s: %w", cfg.SQLitePath, err)
	}
	return nil
}
