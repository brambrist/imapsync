package syncer

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"imapsync/config"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

// unpackPST decompresses the shared PST sample fixture into a temp file.
func unpackPST(t *testing.T) string {
	t.Helper()
	f, err := os.Open("../endpoint/testdata/support.pst.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	path := filepath.Join(t.TempDir(), "support.pst")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, gz); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSyncUserPSTToMaildir imports a PST "Sent Messages" folder into a Maildir.
// The PST is read-only, so only A->B copying happens regardless of direction.
func TestSyncUserPSTToMaildir(t *testing.T) {
	pstPath := unpackPST(t)
	rootB := makeMaildir(t)

	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		ServerA:        config.Server{Type: config.EndpointPST, Root: pstPath},
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		FetchBatchSize: 50,
		HashHeader:     "X-Imapsync-Hash",
		Folders:        []config.FolderPair{{A: "Sent Messages", B: "Sent"}},
		Direction:      config.DirectionAToB,
		StateCache:     true,
		SQLitePath:     "unused",
	}
	usr := config.User{Name: "u", UserA: "u@corp", UserB: "u"}

	c1 := stats.New()
	c1.BeginCycle()
	NewWithState(cfg, c1, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c1.Snapshot(); r.Total.CopiedAToB != 11 || r.Total.CopiedBToA != 0 || r.Total.Errors != 0 {
		t.Fatalf("cycle 1: %+v (%+v)", r.Total, r.Users)
	}

	// The Maildir .Sent subfolder now holds the 11 imported messages.
	entries, err := os.ReadDir(filepath.Join(rootB, ".Sent", "cur"))
	if err != nil {
		t.Fatal(err)
	}
	newDir, _ := os.ReadDir(filepath.Join(rootB, ".Sent", "new"))
	if got := len(entries) + len(newDir); got != 11 {
		t.Fatalf("Maildir .Sent has %d messages, want 11", got)
	}

	// Second cycle: nothing new, and the read-only PST is never written to.
	c2 := stats.New()
	c2.BeginCycle()
	NewWithState(cfg, c2, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c2.Snapshot(); r.Total.CopiedAToB != 0 || r.Total.CopiedBToA != 0 || r.Total.Errors != 0 {
		t.Fatalf("cycle 2 not idempotent: %+v (%+v)", r.Total, r.Users)
	}
}

// TestSyncUserPSTDirectionForced verifies that with direction "both" a read-only
// PST still only receives nothing and sources everything.
func TestSyncUserPSTDirectionForced(t *testing.T) {
	pstPath := unpackPST(t)
	rootB := makeMaildir(t)
	seedMaildir(t, rootB, "already on B", "b-only@corp")

	cfg := &config.Config{
		ServerA:        config.Server{Type: config.EndpointPST, Root: pstPath},
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		FetchBatchSize: 50,
		HashHeader:     "X-Imapsync-Hash",
		Folders:        []config.FolderPair{{A: "Drafts", B: "INBOX"}},
		Direction:      config.DirectionBoth,
	}
	usr := config.User{Name: "u", UserA: "u@corp", UserB: "u"}

	coll := stats.New()
	coll.BeginCycle()
	New(cfg, coll, t.Logf).SyncUser(context.Background(), usr)

	r := coll.Snapshot()
	if r.Total.CopiedBToA != 0 {
		t.Errorf("copied %d messages toward the read-only PST", r.Total.CopiedBToA)
	}
	if r.Total.CopiedAToB != 6 { // Drafts holds 6 messages
		t.Errorf("imported %d from the PST, want 6", r.Total.CopiedAToB)
	}
}
