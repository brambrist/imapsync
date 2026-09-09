// Package store - локальная БД SQLite с парами папок и пользователей.
// Используется как альтернативный источник конфигурации (source: sqlite),
// чтобы не держать большие списки в YAML.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	_ "modernc.org/sqlite" // драйвер "sqlite" (чистый Go, без CGO)

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

// Store - открытая БД.
type Store struct {
	db   *sql.DB
	lock *os.File // != nil при OpenExclusive; держит flock до Close
}

// Open открывает (создавая при необходимости) БД по пути path и применяет схему.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("путь к sqlite не задан")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("открытие sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // sqlite: сериализуем доступ, избегаем "database is locked"
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("настройка sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("создание схемы sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(stateSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("создание схемы кэша состояния sqlite %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close закрывает БД и снимает advisory-блокировку, если она бралась.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
	return err
}

// UpsertUser добавляет или обновляет пользователя по имени.
func (s *Store) UpsertUser(u config.User, enabled bool) error {
	name := strings.TrimSpace(u.Name)
	a := strings.TrimSpace(u.UserA)
	b := strings.TrimSpace(u.UserB)
	if name == "" || a == "" || b == "" {
		return fmt.Errorf("пользователь: пустое поле (name=%q user_a=%q user_b=%q)", name, a, b)
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
		return fmt.Errorf("upsert пользователя %q: %w", name, err)
	}
	return nil
}

// UpsertFolderPair добавляет пару папок (без дублей).
func (s *Store) UpsertFolderPair(fp config.FolderPair) error {
	a := strings.TrimSpace(fp.A)
	b := strings.TrimSpace(fp.B)
	if a == "" || b == "" {
		return fmt.Errorf("пара папок: пустое имя (a=%q b=%q)", a, b)
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO folder_pairs (folder_a, folder_b) VALUES (?, ?)`, a, b)
	if err != nil {
		return fmt.Errorf("вставка пары папок %q/%q: %w", a, b, err)
	}
	return nil
}

// SetUserEnabled включает/выключает пользователя.
func (s *Store) SetUserEnabled(name string, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	res, err := s.db.Exec(`UPDATE users SET enabled=? WHERE name=?`, en, strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("смена enabled для %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("пользователь %q не найден", name)
	}
	return nil
}

// ListUsers возвращает включённых пользователей (для запуска демона).
func (s *Store) ListUsers() ([]config.User, error) {
	return s.queryUsers(true)
}

// AllUsers возвращает всех пользователей (для вывода списка).
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
		return nil, fmt.Errorf("выборка пользователей: %w", err)
	}
	defer rows.Close()

	var out []config.User
	for rows.Next() {
		var u config.User
		if err := rows.Scan(&u.Name, &u.UserA, &u.UserB); err != nil {
			return nil, fmt.Errorf("чтение строки пользователя: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListFolderPairs возвращает все пары папок.
func (s *Store) ListFolderPairs() ([]config.FolderPair, error) {
	rows, err := s.db.Query(`SELECT folder_a, folder_b FROM folder_pairs ORDER BY folder_a, folder_b`)
	if err != nil {
		return nil, fmt.Errorf("выборка пар папок: %w", err)
	}
	defer rows.Close()

	var out []config.FolderPair
	for rows.Next() {
		var fp config.FolderPair
		if err := rows.Scan(&fp.A, &fp.B); err != nil {
			return nil, fmt.Errorf("чтение строки пары папок: %w", err)
		}
		out = append(out, fp)
	}
	return out, rows.Err()
}

// LoadInto заполняет cfg.Users и cfg.Folders из БД.
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

// ImportConfig переносит в БД все пары папок и всех юзеров из cfg.
// Возвращает число обработанных папок и юзеров.
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
