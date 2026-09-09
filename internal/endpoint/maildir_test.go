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
	b, err := NewBackend(config.Server{Type: config.EndpointMaildir, Root: root}, 0, 0, false, 10)
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

	body := "Subject: привет\r\nFrom: a@b\r\nMessage-ID: <m1@corp>\r\n\r\nтело"
	when := time.Unix(1_700_000_000, 0)
	id, err := ep.Append([]string{`\Seen`, `\Flagged`}, when, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("Append не вернул ID")
	}

	ids, err := ep.ListIDs()
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("ListIDs => %v (err %v), ожидали [%s]", ids, err, id)
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
		t.Errorf("флаги не прочитались из имени файла: %v", m.Flags)
	}
	if !m.InternalDate.Equal(when) {
		t.Errorf("InternalDate = %v, ожидали %v (mtime)", m.InternalDate, when)
	}
	if !bytes.HasPrefix(m.Header, []byte("Subject: привет")) {
		t.Errorf("header: %q", m.Header)
	}

	lit, err := ep.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(lit)
	if string(raw) != body {
		t.Errorf("Open вернул не то тело:\n%q", raw)
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
			t.Errorf(".Sent/%s не создан", s)
		}
	}

	// SPECIAL-USE токен резолвится в ту же .Sent
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
		t.Errorf("письмо без флагов должно быть в new/, там %d файлов", len(ents))
	}
}

func TestFlagRoundTrip(t *testing.T) {
	if s := flagSuffix([]string{`\Seen`, `\Answered`, `\Draft`}); s != ":2,DRS" {
		t.Errorf("flagSuffix => %q, ожидали :2,DRS (сортировано)", s)
	}
	got := flagsFromName("123.host:2,FS")
	if fmtFlags(got) != `\Flagged \Seen` {
		t.Errorf("flagsFromName => %v", got)
	}
	if flagSuffix(nil) != "" {
		t.Error("пустые флаги => пустой суффикс")
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
			t.Errorf("expandUser(%q) = %q, ожидали %q", tmpl, got, want)
		}
	}
	if got := expandUser("/home/%n/M", "plainuser"); got != "/home/plainuser/M" {
		t.Errorf("без @: %q", got)
	}
}

func fmtFlags(fs []string) string { return strings.Join(fs, " ") }
