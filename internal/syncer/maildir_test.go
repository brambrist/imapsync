package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/endpoint"
	"imapsync/internal/mailbox"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

func makeMaildir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Maildir")
	for _, s := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(root, s), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func seedMaildir(t *testing.T, root, subject, msgID string) {
	t.Helper()
	name := fmt.Sprintf("%d.seed.host", time.Now().UnixNano())
	body := fmt.Sprintf("Subject: %s\r\nFrom: a@b\r\nDate: Wed, 09 Sep 2026 12:00:00 +0000\r\n"+
		"Message-ID: <%s>\r\n\r\nbody", subject, msgID)
	if err := os.WriteFile(filepath.Join(root, "new", name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond) // keep filenames unique by time
}

func maildirMsgIDs(t *testing.T, root string) []string {
	t.Helper()
	b, err := endpoint.NewBackend(config.Server{Type: config.EndpointMaildir, Root: root}, 0, 0, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.Connect(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if _, _, err := ep.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	metas, err := ep.FetchMeta(nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range metas {
		f, err := mailbox.ParseFields(m.Header, "X-Imapsync-Hash")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.MessageID)
	}
	sort.Strings(ids)
	return ids
}

func TestSyncUserMaildirToMaildir(t *testing.T) {
	rootA := makeMaildir(t)
	rootB := makeMaildir(t)
	seedMaildir(t, rootA, "only on A", "only-a@corp")
	seedMaildir(t, rootB, "only on B", "only-b@corp")
	seedMaildir(t, rootA, "shared", "shared@corp")
	seedMaildir(t, rootB, "shared", "shared@corp")

	cfg := &config.Config{
		ServerA:        config.Server{Type: config.EndpointMaildir, Root: rootA},
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		Folders:        []config.FolderPair{{A: "INBOX", B: "INBOX"}},
	}

	coll := stats.New()
	coll.BeginCycle()
	New(cfg, coll, t.Logf).SyncUser(context.Background(), config.User{Name: "u", UserA: "u", UserB: "u"})

	want := []string{"only-a@corp", "only-b@corp", "shared@corp"}
	if got := maildirMsgIDs(t, rootA); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("A = %v, expected %v", got, want)
	}
	if got := maildirMsgIDs(t, rootB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("B = %v, expected %v", got, want)
	}

	rep := coll.Snapshot()
	if rep.Total.CopiedAToB != 1 || rep.Total.CopiedBToA != 1 || rep.Total.Errors != 0 {
		t.Errorf("counters: %+v (%+v)", rep.Total, rep.Users)
	}

	// idempotency
	c2 := stats.New()
	c2.BeginCycle()
	New(cfg, c2, t.Logf).SyncUser(context.Background(), config.User{Name: "u", UserA: "u", UserB: "u"})
	if r := c2.Snapshot(); r.Total.CopiedAToB != 0 || r.Total.CopiedBToA != 0 {
		t.Errorf("the second run copied extra: %+v", r.Total)
	}
}

func TestSyncUserIMAPToMaildir(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert) // IMAP: INBOX with its original message + our own
	appendMsg(t, srvA, "from IMAP", "imap-side@corp")
	rootB := makeMaildir(t) // Maildir
	seedMaildir(t, rootB, "from maildir", "mdir-side@corp")

	cfg := &config.Config{
		ServerA:        srvA,
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		InsecureTLS:    true,
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		DialTimeout:    config.Duration(5 * time.Second),
		Folders:        []config.FolderPair{{A: "INBOX", B: "INBOX"}},
	}

	coll := stats.New()
	coll.BeginCycle()
	New(cfg, coll, t.Logf).SyncUser(context.Background(), config.User{Name: "u", UserA: "username", UserB: "u"})

	if r := coll.Snapshot(); r.Total.Errors != 0 {
		t.Fatalf("errors during IMAP<->Maildir: %+v", r.Users)
	}
	// the maildir side now has the IMAP message (+ the original maildir one +
	// the transferred original IMAP message <0000000@localhost/>)
	want := []string{"0000000@localhost/", "imap-side@corp", "mdir-side@corp"}
	if got := maildirMsgIDs(t, rootB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Maildir B = %v, expected %v", got, want)
	}
	if got := inboxMessageIDs(t, srvA); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("IMAP A = %v, expected %v", got, want)
	}
}

func TestSyncUserMaildirIncremental(t *testing.T) {
	rootA := makeMaildir(t)
	rootB := makeMaildir(t)
	seedMaildir(t, rootA, "inc", "inc-a@corp")

	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		ServerA:        config.Server{Type: config.EndpointMaildir, Root: rootA},
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		Folders:        []config.FolderPair{{A: "INBOX", B: "INBOX"}},
		StateCache:     true,
		SQLitePath:     "unused",
	}
	usr := config.User{Name: "u", UserA: "u", UserB: "u"}

	c1 := stats.New()
	c1.BeginCycle()
	NewWithState(cfg, c1, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c1.Snapshot(); r.Total.CopiedAToB != 1 || r.Total.Errors != 0 {
		t.Fatalf("cycle 1: %+v", r.Total)
	}

	pair := store.PairKey("INBOX", "INBOX")
	ep, msgs, err := st.LoadEndpoint("u", pair, "a")
	if err != nil || !ep.Exists || len(msgs) != 1 {
		t.Fatalf("cache A: %+v msgs=%d err=%v", ep, len(msgs), err)
	}

	// add a message on B; the second cycle must pick it up
	seedMaildir(t, rootB, "new on B", "new-b@corp")
	c2 := stats.New()
	c2.BeginCycle()
	NewWithState(cfg, c2, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c2.Snapshot(); r.Total.CopiedBToA != 1 || r.Total.CopiedAToB != 0 {
		t.Errorf("cycle 2: %+v", r.Total)
	}
	if got := maildirMsgIDs(t, rootA); fmt.Sprint(got) != fmt.Sprint([]string{"inc-a@corp", "new-b@corp"}) {
		t.Errorf("A after cycle 2 = %v", got)
	}
}
