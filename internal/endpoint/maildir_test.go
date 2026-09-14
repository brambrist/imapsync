package endpoint

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"imapsync/config"
)

func newMaildir(t *testing.T) (Backend, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Maildir")
	for _, s := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(root, s), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b, err := NewBackend(config.Server{Type: config.EndpointMaildir, Root: root}, 0, 0, false, 10, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b, root
}

func TestMaildirAppendListFetchOpen(t *testing.T) {
	b, _ := newMaildir(t)
	ep, err := b.Connect(context.Background(), "ivanov@corp.ru")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()

	folder, validity, err := ep.Select("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if folder != "INBOX" || validity == "" {
		t.Errorf("Select => %q %q", folder, validity)
	}

	body := "Subject: hello\r\nFrom: a@b\r\nMessage-ID: <m1@corp>\r\n\r\nbody"
	when := time.Unix(1_700_000_000, 0)
	id, err := ep.Append([]string{`\Seen`, `\Flagged`}, when, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("Append returned no ID")
	}

	ids, err := ep.ListIDs()
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("ListIDs => %v (err %v), expected [%s]", ids, err, id)
	}

	metas, err := ep.FetchMeta(nil)
	if err != nil || len(metas) != 1 {
		t.Fatalf("FetchMeta => %d (err %v)", len(metas), err)
	}
	m := metas[0]
	if m.ID != id {
		t.Errorf("meta.ID = %q", m.ID)
	}
	if got := fmtFlags(m.Flags); got != `\Flagged \Seen` && got != `\Seen \Flagged` {
		t.Errorf("flags not read from the filename: %v", m.Flags)
	}
	if !m.InternalDate.Equal(when) {
		t.Errorf("InternalDate = %v, expected %v (mtime)", m.InternalDate, when)
	}
	if !bytes.HasPrefix(m.Header, []byte("Subject: hello")) {
		t.Errorf("header: %q", m.Header)
	}

	lit, err := ep.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(lit)
	if string(raw) != body {
		t.Errorf("Open returned the wrong body:\n%q", raw)
	}
}

func TestMaildirSelectCreatesSubfolder(t *testing.T) {
	b, root := newMaildir(t)
	ep, err := b.Connect(context.Background(), "u@d")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()

	folder, _, err := ep.Select("Sent")
	if err != nil {
		t.Fatal(err)
	}
	if folder != "Sent" {
		t.Errorf("folder = %q", folder)
	}
	for _, s := range []string{"tmp", "new", "cur"} {
		if !isDir(filepath.Join(root, ".Sent", s)) {
			t.Errorf(".Sent/%s not created", s)
		}
	}

	// the SPECIAL-USE token resolves to the same .Sent
	f2, _, err := ep.Select(`\Sent`)
	if err != nil || f2 != "Sent" {
		t.Errorf(`Select("\\Sent") => %q %v`, f2, err)
	}
}

func TestMaildirNoFlagsGoesToNew(t *testing.T) {
	b, root := newMaildir(t)
	ep, _ := b.Connect(context.Background(), "u@d")
	defer ep.Close()
	ep.Select("INBOX")

	if _, err := ep.Append(nil, time.Time{}, bytes.NewBufferString("Subject: x\r\n\r\nb")); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "new"))
	if len(ents) != 1 {
		t.Errorf("a message without flags must be in new/, found %d files there", len(ents))
	}
}

func TestFlagRoundTrip(t *testing.T) {
	if s := flagSuffix([]string{`\Seen`, `\Answered`, `\Draft`}); s != ":2,DRS" {
		t.Errorf("flagSuffix => %q, expected :2,DRS (sorted)", s)
	}
	got := flagsFromName("123.host:2,FS")
	if fmtFlags(got) != `\Flagged \Seen` {
		t.Errorf("flagsFromName => %v", got)
	}
	if flagSuffix(nil) != "" {
		t.Error("empty flags => empty suffix")
	}
}

func TestExpandUser(t *testing.T) {
	cases := map[string]string{
		"/m/%u/Maildir":    "/m/ivanov@corp.ru/Maildir",
		"/home/%n/Maildir": "/home/ivanov/Maildir",
		"/srv/%d/%n":       "/srv/corp.ru/ivanov",
		"/no/placeholders": "/no/placeholders",
	}
	for tmpl, want := range cases {
		if got := expandUser(tmpl, "ivanov@corp.ru"); got != want {
			t.Errorf("expandUser(%q) = %q, expected %q", tmpl, got, want)
		}
	}
	if got := expandUser("/home/%n/M", "plainuser"); got != "/home/plainuser/M" {
		t.Errorf("without @: %q", got)
	}
}

func fmtFlags(fs []string) string { return strings.Join(fs, " ") }

func TestMaildirListFoldersAndCreate(t *testing.T) {
	b, root := newMaildir(t)
	ep, err := b.Connect(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()

	lister, ok := ep.(FolderLister)
	if !ok {
		t.Fatal("maildirEndpoint should implement FolderLister")
	}
	creator, ok := ep.(FolderCreator)
	if !ok {
		t.Fatal("maildirEndpoint should implement FolderCreator")
	}

	// pre-existing SPECIAL-USE-mapped and plain nested subfolders.
	for _, sub := range []string{".Sent", ".Archive.2024"} {
		for _, d := range []string{"tmp", "new", "cur"} {
			if err := os.MkdirAll(filepath.Join(root, sub, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}

	folders, err := lister.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]FolderInfo{}
	for _, f := range folders {
		byName[f.Name] = f
	}
	if _, ok := byName["INBOX"]; !ok {
		t.Errorf("INBOX missing: %+v", folders)
	}
	if got := byName["Sent"].SpecialUse; got != `\Sent` {
		t.Errorf("Sent SpecialUse = %q, want \\Sent", got)
	}
	if _, ok := byName["Archive/2024"]; !ok {
		t.Errorf("nested Archive/2024 (canonical, from Maildir++ .Archive.2024) missing: %+v", folders)
	}

	// create a new nested folder and confirm it shows up.
	if err := creator.CreateFolder("Projects/Q1"); err != nil {
		t.Fatal(err)
	}
	if !isDir(filepath.Join(root, ".Projects.Q1", "cur")) {
		t.Fatal("CreateFolder did not create the expected Maildir++ directory")
	}
	folders, err = lister.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range folders {
		if f.Name == "Projects/Q1" {
			found = true
		}
	}
	if !found {
		t.Errorf("Projects/Q1 not listed after CreateFolder: %+v", folders)
	}

	// selecting the newly created folder should work end to end too.
	if folder, _, err := ep.Select("Projects/Q1"); err != nil || folder != "Projects/Q1" {
		t.Errorf("Select(Projects/Q1) => %q %v", folder, err)
	}
}
