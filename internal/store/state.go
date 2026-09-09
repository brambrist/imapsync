package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"imapsync/config"
)

// stateSchema - the incremental-reconciliation cache tables. They live in the
// same file as the config tables.
//
// pair is a string key for a folder pair ("<folder_a>\x00<folder_b>"),
// side is 'a'|'b'.
//
// schemaVersion - when the cache tables (sync_endpoint/sync_msg_cache) evolve
// they are recreated (this is a pure cache, a rescan restores it). user_status /
// user_run are preserved.
const schemaVersion = 2

const stateSchema = `
CREATE TABLE IF NOT EXISTS sync_endpoint (
    user_name      TEXT NOT NULL,
    pair           TEXT NOT NULL,
    side           TEXT NOT NULL,
    validity       TEXT NOT NULL,
    full_resync_at INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (user_name, pair, side)
);
CREATE TABLE IF NOT EXISTS sync_msg_cache (
    user_name    TEXT NOT NULL,
    pair         TEXT NOT NULL,
    side         TEXT NOT NULL,
    msg_id       TEXT NOT NULL,
    msgid        TEXT NOT NULL,
    xhash        TEXT NOT NULL,
    surrogate    TEXT NOT NULL,
    internaldate INTEGER NOT NULL,
    flags        TEXT NOT NULL,
    PRIMARY KEY (user_name, pair, side, msg_id)
);
CREATE TABLE IF NOT EXISTS user_status (
    user_name    TEXT PRIMARY KEY,
    last_run_at  INTEGER NOT NULL,
    last_ok_at   INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL,          -- ok | error
    copied_a_b   INTEGER NOT NULL DEFAULT 0,
    copied_b_a   INTEGER NOT NULL DEFAULT 0,
    skipped_dup  INTEGER NOT NULL DEFAULT 0,
    errors       INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    fail_since   INTEGER NOT NULL DEFAULT 0,  -- start of the current error streak; 0 = currently ok
    fail_streak  INTEGER NOT NULL DEFAULT 0   -- number of consecutive failed runs
);
CREATE TABLE IF NOT EXISTS user_run (
    user_name   TEXT NOT NULL,
    run_at      INTEGER NOT NULL,
    status      TEXT NOT NULL,
    copied_a_b  INTEGER NOT NULL DEFAULT 0,
    copied_b_a  INTEGER NOT NULL DEFAULT 0,
    skipped_dup INTEGER NOT NULL DEFAULT 0,
    errors      INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user_name, run_at)
);
CREATE INDEX IF NOT EXISTS user_run_by_user ON user_run (user_name, run_at DESC);
`

// userRunKeep - how many recent runs to keep in user_run per user.
const userRunKeep = 200

// Endpoint - the stored state of one side of a folder pair.
type Endpoint struct {
	Exists       bool
	Validity     string    // validity token (IMAP UIDVALIDITY etc.)
	FullResyncAt time.Time // time of the last full rescan; zero = never
}

// CachedMsg - a parsed message in the state cache (without raw headers or body).
type CachedMsg struct {
	ID           string // opaque message ID on its own side
	MsgID        string // normalized Message-ID
	XHash        string
	Surrogate    string
	InternalDate time.Time
	Flags        []string
}

func encodeFlags(flags []string) string { return strings.Join(flags, " ") }

func decodeFlags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, " ")
}

// LoadEndpoint returns the stored endpoint state and an id->message map. If the
// endpoint has not been saved yet - Endpoint{Exists:false} and an empty map.
func (s *Store) LoadEndpoint(user, pair, side string) (Endpoint, map[string]CachedMsg, error) {
	var ep Endpoint
	var resyncAt int64
	err := s.db.QueryRow(
		`SELECT validity, full_resync_at FROM sync_endpoint WHERE user_name=? AND pair=? AND side=?`,
		user, pair, side).Scan(&ep.Validity, &resyncAt)
	if err == sql.ErrNoRows {
		return Endpoint{}, map[string]CachedMsg{}, nil
	}
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("reading sync_endpoint (%s/%s/%s): %w", user, pair, side, err)
	}
	ep.Exists = true
	if resyncAt > 0 {
		ep.FullResyncAt = time.Unix(0, resyncAt) // stored in nanoseconds
	}

	rows, err := s.db.Query(
		`SELECT msg_id, msgid, xhash, surrogate, internaldate, flags FROM sync_msg_cache
		 WHERE user_name=? AND pair=? AND side=?`, user, pair, side)
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("reading sync_msg_cache (%s/%s/%s): %w", user, pair, side, err)
	}
	defer rows.Close()

	msgs := make(map[string]CachedMsg)
	for rows.Next() {
		var m CachedMsg
		var idate int64
		var flags string
		if err := rows.Scan(&m.ID, &m.MsgID, &m.XHash, &m.Surrogate, &idate, &flags); err != nil {
			return Endpoint{}, nil, fmt.Errorf("parsing sync_msg_cache row: %w", err)
		}
		m.InternalDate = time.Unix(idate, 0)
		m.Flags = decodeFlags(flags)
		msgs[m.ID] = m
	}
	return ep, msgs, rows.Err()
}

// SaveEndpoint records the validity token and the time of the last full rescan.
// A zero fullResyncAt means "no rescan yet" and is stored as 0.
func (s *Store) SaveEndpoint(user, pair, side, validity string, fullResyncAt time.Time) error {
	var resyncAt int64
	if !fullResyncAt.IsZero() {
		resyncAt = fullResyncAt.UnixNano()
	}
	_, err := s.db.Exec(`
		INSERT INTO sync_endpoint (user_name, pair, side, validity, full_resync_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name, pair, side) DO UPDATE SET
			validity=excluded.validity, full_resync_at=excluded.full_resync_at, updated_at=excluded.updated_at`,
		user, pair, side, validity, resyncAt, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("writing sync_endpoint (%s/%s/%s): %w", user, pair, side, err)
	}
	return nil
}

// PutCachedMsgs inserts/replaces messages in the endpoint cache in one
// transaction.
func (s *Store) PutCachedMsgs(user, pair, side string, msgs []CachedMsg) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin cache tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(`
		INSERT INTO sync_msg_cache (user_name, pair, side, msg_id, msgid, xhash, surrogate, internaldate, flags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name, pair, side, msg_id) DO UPDATE SET
			msgid=excluded.msgid, xhash=excluded.xhash, surrogate=excluded.surrogate,
			internaldate=excluded.internaldate, flags=excluded.flags`)
	if err != nil {
		return fmt.Errorf("preparing cache stmt: %w", err)
	}
	defer stmt.Close()

	for _, m := range msgs {
		if _, err := stmt.Exec(user, pair, side, m.ID, m.MsgID, m.XHash, m.Surrogate,
			m.InternalDate.Unix(), encodeFlags(m.Flags)); err != nil {
			return fmt.Errorf("inserting message id=%s into cache: %w", m.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing cache: %w", err)
	}
	return nil
}

// DeleteCachedMsgs removes messages with the given IDs from the cache (e.g.
// deleted on the server).
func (s *Store) DeleteCachedMsgs(user, pair, side string, ids []string) error {
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]

		ph := strings.Repeat(",?", len(batch))[1:]
		args := make([]any, 0, len(batch)+3)
		args = append(args, user, pair, side)
		for _, id := range batch {
			args = append(args, id)
		}
		q := fmt.Sprintf(
			`DELETE FROM sync_msg_cache WHERE user_name=? AND pair=? AND side=? AND msg_id IN (%s)`, ph)
		if _, err := s.db.Exec(q, args...); err != nil {
			return fmt.Errorf("deleting messages from cache (%s/%s/%s): %w", user, pair, side, err)
		}
	}
	return nil
}

// ResetEndpoint fully resets the endpoint cache (on a validity change).
func (s *Store) ResetEndpoint(user, pair, side string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin endpoint-reset tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, q := range []string{
		`DELETE FROM sync_msg_cache WHERE user_name=? AND pair=? AND side=?`,
		`DELETE FROM sync_endpoint WHERE user_name=? AND pair=? AND side=?`,
	} {
		if _, err := tx.Exec(q, user, pair, side); err != nil {
			return fmt.Errorf("resetting endpoint (%s/%s/%s): %w", user, pair, side, err)
		}
	}
	return tx.Commit()
}

// PairKey builds a string key for a folder pair for the state tables.
func PairKey(folderA, folderB string) string {
	return folderA + "\x00" + folderB
}

// RunResult - the outcome of one sync run for a user.
type RunResult struct {
	At         time.Time
	Status     string // "ok" | "error"
	CopiedAToB int64
	CopiedBToA int64
	SkippedDup int64
	Errors     int64
	LastError  string
}

// UserStatus - a user's current state (last run + error streak).
type UserStatus struct {
	LastRun    time.Time
	LastOK     time.Time // time of the last run without errors
	Status     string    // "ok" | "error"
	CopiedAToB int64
	CopiedBToA int64
	SkippedDup int64
	Errors     int64
	LastError  string
	FailSince  time.Time // start of the current error streak; zero = currently ok
	FailStreak int64     // number of consecutive failed runs
}

// RecordRun records a run outcome: writes a row into the user_run history,
// updates the current user_status (including the error streak) and trims the
// history to userRunKeep most recent entries.
func (s *Store) RecordRun(user string, r RunResult) error {
	if r.Status == "" {
		r.Status = "ok"
	}
	at := r.At.Unix()
	failed := r.Status == "error"

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin user_run tx (%s): %w", user, err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`
		INSERT INTO user_run (user_name, run_at, status, copied_a_b, copied_b_a, skipped_dup, errors, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name, run_at) DO UPDATE SET
			status=excluded.status, copied_a_b=excluded.copied_a_b, copied_b_a=excluded.copied_b_a,
			skipped_dup=excluded.skipped_dup, errors=excluded.errors, last_error=excluded.last_error`,
		user, at, r.Status, r.CopiedAToB, r.CopiedBToA, r.SkippedDup, r.Errors, r.LastError); err != nil {
		return fmt.Errorf("writing user_run (%s): %w", user, err)
	}

	var lastOK, failSince, failStreak int64
	if !failed {
		lastOK = at
	} else {
		failSince = at // on the very first failed run; on later ones - COALESCE below
		failStreak = 1
	}
	if _, err := tx.Exec(`
		INSERT INTO user_status
			(user_name, last_run_at, last_ok_at, status, copied_a_b, copied_b_a, skipped_dup, errors, last_error, fail_since, fail_streak)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name) DO UPDATE SET
			last_run_at = excluded.last_run_at,
			last_ok_at  = CASE WHEN excluded.last_ok_at > 0 THEN excluded.last_ok_at ELSE user_status.last_ok_at END,
			status      = excluded.status,
			copied_a_b  = excluded.copied_a_b,
			copied_b_a  = excluded.copied_b_a,
			skipped_dup = excluded.skipped_dup,
			errors      = excluded.errors,
			last_error  = excluded.last_error,
			fail_since  = CASE WHEN excluded.status='error'
			                   THEN CASE WHEN user_status.fail_since>0 THEN user_status.fail_since ELSE excluded.last_run_at END
			                   ELSE 0 END,
			fail_streak = CASE WHEN excluded.status='error' THEN user_status.fail_streak + 1 ELSE 0 END`,
		user, at, lastOK, r.Status, r.CopiedAToB, r.CopiedBToA, r.SkippedDup, r.Errors, r.LastError, failSince, failStreak); err != nil {
		return fmt.Errorf("writing user_status (%s): %w", user, err)
	}

	// trim history
	if _, err := tx.Exec(`
		DELETE FROM user_run WHERE user_name=? AND run_at NOT IN (
			SELECT run_at FROM user_run WHERE user_name=? ORDER BY run_at DESC LIMIT ?
		)`, user, user, userRunKeep); err != nil {
		return fmt.Errorf("trimming user_run (%s): %w", user, err)
	}
	return tx.Commit()
}

func scanUserStatus(sc interface{ Scan(...any) error }) (string, UserStatus, error) {
	var name string
	var lastRun, lastOK, failSince int64
	var st UserStatus
	if err := sc.Scan(&name, &lastRun, &lastOK, &st.Status,
		&st.CopiedAToB, &st.CopiedBToA, &st.SkippedDup, &st.Errors, &st.LastError,
		&failSince, &st.FailStreak); err != nil {
		return "", UserStatus{}, err
	}
	st.LastRun = time.Unix(lastRun, 0)
	if lastOK > 0 {
		st.LastOK = time.Unix(lastOK, 0)
	}
	if failSince > 0 {
		st.FailSince = time.Unix(failSince, 0)
	}
	return name, st, nil
}

const userStatusCols = `user_name, last_run_at, last_ok_at, status,
	copied_a_b, copied_b_a, skipped_dup, errors, last_error, fail_since, fail_streak`

// UserStatuses returns the status of every user it has been recorded for.
func (s *Store) UserStatuses() (map[string]UserStatus, error) {
	rows, err := s.db.Query(`SELECT ` + userStatusCols + ` FROM user_status`)
	if err != nil {
		return nil, fmt.Errorf("reading user_status: %w", err)
	}
	defer rows.Close()

	out := make(map[string]UserStatus)
	for rows.Next() {
		name, st, err := scanUserStatus(rows)
		if err != nil {
			return nil, fmt.Errorf("parsing user_status row: %w", err)
		}
		out[name] = st
	}
	return out, rows.Err()
}

// UserStatusOf returns one user's status. The second value reports whether it
// was found.
func (s *Store) UserStatusOf(user string) (UserStatus, bool, error) {
	_, st, err := scanUserStatus(s.db.QueryRow(
		`SELECT `+userStatusCols+` FROM user_status WHERE user_name=?`, user))
	if err == sql.ErrNoRows {
		return UserStatus{}, false, nil
	}
	if err != nil {
		return UserStatus{}, false, fmt.Errorf("reading user_status (%s): %w", user, err)
	}
	return st, true, nil
}

// ResumeUser clears a user's error streak (fail_streak/fail_since) without
// touching the cache or history. It lifts the max_fail_streak stop.
func (s *Store) ResumeUser(user string) error {
	res, err := s.db.Exec(
		`UPDATE user_status SET fail_streak=0, fail_since=0 WHERE user_name=?`, strings.TrimSpace(user))
	if err != nil {
		return fmt.Errorf("clearing error streak (%s): %w", user, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user %q has no status (nothing to clear)", user)
	}
	return nil
}

// UserRuns returns a user's most recent runs (newest first).
func (s *Store) UserRuns(user string, limit int) ([]RunResult, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT run_at, status, copied_a_b, copied_b_a, skipped_dup, errors, last_error
		FROM user_run WHERE user_name=? ORDER BY run_at DESC LIMIT ?`, user, limit)
	if err != nil {
		return nil, fmt.Errorf("reading user_run (%s): %w", user, err)
	}
	defer rows.Close()

	var out []RunResult
	for rows.Next() {
		var r RunResult
		var at int64
		if err := rows.Scan(&at, &r.Status, &r.CopiedAToB, &r.CopiedBToA, &r.SkippedDup, &r.Errors, &r.LastError); err != nil {
			return nil, fmt.Errorf("parsing user_run row: %w", err)
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteUser removes a user from the users table.
func (s *Store) DeleteUser(name string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE name=?`, strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("deleting user %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user %q not found", name)
	}
	return nil
}

// DeleteFolderPair removes a folder pair.
func (s *Store) DeleteFolderPair(fp config.FolderPair) error {
	res, err := s.db.Exec(`DELETE FROM folder_pairs WHERE folder_a=? AND folder_b=?`,
		strings.TrimSpace(fp.A), strings.TrimSpace(fp.B))
	if err != nil {
		return fmt.Errorf("deleting folder pair %q/%q: %w", fp.A, fp.B, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("folder pair %q/%q not found", fp.A, fp.B)
	}
	return nil
}

// ForgetUser deletes all of a user's state (cache, status, history). The user
// itself stays in the users table - the next cycle starts syncing from scratch.
func (s *Store) ForgetUser(name string) error {
	name = strings.TrimSpace(name)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin forget tx (%s): %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{
		`DELETE FROM sync_msg_cache WHERE user_name=?`,
		`DELETE FROM sync_endpoint  WHERE user_name=?`,
		`DELETE FROM user_status    WHERE user_name=?`,
		`DELETE FROM user_run       WHERE user_name=?`,
	} {
		if _, err := tx.Exec(q, name); err != nil {
			return fmt.Errorf("clearing state for %q: %w", name, err)
		}
	}
	return tx.Commit()
}

// Vacuum compacts the DB file (after bulk deletes).
func (s *Store) Vacuum() error {
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("VACUUM: %w", err)
	}
	return nil
}
