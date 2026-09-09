package store

import (
	"testing"
	"time"

	"imapsync/config"
)

func TestEndpointCacheRoundTrip(t *testing.T) {
	st := openTemp(t)
	const user, pair, side = "ivanov", "Sent\x00Sent Items", "a"

	// first call - empty
	ep, msgs, err := st.LoadEndpoint(user, pair, side)
	if err != nil || ep.Exists || len(msgs) != 0 {
		t.Fatalf("empty endpoint: %+v msgs=%d err=%v", ep, len(msgs), err)
	}

	now := time.Unix(1_700_000_000, 0)
	in := []CachedMsg{
		{ID: "10", MsgID: "a@c", Surrogate: "h1", InternalDate: now, Flags: []string{`\Seen`}},
		{ID: "11", MsgID: "", XHash: "h2", Surrogate: "h2", InternalDate: now},
	}
	if err := st.PutCachedMsgs(user, pair, side, in); err != nil {
		t.Fatal(err)
	}
	resync := time.Unix(1_700_000_500, 0)
	if err := st.SaveEndpoint(user, pair, side, "42", resync); err != nil {
		t.Fatal(err)
	}

	ep, msgs, err = st.LoadEndpoint(user, pair, side)
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Exists || ep.Validity != "42" || len(msgs) != 2 {
		t.Fatalf("after write: %+v msgs=%d", ep, len(msgs))
	}
	if !ep.FullResyncAt.Equal(resync) {
		t.Errorf("full_resync_at: %v != %v", ep.FullResyncAt, resync)
	}
	if got := msgs["10"]; got.MsgID != "a@c" || len(got.Flags) != 1 || got.Flags[0] != `\Seen` {
		t.Errorf("msg 10: %+v", got)
	}
	if !msgs["10"].InternalDate.Equal(now) {
		t.Errorf("msg 10 date: %v != %v", msgs["10"].InternalDate, now)
	}

	// delete
	if err := st.DeleteCachedMsgs(user, pair, side, []string{"10"}); err != nil {
		t.Fatal(err)
	}
	_, msgs, _ = st.LoadEndpoint(user, pair, side)
	if len(msgs) != 1 || msgs["11"].XHash != "h2" {
		t.Fatalf("after delete: %+v", msgs)
	}

	// reset
	if err := st.ResetEndpoint(user, pair, side); err != nil {
		t.Fatal(err)
	}
	ep, msgs, _ = st.LoadEndpoint(user, pair, side)
	if ep.Exists || len(msgs) != 0 {
		t.Fatalf("after reset: %+v msgs=%d", ep, len(msgs))
	}
}

func TestUpsertCachedMsgUpdatesRow(t *testing.T) {
	st := openTemp(t)
	const user, pair, side = "u", "p", "b"
	_ = st.PutCachedMsgs(user, pair, side, []CachedMsg{{ID: "1", MsgID: "old", Surrogate: "s"}})
	_ = st.PutCachedMsgs(user, pair, side, []CachedMsg{{ID: "1", MsgID: "new", Surrogate: "s"}})
	_ = st.SaveEndpoint(user, pair, side, "1", time.Time{})

	_, msgs, _ := st.LoadEndpoint(user, pair, side)
	if len(msgs) != 1 || msgs["1"].MsgID != "new" {
		t.Fatalf("upsert did not update: %+v", msgs)
	}
}

func TestOpenExclusiveBlocksSecondProcess(t *testing.T) {
	path := t.TempDir() + "/x.db"
	st1, err := OpenExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExclusive(path); err == nil {
		t.Fatal("the second OpenExclusive should have failed")
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}
	// after close - possible again
	st2, err := OpenExclusive(path)
	if err != nil {
		t.Fatalf("lock not released after Close: %v", err)
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
	must(st.RecordRun("u1", RunResult{At: time.Unix(2000, 0), Status: "error", Errors: 3, LastError: "boom-1"}))
	must(st.RecordRun("u1", RunResult{At: time.Unix(3000, 0), Status: "error", Errors: 1, LastError: "boom-2"}))

	got := mustStatus(t, st, "u1")
	if got.Status != "error" || got.LastError != "boom-2" {
		t.Errorf("current status: %+v", got)
	}
	if got.FailStreak != 2 {
		t.Errorf("fail_streak = %d, expected 2", got.FailStreak)
	}
	if !got.FailSince.Equal(time.Unix(2000, 0)) {
		t.Errorf("fail_since = %v, expected the time of the first error (2000)", got.FailSince)
	}
	if !got.LastOK.Equal(time.Unix(1000, 0)) {
		t.Errorf("last_ok overwritten: %v", got.LastOK)
	}

	// recovery
	must(st.RecordRun("u1", RunResult{At: time.Unix(4000, 0), Status: "ok", CopiedBToA: 2}))
	got = mustStatus(t, st, "u1")
	if got.Status != "ok" || got.FailStreak != 0 || !got.FailSince.IsZero() {
		t.Errorf("after recovery: %+v", got)
	}
	if !got.LastOK.Equal(time.Unix(4000, 0)) {
		t.Errorf("last_ok not updated: %v", got.LastOK)
	}

	// history
	runs, err := st.UserRuns("u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 || !runs[0].At.Equal(time.Unix(4000, 0)) || !runs[3].At.Equal(time.Unix(1000, 0)) {
		t.Errorf("history: %+v", runs)
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
		t.Errorf("after retention %d runs kept, expected %d", len(runs), userRunKeep)
	}
}

func TestDeleteAndForget(t *testing.T) {
	st := openTemp(t)
	_ = st.UpsertUser(config.User{Name: "u1", UserA: "a", UserB: "b"}, true)
	_ = st.UpsertFolderPair(config.FolderPair{A: "Sent", B: "S"})
	_ = st.PutCachedMsgs("u1", "Sent\x00S", "a", []CachedMsg{{ID: "1", Surrogate: "x"}})
	_ = st.SaveEndpoint("u1", "Sent\x00S", "a", "1", time.Time{})
	_ = st.RecordRun("u1", RunResult{At: time.Unix(1, 0), Status: "ok"})

	if err := st.ForgetUser("u1"); err != nil {
		t.Fatal(err)
	}
	ep, msgs, _ := st.LoadEndpoint("u1", "Sent\x00S", "a")
	if ep.Exists || len(msgs) != 0 {
		t.Errorf("ForgetUser did not clear the state: %+v", ep)
	}
	if runs, _ := st.UserRuns("u1", 10); len(runs) != 0 {
		t.Errorf("ForgetUser did not clear the history: %+v", runs)
	}
	// the user itself remains
	if us, _ := st.ListUsers(); len(us) != 1 {
		t.Errorf("ForgetUser must not delete from users: %+v", us)
	}

	if err := st.DeleteUser("u1"); err != nil {
		t.Fatal(err)
	}
	if us, _ := st.AllUsers(); len(us) != 0 {
		t.Errorf("DeleteUser did not work: %+v", us)
	}
	if err := st.DeleteFolderPair(config.FolderPair{A: "Sent", B: "S"}); err != nil {
		t.Fatal(err)
	}
	if fps, _ := st.ListFolderPairs(); len(fps) != 0 {
		t.Errorf("DeleteFolderPair did not work: %+v", fps)
	}
	if err := st.DeleteUser("missing"); err == nil {
		t.Error("DeleteUser of a missing user must return an error")
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
		t.Fatalf("no status for %q", name)
	}
	return s
}
