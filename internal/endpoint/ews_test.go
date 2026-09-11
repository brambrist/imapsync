package endpoint

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/endpoint/ewstest"
)

func ewsBackendFor(t *testing.T, url string) Backend {
	t.Helper()
	b, err := NewBackend(config.Server{Type: config.EndpointEWS, EWSUrl: url, MasterUser: "svc", MasterPass: "pw"},
		0, 5*time.Second, false, 50, false, nil)
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

func TestEWSRejectsLegacyTLSWithoutTheKnob(t *testing.T) {
	srv := ewstest.NewTLS(t, nil, &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11})
	b, err := NewBackend(config.Server{Type: config.EndpointEWS, EWSUrl: srv.URL, MasterUser: "svc", MasterPass: "pw"},
		0, 5*time.Second, true, 50, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.Connect(context.Background(), "u@d")
	if err != nil {
		t.Fatal(err) // Connect itself does not dial - the handshake happens on first use
	}
	defer ep.Close()
	// Select("INBOX") is a distinguished folder and makes no network call; the
	// TLS handshake happens on the first real request.
	if _, _, err := ep.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.ListIDs(); err == nil {
		t.Fatal("expected a TLS handshake failure against a TLS-1.0/1.1-only EWS server")
	}
}

func TestEWSHonoursMinMaxTLSVersion(t *testing.T) {
	srv := ewstest.NewTLS(t, nil, &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11})
	b, err := NewBackend(config.Server{
		Type: config.EndpointEWS, EWSUrl: srv.URL, MasterUser: "svc", MasterPass: "pw",
		MinTLSVersion: "1.0", MaxTLSVersion: "1.1",
	}, 0, 5*time.Second, true, 50, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.Connect(context.Background(), "u@d")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if _, _, err := ep.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.ListIDs(); err != nil {
		t.Fatalf("min_tls_version/max_tls_version should let the client reach a legacy EWS server: %v", err)
	}
}

func TestEWSRejectsSelfSignedWithoutCACert(t *testing.T) {
	srv := ewstest.NewTLS(t, nil, &tls.Config{})
	// insecureTLS false, no ca_cert - the self-signed cert is untrusted.
	b, err := NewBackend(config.Server{Type: config.EndpointEWS, EWSUrl: srv.URL, MasterUser: "svc", MasterPass: "pw"},
		0, 5*time.Second, false, 50, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.Connect(context.Background(), "u@d")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if _, _, err := ep.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.ListIDs(); err == nil {
		t.Fatal("expected a certificate verification failure against a self-signed EWS cert")
	}
}

func TestEWSHonoursCACert(t *testing.T) {
	srv := ewstest.NewTLS(t, nil, &tls.Config{})
	caCert := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(caCert, srv.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := NewBackend(config.Server{
		Type: config.EndpointEWS, EWSUrl: srv.URL, MasterUser: "svc", MasterPass: "pw",
		CACert: caCert,
	}, 0, 5*time.Second, false, 50, false, nil) // insecureTLS stays false
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.Connect(context.Background(), "u@d")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if _, _, err := ep.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.ListIDs(); err != nil {
		t.Fatalf("ca_cert should let the client verify a self-signed EWS cert: %v", err)
	}
}

func TestEWSDebugLogsRequestAndResponse(t *testing.T) {
	srv := ewstest.New(t, map[string]string{"i-1": ewsMsg})

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	b, err := NewBackend(config.Server{Type: config.EndpointEWS, EWSUrl: srv.URL, MasterUser: "svc", MasterPass: "pw"},
		0, 5*time.Second, false, 50, true, logf)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.Connect(context.Background(), "ivanov@corp.ru")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if _, _, err := ep.Select("Sent"); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.ListIDs(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawRequest, sawResponse bool
	for _, l := range lines {
		if strings.Contains(l, "request as svc, impersonating ivanov@corp.ru") && strings.Contains(l, "FindItem") {
			sawRequest = true
		}
		if strings.Contains(l, "response HTTP 200") && strings.Contains(l, "ResponseClass") {
			sawResponse = true
		}
		// never log the Basic auth credential itself.
		if strings.Contains(l, "pw") && !strings.Contains(l, "request as svc") {
			t.Errorf("debug log line looks like it leaked the password: %q", l)
		}
	}
	if !sawRequest {
		t.Errorf("expected a logged SOAP request naming the caller and impersonation target, got:\n%s", strings.Join(lines, "\n"))
	}
	if !sawResponse {
		t.Errorf("expected a logged SOAP response, got:\n%s", strings.Join(lines, "\n"))
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
