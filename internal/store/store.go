// Package store is a local SQLite DB holding folder pairs and users. It is used
// as an alternative config source (source: sqlite) so large lists don't have to
// live in YAML.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // "sqlite" driver (pure Go, no CGO)

	"imapsync/config"
)

const schema = `
CREATE TABLE IF NOT EXISTS folder_pairs (
    folder_a TEXT NOT NULL,
    folder_b TEXT NOT NULL,
    PRIMARY KEY (folder_a, folder_b)
);
CREATE TABLE IF NOT EXISTS users (
    name    TEXT PRIMARY KEY,
    user_a  TEXT NOT NULL,
    user_b  TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1
);
`

// Store is an open DB.
type Store struct {
	db   *sql.DB
	lock *os.File // != nil with OpenExclusive; holds the flock until Close
}

// poolSize is the DB connection pool size. WAL allows concurrent reads; writes
// are serialized via busy_timeout, not via a single connection.
const poolSize = 8

// dsn builds the connection string with pragmas applied to every connection.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) the DB at path and applies the schema.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite path not set")
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := migrateState(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating sqlite schema %s: %w", path, err)
	}
	if _, err := db.Exec(stateSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating sqlite state-cache schema %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// migrateState brings the state-cache tables up to schemaVersion. On a version
// mismatch the cache (sync_endpoint / sync_msg_cache) is recreated - that is
// acceptable, a full rescan restores it. user_status / user_run are left alone.
func migrateState(db *sql.DB) error {
	var uv int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil {
		return err
	}
	if uv >= schemaVersion {
		return nil
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS sync_endpoint; DROP TABLE IF EXISTS sync_msg_cache;`); err != nil {
		return err
	}
	// PRAGMA does not accept placeholders.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return nil
}

// Close closes the DB and releases the advisory lock if one was taken.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
	return err
}

// UpsertUser inserts or updates a user by name.
func (s *Store) UpsertUser(u config.User, enabled bool) error {
	name := strings.TrimSpace(u.Name)
	a := strings.TrimSpace(u.UserA)
	b := strings.TrimSpace(u.UserB)
	if name == "" || a == "" || b == "" {
		return fmt.Errorf("user: empty field (name=%q user_a=%q user_b=%q)", name, a, b)
	}
	en := 0
	if enabled {
		en = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO users (name, user_a, user_b, enabled) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET user_a=excluded.user_a, user_b=excluded.user_b, enabled=excluded.enabled`,
		name, a, b, en)
	if err != nil {
		return fmt.Errorf("upserting user %q: %w", name, err)
	}
	return nil
}

// UpsertFolderPair adds a folder pair (no duplicates).
func (s *Store) UpsertFolderPair(fp config.FolderPair) error {
	a := strings.TrimSpace(fp.A)
	b := strings.TrimSpace(fp.B)
	if a == "" || b == "" {
		return fmt.Errorf("folder pair: empty name (a=%q b=%q)", a, b)
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO folder_pairs (folder_a, folder_b) VALUES (?, ?)`, a, b)
	if err != nil {
		return fmt.Errorf("inserting folder pair %q/%q: %w", a, b, err)
	}
	return nil
}

// SetUserEnabled enables/disables a user.
func (s *Store) SetUserEnabled(name string, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	res, err := s.db.Exec(`UPDATE users SET enabled=? WHERE name=?`, en, strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("changing enabled for %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user %q not found", name)
	}
	return nil
}

// ListUsers returns the enabled users (for running the daemon).
func (s *Store) ListUsers() ([]config.User, error) {
	return s.queryUsers(true)
}

// AllUsers returns all users (for listing).
func (s *Store) AllUsers() ([]config.User, error) {
	return s.queryUsers(false)
}

func (s *Store) queryUsers(onlyEnabled bool) ([]config.User, error) {
	q := `SELECT name, user_a, user_b FROM users`
	if onlyEnabled {
		q += ` WHERE enabled=1`
	}
	q += ` ORDER BY name`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("selecting users: %w", err)
	}
	defer rows.Close()

	var out []config.User
	for rows.Next() {
		var u config.User
		if err := rows.Scan(&u.Name, &u.UserA, &u.UserB); err != nil {
			return nil, fmt.Errorf("reading user row: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListFolderPairs returns all folder pairs.
func (s *Store) ListFolderPairs() ([]config.FolderPair, error) {
	rows, err := s.db.Query(`SELECT folder_a, folder_b FROM folder_pairs ORDER BY folder_a, folder_b`)
	if err != nil {
		return nil, fmt.Errorf("selecting folder pairs: %w", err)
	}
	defer rows.Close()

	var out []config.FolderPair
	for rows.Next() {
		var fp config.FolderPair
		if err := rows.Scan(&fp.A, &fp.B); err != nil {
			return nil, fmt.Errorf("reading folder pair row: %w", err)
		}
		out = append(out, fp)
	}
	return out, rows.Err()
}

// LoadInto populates cfg.Users and cfg.Folders from the DB.
func (s *Store) LoadInto(cfg *config.Config) error {
	users, err := s.ListUsers()
	if err != nil {
		return err
	}
	folders, err := s.ListFolderPairs()
	if err != nil {
		return err
	}
	cfg.Users = users
	cfg.Folders = folders
	return nil
}

// ImportConfig moves all folder pairs and all users from cfg into the DB.
// Returns the number of folders and users processed.
func (s *Store) ImportConfig(cfg *config.Config) (folders, users int, err error) {
	for _, fp := range cfg.Folders {
		if err = s.UpsertFolderPair(fp); err != nil {
			return
		}
		folders++
	}
	for _, u := range cfg.Users {
		if err = s.UpsertUser(u, true); err != nil {
			return
		}
		users++
	}
	return
}
