package syncer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/endpoint/ewstest"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

func TestSyncUserEWSToMaildir(t *testing.T) {
	ews := ewstest.New(t, map[string]string{
		"ews-a": "Subject: с EWS\r\nFrom: a@b\r\nDate: Wed, 09 Sep 2026 12:00:00 +0000\r\n" +
			"Message-ID: <ews-side@corp>\r\n\r\nтело",
	})
	rootB := makeMaildir(t)
	seedMaildir(t, rootB, "с maildir", "mdir-side@corp")

	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		ServerA:        config.Server{Type: config.EndpointEWS, EWSUrl: ews.URL, MasterUser: "svc", MasterPass: "pw"},
		ServerB:        config.Server{Type: config.EndpointMaildir, Root: rootB},
		IOTimeout:      config.Duration(5 * time.Second),
		FetchBatchSize: 50,
		HashHeader:     "X-Imapsync-Hash",
		Folders:        []config.FolderPair{{A: "Sent", B: "INBOX"}},
		StateCache:     true,
		SQLitePath:     "unused",
	}
	usr := config.User{Name: "u", UserA: "ivanov@corp.ru", UserB: "u"}

	c1 := stats.New()
	c1.BeginCycle()
	NewWithState(cfg, c1, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c1.Snapshot(); r.Total.CopiedAToB != 1 || r.Total.CopiedBToA != 1 || r.Total.Errors != 0 {
		t.Fatalf("цикл 1: %+v (%+v)", r.Total, r.Users)
	}
	if ews.LastImp != "ivanov@corp.ru" {
		t.Errorf("имперсонация EWS: %q", ews.LastImp)
	}

	// на maildir теперь письмо из EWS + исходное maildir
	want := []string{"ews-side@corp", "mdir-side@corp"}
	if got := maildirMsgIDs(t, rootB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Maildir B = %v, ожидали %v", got, want)
	}
	// на EWS теперь 2 письма
	if ews.Count() != 2 {
		t.Errorf("на EWS %d писем, ожидали 2", ews.Count())
	}

	// второй цикл - идемпотентность
	c2 := stats.New()
	c2.BeginCycle()
	NewWithState(cfg, c2, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c2.Snapshot(); r.Total.CopiedAToB != 0 || r.Total.CopiedBToA != 0 {
		t.Errorf("цикл 2 скопировал лишнее: %+v", r.Total)
	}
}
