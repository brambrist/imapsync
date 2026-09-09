package store

import (
	"testing"
	"time"

	"imapsync/config"
)

func TestEndpointCacheRoundTrip(t *testing.T) {
	st := openTemp(t)
	const user, pair, side = "ivanov", "Sent\x00Отправленные", "a"

	// первый вызов - пусто
	ep, msgs, err := st.LoadEndpoint(user, pair, side)
	if err != nil || ep.Exists || len(msgs) != 0 {
		t.Fatalf("пустой эндпоинт: %+v msgs=%d err=%v", ep, len(msgs), err)
	}

	now := time.Unix(1_700_000_000, 0)
	in := []CachedMsg{
		{Uid: 10, MsgID: "a@c", Surrogate: "h1", InternalDate: now, Flags: []string{`\Seen`}},
		{Uid: 11, MsgID: "", XHash: "h2", Surrogate: "h2", InternalDate: now},
	}
	if err := st.PutCachedMsgs(user, pair, side, in); err != nil {
		t.Fatal(err)
	}
	resync := time.Unix(1_700_000_500, 0)
	if err := st.SaveEndpoint(user, pair, side, 42, resync); err != nil {
		t.Fatal(err)
	}

	ep, msgs, err = st.LoadEndpoint(user, pair, side)
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Exists || ep.UIDValidity != 42 || len(msgs) != 2 {
		t.Fatalf("после записи: %+v msgs=%d", ep, len(msgs))
	}
	if !ep.FullResyncAt.Equal(resync) {
		t.Errorf("full_resync_at: %v != %v", ep.FullResyncAt, resync)
	}
	if got := msgs[10]; got.MsgID != "a@c" || len(got.Flags) != 1 || got.Flags[0] != `\Seen` {
		t.Errorf("uid 10: %+v", got)
	}
	if !msgs[10].InternalDate.Equal(now) {
		t.Errorf("uid 10 дата: %v != %v", msgs[10].InternalDate, now)
	}

	// удаление
	if err := st.DeleteCachedMsgs(user, pair, side, []uint32{10}); err != nil {
		t.Fatal(err)
	}
	_, msgs, _ = st.LoadEndpoint(user, pair, side)
	if len(msgs) != 1 || msgs[11].XHash != "h2" {
		t.Fatalf("после удаления: %+v", msgs)
	}

	// reset
	if err := st.ResetEndpoint(user, pair, side); err != nil {
		t.Fatal(err)
	}
	ep, msgs, _ = st.LoadEndpoint(user, pair, side)
	if ep.Exists || len(msgs) != 0 {
		t.Fatalf("после reset: %+v msgs=%d", ep, len(msgs))
	}
}

func TestUpsertCachedMsgUpdatesRow(t *testing.T) {
	st := openTemp(t)
	const user, pair, side = "u", "p", "b"
	_ = st.PutCachedMsgs(user, pair, side, []CachedMsg{{Uid: 1, MsgID: "old", Surrogate: "s"}})
	_ = st.PutCachedMsgs(user, pair, side, []CachedMsg{{Uid: 1, MsgID: "new", Surrogate: "s"}})
	_ = st.SaveEndpoint(user, pair, side, 1, time.Time{})

	_, msgs, _ := st.LoadEndpoint(user, pair, side)
	if len(msgs) != 1 || msgs[1].MsgID != "new" {
		t.Fatalf("upsert не обновил: %+v", msgs)
	}
}

func TestOpenExclusiveBlocksSecondProcess(t *testing.T) {
	path := t.TempDir() + "/x.db"
	st1, err := OpenExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExclusive(path); err == nil {
		t.Fatal("второй OpenExclusive должен был не пройти")
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}
	// после закрытия - снова можно
	st2, err := OpenExclusive(path)
	if err != nil {
		t.Fatalf("после Close блокировка не снялась: %v", err)
	}
	st2.Close()
}

func TestRecordRunStreaksAndHistory(t *testing.T) {
	st := openTemp(t)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(st.RecordRun("u1", RunResult{At: time.Unix(1000, 0), Status: "ok", CopiedAToB: 5}))
	must(st.RecordRun("u1", RunResult{At: time.Unix(2000, 0), Status: "error", Errors: 3, LastError: "бах-1"}))
	must(st.RecordRun("u1", RunResult{At: time.Unix(3000, 0), Status: "error", Errors: 1, LastError: "бах-2"}))

	got := mustStatus(t, st, "u1")
	if got.Status != "error" || got.LastError != "бах-2" {
		t.Errorf("текущий статус: %+v", got)
	}
	if got.FailStreak != 2 {
		t.Errorf("fail_streak = %d, ожидали 2", got.FailStreak)
	}
	if !got.FailSince.Equal(time.Unix(2000, 0)) {
		t.Errorf("fail_since = %v, ожидали момент первой ошибки (2000)", got.FailSince)
	}
	if !got.LastOK.Equal(time.Unix(1000, 0)) {
		t.Errorf("last_ok затёрт: %v", got.LastOK)
	}

	// восстановление
	must(st.RecordRun("u1", RunResult{At: time.Unix(4000, 0), Status: "ok", CopiedBToA: 2}))
	got = mustStatus(t, st, "u1")
	if got.Status != "ok" || got.FailStreak != 0 || !got.FailSince.IsZero() {
		t.Errorf("после восстановления: %+v", got)
	}
	if !got.LastOK.Equal(time.Unix(4000, 0)) {
		t.Errorf("last_ok не обновлён: %v", got.LastOK)
	}

	// история
	runs, err := st.UserRuns("u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 || !runs[0].At.Equal(time.Unix(4000, 0)) || !runs[3].At.Equal(time.Unix(1000, 0)) {
		t.Errorf("история: %+v", runs)
	}
}

func TestUserRunRetention(t *testing.T) {
	st := openTemp(t)
	for i := 1; i <= userRunKeep+25; i++ {
		if err := st.RecordRun("u", RunResult{At: time.Unix(int64(i)*60, 0), Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := st.UserRuns("u", userRunKeep*2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != userRunKeep {
		t.Errorf("после ретенции хранится %d прогонов, ожидали %d", len(runs), userRunKeep)
	}
}

func TestDeleteAndForget(t *testing.T) {
	st := openTemp(t)
	_ = st.UpsertUser(config.User{Name: "u1", UserA: "a", UserB: "b"}, true)
	_ = st.UpsertFolderPair(config.FolderPair{A: "Sent", B: "S"})
	_ = st.PutCachedMsgs("u1", "Sent\x00S", "a", []CachedMsg{{Uid: 1, Surrogate: "x"}})
	_ = st.SaveEndpoint("u1", "Sent\x00S", "a", 1, time.Time{})
	_ = st.RecordRun("u1", RunResult{At: time.Unix(1, 0), Status: "ok"})

	if err := st.ForgetUser("u1"); err != nil {
		t.Fatal(err)
	}
	ep, msgs, _ := st.LoadEndpoint("u1", "Sent\x00S", "a")
	if ep.Exists || len(msgs) != 0 {
		t.Errorf("ForgetUser не очистил состояние: %+v", ep)
	}
	if runs, _ := st.UserRuns("u1", 10); len(runs) != 0 {
		t.Errorf("ForgetUser не очистил историю: %+v", runs)
	}
	// сам юзер остался
	if us, _ := st.ListUsers(); len(us) != 1 {
		t.Errorf("ForgetUser не должен удалять из users: %+v", us)
	}

	if err := st.DeleteUser("u1"); err != nil {
		t.Fatal(err)
	}
	if us, _ := st.AllUsers(); len(us) != 0 {
		t.Errorf("DeleteUser не сработал: %+v", us)
	}
	if err := st.DeleteFolderPair(config.FolderPair{A: "Sent", B: "S"}); err != nil {
		t.Fatal(err)
	}
	if fps, _ := st.ListFolderPairs(); len(fps) != 0 {
		t.Errorf("DeleteFolderPair не сработал: %+v", fps)
	}
	if err := st.DeleteUser("missing"); err == nil {
		t.Error("DeleteUser несуществующего должен вернуть ошибку")
	}
}

func mustStatus(t *testing.T, st *Store, name string) UserStatus {
	t.Helper()
	all, err := st.UserStatuses()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := all[name]
	if !ok {
		t.Fatalf("нет статуса для %q", name)
	}
	return s
}
