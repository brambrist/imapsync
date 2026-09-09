package endpoint

import (
	"bytes"
	"context"
	"io"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/endpoint/ewstest"
)

func ewsBackendFor(t *testing.T, url string) Backend {
	t.Helper()
	b, err := NewBackend(config.Server{Type: config.EndpointEWS, EWSUrl: url, MasterUser: "svc", MasterPass: "pw"},
		0, 5*time.Second, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const ewsMsg = "Subject: hello\r\nFrom: a@b\r\nDate: Wed, 09 Sep 2026 12:00:00 +0000\r\n" +
	"Message-ID: <ews1@corp>\r\nX-Test-Read: yes\r\n\r\nbody"

func TestEWSListFetchOpen(t *testing.T) {
	srv := ewstest.New(t, map[string]string{"i-1": ewsMsg})
	ep, err := ewsBackendFor(t, srv.URL).Connect(context.Background(), "ivanov@corp.ru")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()

	folder, validity, err := ep.Select("Sent")
	if err != nil || folder != "Sent" || validity != "ews" {
		t.Fatalf("Select => %q %q %v", folder, validity, err)
	}

	ids, err := ep.ListIDs()
	if err != nil || len(ids) != 1 || ids[0] != "i-1" {
		t.Fatalf("ListIDs => %v %v", ids, err)
	}

	metas, err := ep.FetchMeta(nil)
	if err != nil || len(metas) != 1 {
		t.Fatalf("FetchMeta => %d %v", len(metas), err)
	}
	m := metas[0]
	if m.ID != "i-1" {
		t.Errorf("meta.ID = %q", m.ID)
	}
	if !slices.Contains(m.Flags, `\Seen`) {
		t.Errorf("IsRead=true did not yield \\Seen: %v", m.Flags)
	}
	hm, _ := mail.ReadMessage(bytes.NewReader(m.Header))
	if hm.Header.Get("Message-Id") != "<ews1@corp>" {
		t.Errorf("Message-ID not reconstructed: %q", hm.Header.Get("Message-Id"))
	}

	lit, err := ep.Open("i-1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(lit)
	if string(raw) != ewsMsg {
		t.Errorf("Open returned the wrong body:\n%q", raw)
	}
	if srv.LastImp != "ivanov@corp.ru" {
		t.Errorf("ExchangeImpersonation = %q", srv.LastImp)
	}
}

func TestEWSAppendRoundTrip(t *testing.T) {
	srv := ewstest.New(t, nil)
	ep, _ := ewsBackendFor(t, srv.URL).Connect(context.Background(), "u@d")
	defer ep.Close()
	if _, _, err := ep.Select("INBOX"); err != nil {
		t.Fatal(err)
	}

	id, err := ep.Append([]string{`\Seen`}, time.Now(), bytes.NewBufferString(ewsMsg))
	if err != nil || id == "" {
		t.Fatalf("Append => %q %v", id, err)
	}
	if srv.Count() != 1 {
		t.Fatalf("server has %d messages", srv.Count())
	}

	ids, _ := ep.ListIDs()
	if len(ids) != 1 || ids[0] != id {
		t.Errorf("after Append ListIDs = %v (id %s)", ids, id)
	}
	lit, err := ep.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(lit)
	if string(raw) != ewsMsg {
		t.Errorf("read back something other than what was written:\n%q", raw)
	}
	if !strings.Contains(id, "==") {
		t.Errorf("ID does not look like an EWS ItemId: %q", id)
	}
}

func TestEWSFindFolderFallback(t *testing.T) {
	srv := ewstest.New(t, nil)
	ep, _ := ewsBackendFor(t, srv.URL).Connect(context.Background(), "u@d")
	folder, _, err := ep.Select("Custom")
	if err != nil || folder != "Custom" {
		t.Fatalf("Select(Custom) => %q %v", folder, err)
	}
}
