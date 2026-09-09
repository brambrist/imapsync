package store

import (
	"testing"
	"time"
)

func TestEndpointCacheRoundTrip(t *testing.T) {
	st := openTemp(t)
	const user, pair, side = "ivanov", "Sent\x00Отправленные", "a"

	// первый вызов - пусто
	uidv, msgs, err := st.LoadEndpoint(user, pair, side)
	if err != nil || uidv != 0 || len(msgs) != 0 {
		t.Fatalf("пустой эндпоинт: uidv=%d msgs=%d err=%v", uidv, len(msgs), err)
	}

	now := time.Unix(1_700_000_000, 0)
	in := []CachedMsg{
		{Uid: 10, MsgID: "a@c", Surrogate: "h1", InternalDate: now, Flags: []string{`\Seen`}},
		{Uid: 11, MsgID: "", XHash: "h2", Surrogate: "h2", InternalDate: now},
	}
	if err := st.PutCachedMsgs(user, pair, side, in); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEndpoint(user, pair, side, 42); err != nil {
		t.Fatal(err)
	}

	uidv, msgs, err = st.LoadEndpoint(user, pair, side)
	if err != nil {
		t.Fatal(err)
	}
	if uidv != 42 || len(msgs) != 2 {
		t.Fatalf("после записи: uidv=%d msgs=%d", uidv, len(msgs))
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
	uidv, msgs, _ = st.LoadEndpoint(user, pair, side)
	if uidv != 0 || len(msgs) != 0 {
		t.Fatalf("после reset: uidv=%d msgs=%d", uidv, len(msgs))
	}
}

func TestUpsertCachedMsgUpdatesRow(t *testing.T) {
	st := openTemp(t)
	const user, pair, side = "u", "p", "b"
	_ = st.PutCachedMsgs(user, pair, side, []CachedMsg{{Uid: 1, MsgID: "old", Surrogate: "s"}})
	_ = st.PutCachedMsgs(user, pair, side, []CachedMsg{{Uid: 1, MsgID: "new", Surrogate: "s"}})
	_ = st.SaveEndpoint(user, pair, side, 1)

	_, msgs, _ := st.LoadEndpoint(user, pair, side)
	if len(msgs) != 1 || msgs[1].MsgID != "new" {
		t.Fatalf("upsert не обновил: %+v", msgs)
	}
}

func TestUserStatusPreservesLastOKOnError(t *testing.T) {
	st := openTemp(t)

	ok := UserStatus{LastRun: time.Unix(1000, 0), LastOK: time.Unix(1000, 0), Status: "ok", CopiedAToB: 5}
	if err := st.SaveUserStatus("u1", ok); err != nil {
		t.Fatal(err)
	}

	fail := UserStatus{LastRun: time.Unix(2000, 0), Status: "error", Errors: 3, LastError: "бах"}
	if err := st.SaveUserStatus("u1", fail); err != nil {
		t.Fatal(err)
	}

	all, err := st.UserStatuses()
	if err != nil {
		t.Fatal(err)
	}
	got := all["u1"]
	if got.Status != "error" || got.Errors != 3 || got.LastError != "бах" {
		t.Errorf("статус ошибки не записан: %+v", got)
	}
	if !got.LastOK.Equal(time.Unix(1000, 0)) {
		t.Errorf("last_ok затёрт при ошибке: %v", got.LastOK)
	}
	if !got.LastRun.Equal(time.Unix(2000, 0)) {
		t.Errorf("last_run не обновлён: %v", got.LastRun)
	}
}
