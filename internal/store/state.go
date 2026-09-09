package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// stateSchema - таблицы кэша инкрементальной сверки. Живут в том же файле, что и
// конфигурационные таблицы.
//
// pair - строковый ключ пары папок ("<folder_a>\x00<folder_b>"), side - 'a'|'b'.
const stateSchema = `
CREATE TABLE IF NOT EXISTS sync_endpoint (
    user_name      TEXT NOT NULL,
    pair           TEXT NOT NULL,
    side           TEXT NOT NULL,
    uidvalidity    INTEGER NOT NULL,
    full_resync_at INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (user_name, pair, side)
);
CREATE TABLE IF NOT EXISTS sync_msg_cache (
    user_name    TEXT NOT NULL,
    pair         TEXT NOT NULL,
    side         TEXT NOT NULL,
    uid          INTEGER NOT NULL,
    msgid        TEXT NOT NULL,
    xhash        TEXT NOT NULL,
    surrogate    TEXT NOT NULL,
    internaldate INTEGER NOT NULL,
    flags        TEXT NOT NULL,
    PRIMARY KEY (user_name, pair, side, uid)
);
CREATE TABLE IF NOT EXISTS user_status (
    user_name   TEXT PRIMARY KEY,
    last_run_at INTEGER NOT NULL,
    last_ok_at  INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL,          -- ok | error
    copied_a_b  INTEGER NOT NULL DEFAULT 0,
    copied_b_a  INTEGER NOT NULL DEFAULT 0,
    skipped_dup INTEGER NOT NULL DEFAULT 0,
    errors      INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT ''
);
`

// Endpoint - сохранённое состояние одной стороны пары папок.
type Endpoint struct {
	Exists       bool
	UIDValidity  uint32
	FullResyncAt time.Time // время последнего полного пере-скана; нулевое = не было
}

// CachedMsg - разобранное письмо в кэше состояния (без сырых заголовков и тела).
type CachedMsg struct {
	Uid          uint32
	MsgID        string
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

// LoadEndpoint возвращает сохранённое состояние эндпоинта и карту uid->письмо.
// Если эндпоинт ещё не сохранялся - Endpoint{Exists:false} и пустая карта.
func (s *Store) LoadEndpoint(user, pair, side string) (Endpoint, map[uint32]CachedMsg, error) {
	var ep Endpoint
	var resyncAt int64
	err := s.db.QueryRow(
		`SELECT uidvalidity, full_resync_at FROM sync_endpoint WHERE user_name=? AND pair=? AND side=?`,
		user, pair, side).Scan(&ep.UIDValidity, &resyncAt)
	if err == sql.ErrNoRows {
		return Endpoint{}, map[uint32]CachedMsg{}, nil
	}
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("чтение sync_endpoint (%s/%s/%s): %w", user, pair, side, err)
	}
	ep.Exists = true
	if resyncAt > 0 {
		ep.FullResyncAt = time.Unix(0, resyncAt) // хранится в наносекундах
	}

	rows, err := s.db.Query(
		`SELECT uid, msgid, xhash, surrogate, internaldate, flags FROM sync_msg_cache
		 WHERE user_name=? AND pair=? AND side=?`, user, pair, side)
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("чтение sync_msg_cache (%s/%s/%s): %w", user, pair, side, err)
	}
	defer rows.Close()

	msgs := make(map[uint32]CachedMsg)
	for rows.Next() {
		var m CachedMsg
		var idate int64
		var flags string
		if err := rows.Scan(&m.Uid, &m.MsgID, &m.XHash, &m.Surrogate, &idate, &flags); err != nil {
			return Endpoint{}, nil, fmt.Errorf("разбор строки sync_msg_cache: %w", err)
		}
		m.InternalDate = time.Unix(idate, 0)
		m.Flags = decodeFlags(flags)
		msgs[m.Uid] = m
	}
	return ep, msgs, rows.Err()
}

// SaveEndpoint фиксирует UIDVALIDITY и время последнего полного пере-скана.
// Нулевой fullResyncAt означает «пере-скан не делался» и записывается как 0.
func (s *Store) SaveEndpoint(user, pair, side string, uidvalidity uint32, fullResyncAt time.Time) error {
	var resyncAt int64
	if !fullResyncAt.IsZero() {
		resyncAt = fullResyncAt.UnixNano()
	}
	_, err := s.db.Exec(`
		INSERT INTO sync_endpoint (user_name, pair, side, uidvalidity, full_resync_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name, pair, side) DO UPDATE SET
			uidvalidity=excluded.uidvalidity, full_resync_at=excluded.full_resync_at, updated_at=excluded.updated_at`,
		user, pair, side, uidvalidity, resyncAt, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("запись sync_endpoint (%s/%s/%s): %w", user, pair, side, err)
	}
	return nil
}

// PutCachedMsgs добавляет/заменяет письма в кэше эндпоинта одной транзакцией.
func (s *Store) PutCachedMsgs(user, pair, side string, msgs []CachedMsg) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx кэша: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(`
		INSERT INTO sync_msg_cache (user_name, pair, side, uid, msgid, xhash, surrogate, internaldate, flags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name, pair, side, uid) DO UPDATE SET
			msgid=excluded.msgid, xhash=excluded.xhash, surrogate=excluded.surrogate,
			internaldate=excluded.internaldate, flags=excluded.flags`)
	if err != nil {
		return fmt.Errorf("prepare кэша: %w", err)
	}
	defer stmt.Close()

	for _, m := range msgs {
		if _, err := stmt.Exec(user, pair, side, m.Uid, m.MsgID, m.XHash, m.Surrogate,
			m.InternalDate.Unix(), encodeFlags(m.Flags)); err != nil {
			return fmt.Errorf("вставка письма uid=%d в кэш: %w", m.Uid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit кэша: %w", err)
	}
	return nil
}

// DeleteCachedMsgs убирает из кэша письма с указанными UID (например удалённые на
// сервере).
func (s *Store) DeleteCachedMsgs(user, pair, side string, uids []uint32) error {
	const chunk = 400
	for start := 0; start < len(uids); start += chunk {
		end := min(start+chunk, len(uids))
		batch := uids[start:end]

		ph := strings.Repeat(",?", len(batch))[1:]
		args := make([]any, 0, len(batch)+3)
		args = append(args, user, pair, side)
		for _, u := range batch {
			args = append(args, u)
		}
		q := fmt.Sprintf(
			`DELETE FROM sync_msg_cache WHERE user_name=? AND pair=? AND side=? AND uid IN (%s)`, ph)
		if _, err := s.db.Exec(q, args...); err != nil {
			return fmt.Errorf("удаление писем из кэша (%s/%s/%s): %w", user, pair, side, err)
		}
	}
	return nil
}

// ResetEndpoint полностью сбрасывает кэш эндпоинта (при смене UIDVALIDITY).
func (s *Store) ResetEndpoint(user, pair, side string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx сброса эндпоинта: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, q := range []string{
		`DELETE FROM sync_msg_cache WHERE user_name=? AND pair=? AND side=?`,
		`DELETE FROM sync_endpoint WHERE user_name=? AND pair=? AND side=?`,
	} {
		if _, err := tx.Exec(q, user, pair, side); err != nil {
			return fmt.Errorf("сброс эндпоинта (%s/%s/%s): %w", user, pair, side, err)
		}
	}
	return tx.Commit()
}

// PairKey строит строковый ключ пары папок для таблиц состояния.
func PairKey(folderA, folderB string) string {
	return folderA + "\x00" + folderB
}

// UserStatus - результат последнего прохода синка по пользователю.
type UserStatus struct {
	LastRun    time.Time
	LastOK     time.Time // время последнего прохода без ошибок; нулевое - не переписывать
	Status     string    // "ok" | "error"
	CopiedAToB int64
	CopiedBToA int64
	SkippedDup int64
	Errors     int64
	LastError  string
}

// SaveUserStatus пишет статус последнего прохода. last_ok_at обновляется только
// при успешном проходе (LastOK не нулевой), иначе прежнее значение сохраняется.
func (s *Store) SaveUserStatus(user string, st UserStatus) error {
	var lastOK int64
	if !st.LastOK.IsZero() {
		lastOK = st.LastOK.Unix()
	}
	_, err := s.db.Exec(`
		INSERT INTO user_status
			(user_name, last_run_at, last_ok_at, status, copied_a_b, copied_b_a, skipped_dup, errors, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name) DO UPDATE SET
			last_run_at = excluded.last_run_at,
			last_ok_at  = CASE WHEN excluded.last_ok_at > 0 THEN excluded.last_ok_at ELSE user_status.last_ok_at END,
			status      = excluded.status,
			copied_a_b  = excluded.copied_a_b,
			copied_b_a  = excluded.copied_b_a,
			skipped_dup = excluded.skipped_dup,
			errors      = excluded.errors,
			last_error  = excluded.last_error`,
		user, st.LastRun.Unix(), lastOK, st.Status,
		st.CopiedAToB, st.CopiedBToA, st.SkippedDup, st.Errors, st.LastError)
	if err != nil {
		return fmt.Errorf("запись user_status (%s): %w", user, err)
	}
	return nil
}

// UserStatuses возвращает статусы всех пользователей, по которым он записан.
func (s *Store) UserStatuses() (map[string]UserStatus, error) {
	rows, err := s.db.Query(`
		SELECT user_name, last_run_at, last_ok_at, status,
		       copied_a_b, copied_b_a, skipped_dup, errors, last_error
		FROM user_status`)
	if err != nil {
		return nil, fmt.Errorf("чтение user_status: %w", err)
	}
	defer rows.Close()

	out := make(map[string]UserStatus)
	for rows.Next() {
		var name string
		var lastRun, lastOK int64
		var st UserStatus
		if err := rows.Scan(&name, &lastRun, &lastOK, &st.Status,
			&st.CopiedAToB, &st.CopiedBToA, &st.SkippedDup, &st.Errors, &st.LastError); err != nil {
			return nil, fmt.Errorf("разбор строки user_status: %w", err)
		}
		st.LastRun = time.Unix(lastRun, 0)
		if lastOK > 0 {
			st.LastOK = time.Unix(lastOK, 0)
		}
		out[name] = st
	}
	return out, rows.Err()
}
