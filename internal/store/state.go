package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"imapsync/config"
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
    user_name    TEXT PRIMARY KEY,
    last_run_at  INTEGER NOT NULL,
    last_ok_at   INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL,          -- ok | error
    copied_a_b   INTEGER NOT NULL DEFAULT 0,
    copied_b_a   INTEGER NOT NULL DEFAULT 0,
    skipped_dup  INTEGER NOT NULL DEFAULT 0,
    errors       INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    fail_since   INTEGER NOT NULL DEFAULT 0,  -- начало текущей серии ошибок; 0 = сейчас ok
    fail_streak  INTEGER NOT NULL DEFAULT 0   -- сколько прогонов подряд с ошибкой
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

// userRunKeep - сколько последних прогонов хранить в user_run на юзера.
const userRunKeep = 200

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

// RunResult - итог одного прохода синка по пользователю.
type RunResult struct {
	At         time.Time
	Status     string // "ok" | "error"
	CopiedAToB int64
	CopiedBToA int64
	SkippedDup int64
	Errors     int64
	LastError  string
}

// UserStatus - текущее состояние пользователя (последний прогон + серия ошибок).
type UserStatus struct {
	LastRun    time.Time
	LastOK     time.Time // время последнего прохода без ошибок
	Status     string    // "ok" | "error"
	CopiedAToB int64
	CopiedBToA int64
	SkippedDup int64
	Errors     int64
	LastError  string
	FailSince  time.Time // начало текущей серии ошибок; нулевое = сейчас ok
	FailStreak int64     // сколько прогонов подряд завершились ошибкой
}

// RecordRun фиксирует итог прохода: пишет строку в историю user_run,
// обновляет текущий статус user_status (включая серию ошибок) и подрезает
// историю до userRunKeep последних записей.
func (s *Store) RecordRun(user string, r RunResult) error {
	if r.Status == "" {
		r.Status = "ok"
	}
	at := r.At.Unix()
	failed := r.Status == "error"

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx user_run (%s): %w", user, err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`
		INSERT INTO user_run (user_name, run_at, status, copied_a_b, copied_b_a, skipped_dup, errors, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_name, run_at) DO UPDATE SET
			status=excluded.status, copied_a_b=excluded.copied_a_b, copied_b_a=excluded.copied_b_a,
			skipped_dup=excluded.skipped_dup, errors=excluded.errors, last_error=excluded.last_error`,
		user, at, r.Status, r.CopiedAToB, r.CopiedBToA, r.SkippedDup, r.Errors, r.LastError); err != nil {
		return fmt.Errorf("запись user_run (%s): %w", user, err)
	}

	var lastOK, failSince, failStreak int64
	if !failed {
		lastOK = at
	} else {
		failSince = at // при первом же прогоне-ошибке; при последующих - COALESCE ниже
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
		return fmt.Errorf("запись user_status (%s): %w", user, err)
	}

	// подрезаем историю
	if _, err := tx.Exec(`
		DELETE FROM user_run WHERE user_name=? AND run_at NOT IN (
			SELECT run_at FROM user_run WHERE user_name=? ORDER BY run_at DESC LIMIT ?
		)`, user, user, userRunKeep); err != nil {
		return fmt.Errorf("чистка user_run (%s): %w", user, err)
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

// UserStatuses возвращает статусы всех пользователей, по которым он записан.
func (s *Store) UserStatuses() (map[string]UserStatus, error) {
	rows, err := s.db.Query(`SELECT ` + userStatusCols + ` FROM user_status`)
	if err != nil {
		return nil, fmt.Errorf("чтение user_status: %w", err)
	}
	defer rows.Close()

	out := make(map[string]UserStatus)
	for rows.Next() {
		name, st, err := scanUserStatus(rows)
		if err != nil {
			return nil, fmt.Errorf("разбор строки user_status: %w", err)
		}
		out[name] = st
	}
	return out, rows.Err()
}

// UserStatusOf возвращает статус одного пользователя. Второе значение - найден ли.
func (s *Store) UserStatusOf(user string) (UserStatus, bool, error) {
	_, st, err := scanUserStatus(s.db.QueryRow(
		`SELECT `+userStatusCols+` FROM user_status WHERE user_name=?`, user))
	if err == sql.ErrNoRows {
		return UserStatus{}, false, nil
	}
	if err != nil {
		return UserStatus{}, false, fmt.Errorf("чтение user_status (%s): %w", user, err)
	}
	return st, true, nil
}

// ResumeUser сбрасывает серию ошибок пользователя (fail_streak/fail_since),
// не трогая кэш и историю. Останавливает действие max_fail_streak.
func (s *Store) ResumeUser(user string) error {
	res, err := s.db.Exec(
		`UPDATE user_status SET fail_streak=0, fail_since=0 WHERE user_name=?`, strings.TrimSpace(user))
	if err != nil {
		return fmt.Errorf("сброс серии ошибок (%s): %w", user, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("по юзеру %q нет статуса (нечего сбрасывать)", user)
	}
	return nil
}

// UserRuns возвращает последние прогоны пользователя (новые первыми).
func (s *Store) UserRuns(user string, limit int) ([]RunResult, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT run_at, status, copied_a_b, copied_b_a, skipped_dup, errors, last_error
		FROM user_run WHERE user_name=? ORDER BY run_at DESC LIMIT ?`, user, limit)
	if err != nil {
		return nil, fmt.Errorf("чтение user_run (%s): %w", user, err)
	}
	defer rows.Close()

	var out []RunResult
	for rows.Next() {
		var r RunResult
		var at int64
		if err := rows.Scan(&at, &r.Status, &r.CopiedAToB, &r.CopiedBToA, &r.SkippedDup, &r.Errors, &r.LastError); err != nil {
			return nil, fmt.Errorf("разбор строки user_run: %w", err)
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteUser убирает пользователя из таблицы users.
func (s *Store) DeleteUser(name string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE name=?`, strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("удаление пользователя %q: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("пользователь %q не найден", name)
	}
	return nil
}

// DeleteFolderPair убирает пару папок.
func (s *Store) DeleteFolderPair(fp config.FolderPair) error {
	res, err := s.db.Exec(`DELETE FROM folder_pairs WHERE folder_a=? AND folder_b=?`,
		strings.TrimSpace(fp.A), strings.TrimSpace(fp.B))
	if err != nil {
		return fmt.Errorf("удаление пары папок %q/%q: %w", fp.A, fp.B, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("пара папок %q/%q не найдена", fp.A, fp.B)
	}
	return nil
}

// ForgetUser удаляет всё состояние пользователя (кэш, статус, история). Сам
// пользователь в таблице users остаётся - следующий цикл начнёт синк с нуля.
func (s *Store) ForgetUser(name string) error {
	name = strings.TrimSpace(name)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx forget (%s): %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{
		`DELETE FROM sync_msg_cache WHERE user_name=?`,
		`DELETE FROM sync_endpoint  WHERE user_name=?`,
		`DELETE FROM user_status    WHERE user_name=?`,
		`DELETE FROM user_run       WHERE user_name=?`,
	} {
		if _, err := tx.Exec(q, name); err != nil {
			return fmt.Errorf("очистка состояния %q: %w", name, err)
		}
	}
	return tx.Commit()
}

// Vacuum сжимает файл БД (после массовых удалений).
func (s *Store) Vacuum() error {
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("VACUUM: %w", err)
	}
	return nil
}
