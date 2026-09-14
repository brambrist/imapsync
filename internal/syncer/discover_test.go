package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/endpoint"
	"imapsync/internal/mailbox"
	"imapsync/internal/stats"
)

func TestMatchFoldersSpecialUseBeatsName(t *testing.T) {
	// "Sent" on A and "Sent Items" on B share \Sent - matched despite the
	// different literal name. A decoy "Projects" pair (same name, no
	// SPECIAL-USE) makes sure the special-use pass does not also grab it.
	a := []endpoint.FolderInfo{
		{Name: "Sent", SpecialUse: `\Sent`, Selectable: true},
		{Name: "Projects", Selectable: true},
	}
	b := []endpoint.FolderInfo{
		{Name: "Sent Items", SpecialUse: `\Sent`, Selectable: true},
		{Name: "Projects", Selectable: true},
	}
	pairs, onlyA, onlyB := matchFolders(a, b)
	if len(onlyA) != 0 || len(onlyB) != 0 {
		t.Fatalf("expected everything matched, onlyA=%v onlyB=%v", onlyA, onlyB)
	}
	want := map[string]string{"Sent": "Sent Items", "Projects": "Projects"}
	if len(pairs) != 2 {
		t.Fatalf("pairs = %+v", pairs)
	}
	for _, p := range pairs {
		if want[p.A] != p.B {
			t.Errorf("pair %+v not expected", p)
		}
	}
}

func TestMatchFoldersByNameCaseInsensitive(t *testing.T) {
	a := []endpoint.FolderInfo{{Name: "Archive/2024", Selectable: true}}
	b := []endpoint.FolderInfo{{Name: "archive/2024", Selectable: true}}
	pairs, onlyA, onlyB := matchFolders(a, b)
	if len(pairs) != 1 || len(onlyA) != 0 || len(onlyB) != 0 {
		t.Fatalf("pairs=%+v onlyA=%v onlyB=%v", pairs, onlyA, onlyB)
	}
}

func TestMatchFoldersLeftovers(t *testing.T) {
	a := []endpoint.FolderInfo{{Name: "INBOX", Selectable: true}, {Name: "OnlyOnA", Selectable: true}}
	b := []endpoint.FolderInfo{{Name: "INBOX", Selectable: true}, {Name: "OnlyOnB", Selectable: true}}
	pairs, onlyA, onlyB := matchFolders(a, b)
	if len(pairs) != 1 || pairs[0].A != "INBOX" {
		t.Fatalf("pairs = %+v", pairs)
	}
	if len(onlyA) != 1 || onlyA[0].Name != "OnlyOnA" {
		t.Fatalf("onlyA = %+v", onlyA)
	}
	if len(onlyB) != 1 || onlyB[0].Name != "OnlyOnB" {
		t.Fatalf("onlyB = %+v", onlyB)
	}
}

func TestFilterFoldersExcludesAndNonSelectable(t *testing.T) {
	in := []endpoint.FolderInfo{
		{Name: "INBOX", Selectable: true},
		{Name: "Trash", Selectable: true},
		{Name: "Junk", SpecialUse: `\Junk`, Selectable: true},
		{Name: "[Gmail]", Selectable: false}, // structural-only
	}
	out := filterFolders(in, []string{"Trash", `\Junk`})
	if len(out) != 1 || out[0].Name != "INBOX" {
		t.Fatalf("filterFolders = %+v", out)
	}
}

func TestIsExcludedIgnoresBlankEntries(t *testing.T) {
	f := endpoint.FolderInfo{Name: "Sent", SpecialUse: `\Sent`}
	if isExcluded(f, []string{"", "  "}) {
		t.Error("blank exclude entries should not match anything")
	}
	if !isExcluded(f, []string{"sent"}) {
		t.Error("name match should be case-insensitive")
	}
}

// TestSyncUserAllFoldersDiscoversAndCreates proves sync_all_folders end to
// end: INBOX pairs by name automatically, and a folder that exists only on A
// (Sent) gets created on B and synced too - without an explicit folders list.
func TestSyncUserAllFoldersDiscoversAndCreates(t *testing.T) {
	rootA := makeMaildir(t)
	rootB := makeMaildir(t)
	seedMaildir(t, rootA, "inbox on A", "root-a@corp")
	seedMaildir(t, rootB, "inbox on B", "root-b@corp")

	sentA := filepath.Join(rootA, ".Sent")
	for _, d := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(sentA, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seedMaildir(t, sentA, "sent on A", "sent-a@corp")
	// B has no Sent folder at all yet - it must be created by the sync.

	cfg := &config.Config{
		ServerA:        config.Server{Type: config.EndpointMaildir, Root: rootA},
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		SyncAllFolders: true,
	}
	usr := config.User{Name: "u", UserA: "u", UserB: "u"}

	coll := stats.New()
	coll.BeginCycle()
	New(cfg, coll, t.Logf).SyncUser(context.Background(), usr)

	if r := coll.Snapshot(); r.Total.Errors != 0 {
		t.Fatalf("errors: %+v", r.Users)
	}

	if got := maildirMsgIDs(t, rootB); fmt.Sprint(got) != fmt.Sprint([]string{"root-a@corp", "root-b@corp"}) {
		t.Errorf("B INBOX = %v", got)
	}
	if fi, err := os.Stat(filepath.Join(rootB, ".Sent", "cur")); err != nil || !fi.IsDir() {
		t.Fatal("B's Sent folder should have been auto-created")
	}
	if got := maildirMsgIDs(t, sentA); fmt.Sprint(got) != fmt.Sprint([]string{"sent-a@corp"}) {
		t.Errorf("A Sent = %v (should be unchanged - nothing to copy back)", got)
	}
	bSent := filepath.Join(rootB, ".Sent")
	if got := maildirMsgIDs(t, bSent); fmt.Sprint(got) != fmt.Sprint([]string{"sent-a@corp"}) {
		t.Errorf("B Sent = %v, want [sent-a@corp]", got)
	}

	// idempotent second cycle: nothing new, no duplicate folder creation.
	c2 := stats.New()
	c2.BeginCycle()
	New(cfg, c2, t.Logf).SyncUser(context.Background(), usr)
	if r := c2.Snapshot(); r.Total.CopiedAToB != 0 || r.Total.CopiedBToA != 0 || r.Total.Errors != 0 {
		t.Errorf("cycle 2 should be a no-op: %+v", r.Total)
	}
}

// appendToFolder appends a message to an arbitrary IMAP folder (creating it
// first if it is not INBOX).
func appendToFolder(t *testing.T, srv config.Server, folder, subject, msgID string) {
	t.Helper()
	cl, err := mailbox.Connect(context.Background(), srv, "username", 5*time.Second, 5*time.Second, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Logout()
	if folder != "INBOX" {
		_ = cl.CreateMailbox(folder) // ignore "already exists"
	}
	body := fmt.Sprintf("From: a@example.org\r\nSubject: %s\r\nDate: Wed, 09 Sep 2026 12:00:00 +0000\r\n"+
		"Message-ID: <%s>\r\n\r\nbody", subject, msgID)
	if err := cl.Append(folder, nil, time.Now(), []byte(body)); err != nil {
		t.Fatal(err)
	}
}

// TestSyncUserAllFoldersIMAPToMaildir proves sync_all_folders across two
// different endpoint types: IMAP's "Sent" folder (name-matched, no
// counterpart on the maildir side yet) gets created on the maildir side and
// synced, same as the maildir<->maildir case above.
func TestSyncUserAllFoldersIMAPToMaildir(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	appendMsg(t, srvA, "inbox on A", "root-a@corp")
	appendToFolder(t, srvA, "Sent", "sent on A", "sent-a@corp")

	rootB := makeMaildir(t)
	seedMaildir(t, rootB, "inbox on B", "root-b@corp")

	cfg := &config.Config{
		ServerA:        srvA,
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		InsecureTLS:    true,
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		DialTimeout:    config.Duration(5 * time.Second),
		SyncAllFolders: true,
	}
	usr := config.User{Name: "u", UserA: "username", UserB: "u"}

	coll := stats.New()
	coll.BeginCycle()
	New(cfg, coll, t.Logf).SyncUser(context.Background(), usr)

	if r := coll.Snapshot(); r.Total.Errors != 0 {
		t.Fatalf("errors: %+v", r.Users)
	}
	// the memory backend seeds INBOX with one message of its own
	// ("0000000@localhost/") - present on both sides after the sync.
	want := []string{"0000000@localhost/", "root-a@corp", "root-b@corp"}
	if got := inboxMessageIDs(t, srvA); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("A INBOX = %v, want %v", got, want)
	}
	if got := maildirMsgIDs(t, rootB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("B INBOX = %v, want %v", got, want)
	}
	bSent := filepath.Join(rootB, ".Sent")
	if got := maildirMsgIDs(t, bSent); fmt.Sprint(got) != fmt.Sprint([]string{"sent-a@corp"}) {
		t.Errorf("B Sent (auto-created) = %v, want [sent-a@corp]", got)
	}
}
