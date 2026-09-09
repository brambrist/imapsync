package store

import (
	"path/filepath"
	"testing"

	"imapsync/config"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUpsertAndListUsers(t *testing.T) {
	st := openTemp(t)

	if err := st.UpsertUser(config.User{Name: "ivanov", UserA: "i@a", UserB: "i@b"}, true); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(config.User{Name: "petrov", UserA: "p@a", UserB: "p@b"}, false); err != nil {
		t.Fatal(err)
	}
	// a second upsert updates
	if err := st.UpsertUser(config.User{Name: "ivanov", UserA: "i2@a", UserB: "i2@b"}, true); err != nil {
		t.Fatal(err)
	}

	enabled, err := st.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0].Name != "ivanov" || enabled[0].UserA != "i2@a" {
		t.Fatalf("ListUsers = %+v", enabled)
	}

	all, err := st.AllUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("AllUsers = %+v", all)
	}
}

func TestFolderPairsNoDup(t *testing.T) {
	st := openTemp(t)
	for range 3 {
		if err := st.UpsertFolderPair(config.FolderPair{A: "Sent", B: "Sent Items"}); err != nil {
			t.Fatal(err)
		}
	}
	fps, err := st.ListFolderPairs()
	if err != nil {
		t.Fatal(err)
	}
	if len(fps) != 1 {
		t.Fatalf("expected 1 pair, got %+v", fps)
	}
}

func TestSetUserEnabled(t *testing.T) {
	st := openTemp(t)
	_ = st.UpsertUser(config.User{Name: "u", UserA: "a", UserB: "b"}, false)
	if err := st.SetUserEnabled("u", true); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListUsers()
	if len(got) != 1 {
		t.Fatalf("user was not enabled: %+v", got)
	}
	if err := st.SetUserEnabled("missing", true); err == nil {
		t.Error("expected an error for a missing user")
	}
}

func TestLoadIntoAndImportConfig(t *testing.T) {
	src := openTemp(t)
	cfg := &config.Config{
		Folders: []config.FolderPair{{A: "Sent", B: "S2"}, {A: "Drafts", B: "D2"}},
		Users: []config.User{
			{Name: "a", UserA: "a@a", UserB: "a@b"},
			{Name: "b", UserA: "b@a", UserB: "b@b"},
		},
	}
	nf, nu, err := src.ImportConfig(cfg)
	if err != nil || nf != 2 || nu != 2 {
		t.Fatalf("ImportConfig: nf=%d nu=%d err=%v", nf, nu, err)
	}

	var got config.Config
	if err := src.LoadInto(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Users) != 2 || len(got.Folders) != 2 {
		t.Fatalf("LoadInto: users=%d folders=%d", len(got.Users), len(got.Folders))
	}
}

func TestRejectsEmptyFields(t *testing.T) {
	st := openTemp(t)
	if err := st.UpsertUser(config.User{Name: "x", UserA: "", UserB: "b"}, true); err == nil {
		t.Error("expected an error on empty user_a")
	}
	if err := st.UpsertFolderPair(config.FolderPair{A: "  ", B: "b"}); err == nil {
		t.Error("expected an error on empty folder name")
	}
}
