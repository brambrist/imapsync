package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validCfg = `
server_a:
  host: a.example
  master_user: m
  master_pass: p
server_b:
  host: b.example
  master_user: m
  master_pass: p
folders:
  - a: Sent
    b: Sent Items
users:
  - name: u1
    user_a: u1@a
    user_b: u1@b
  - name: u2
    user_a: u2@a
    user_b: u2@b
workers: 1
sync_interval: 2m
`

func TestLoadValidWithDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, validCfg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServerA.Port != 993 || cfg.ServerB.Port != 993 {
		t.Errorf("default port not applied: %d/%d", cfg.ServerA.Port, cfg.ServerB.Port)
	}
	if cfg.SyncInterval.Std() != 2*time.Minute {
		t.Errorf("sync_interval: got %v", cfg.SyncInterval.Std())
	}
	if cfg.StatsInterval.Std() != time.Minute {
		t.Errorf("stats_interval default: got %v", cfg.StatsInterval.Std())
	}
	if cfg.FetchBatchSize != 200 {
		t.Errorf("fetch_batch_size default: got %d", cfg.FetchBatchSize)
	}
	if cfg.HashHeader != "X-Imapsync-Hash" {
		t.Errorf("hash_header default: got %q", cfg.HashHeader)
	}
	if cfg.IOTimeout.Std() != 5*time.Minute {
		t.Errorf("io_timeout default: got %v", cfg.IOTimeout.Std())
	}
	if cfg.ConnectRetries != 3 {
		t.Errorf("connect_retries default: got %d", cfg.ConnectRetries)
	}
	if cfg.RetryBackoff.Std() != 5*time.Second {
		t.Errorf("retry_backoff default: got %v", cfg.RetryBackoff.Std())
	}
	if cfg.FullResyncEvery.Std() != 24*time.Hour {
		t.Errorf("full_resync_every default: got %v", cfg.FullResyncEvery.Std())
	}
	if cfg.Debug {
		t.Error("debug should default to false")
	}
}

func TestWorkersMustBeLessThanUsers(t *testing.T) {
	body := validCfg + "" // workers:1, users:2 - ok
	if _, err := Load(writeTemp(t, body)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bad := `
server_a: {host: a, master_user: m, master_pass: p}
server_b: {host: b, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 2
`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("expected error: workers >= number of users")
	}
}

func TestRejectsUnknownField(t *testing.T) {
	bad := validCfg + "\nbogus_field: 1\n"
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("expected error on unknown field")
	}
}

func TestRejectsMissingServer(t *testing.T) {
	bad := `
server_b: {host: b, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("expected error: server_a.host not set")
	}
}

func TestStateCacheRequiresSQLitePath(t *testing.T) {
	if _, err := Load(writeTemp(t, validCfg+"\nstate_cache: true\n")); err == nil {
		t.Fatal("expected error: state_cache without sqlite_path")
	}
	ok := validCfg + "\nstate_cache: true\nsqlite_path: /tmp/x.db\n"
	if _, err := Load(writeTemp(t, ok)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDirectionDefaultAndValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, validCfg))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Direction != DirectionBoth {
		t.Errorf("direction default: got %q", cfg.Direction)
	}
	if _, err := Load(writeTemp(t, validCfg+"\ndirection: sideways\n")); err == nil {
		t.Fatal("expected an error for an unknown direction")
	}
	if _, err := Load(writeTemp(t, validCfg+"\ndirection: a-to-b\n")); err != nil {
		t.Fatalf("a-to-b should be valid: %v", err)
	}
}

func TestPSTEndpointValidation(t *testing.T) {
	base := `
server_a:
  type: pst
  root: /var/archives/%u.pst
server_b:
  host: b.example
  master_user: m
  master_pass: p
folders:
  - a: Sent Items
    b: Sent
users:
  - {name: u1, user_a: u1@a, user_b: u1@b}
  - {name: u2, user_a: u2@a, user_b: u2@b}
workers: 1
sync_interval: 2m
`
	if _, err := Load(writeTemp(t, base)); err != nil {
		t.Fatalf("pst source + imap target should be valid: %v", err)
	}

	// pst without a root
	noRoot := `
server_a: {type: pst}
server_b: {host: b, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, noRoot)); err == nil {
		t.Fatal("expected an error: type pst without root")
	}

	// direction points into the read-only PST
	if _, err := Load(writeTemp(t, base+"\ndirection: b-to-a\n")); err == nil {
		t.Fatal("expected an error: direction b-to-a into a read-only server_a")
	}

	// both sides read-only
	bothPST := `
server_a: {type: pst, root: /a/%u.pst}
server_b: {type: pst, root: /b/%u.pst}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, bothPST)); err == nil {
		t.Fatal("expected an error: both endpoints read-only")
	}
}

func TestParseTLSVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    uint16
		wantErr bool
	}{
		{"", 0, false},
		{"1.0", tls.VersionTLS10, false},
		{"tls1.0", tls.VersionTLS10, false},
		{"1.1", tls.VersionTLS11, false},
		{"1.2", tls.VersionTLS12, false},
		{"1.3", tls.VersionTLS13, false},
		{"sslv3", 0, true},
		{"ssl", 0, true},
		{"2.0", 0, true},
		{"bogus", 0, true},
	}
	for _, c := range cases {
		got, err := ParseTLSVersion(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseTLSVersion(%q): expected an error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTLSVersion(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseTLSVersion(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestServerTLSConfig(t *testing.T) {
	s := Server{Host: "mail.corp.ru", MinTLSVersion: "1.0", MaxTLSVersion: "1.1"}
	cfg, err := s.TLSConfig(false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "mail.corp.ru" {
		t.Errorf("ServerName = %q", cfg.ServerName)
	}
	if cfg.MinVersion != tls.VersionTLS10 || cfg.MaxVersion != tls.VersionTLS11 {
		t.Errorf("MinVersion=%d MaxVersion=%d", cfg.MinVersion, cfg.MaxVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should follow the insecureTLS argument")
	}

	if _, err := (Server{MinTLSVersion: "nonsense"}).TLSConfig(false); err == nil {
		t.Fatal("expected an error for a bad min_tls_version")
	}
}

// testCertPEM is a static self-signed certificate, used only to exercise PEM
// parsing (loadCACertPool) - no live TLS handshake happens against it here;
// see internal/mailbox and internal/endpoint for handshake-level ca_cert tests.
const testCertPEM = `-----BEGIN CERTIFICATE-----
MIIBYDCCAQWgAwIBAgIBATAKBggqhkjOPQQDAjAXMRUwEwYDVQQDEwx0ZXN0Lmlu
dmFsaWQwIBcNMjAwMTAxMDAwMDAwWhgPMjA5OTAxMDEwMDAwMDBaMBcxFTATBgNV
BAMTDHRlc3QuaW52YWxpZDBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABAywIlM6
ZL7lSacKth1r5HP09SsLThl3oYIQP7f0bnh3blf+/OacTBLWtXTeb8HjAzkRHBG5
px+7+qtS/aLt+wCjQDA+MA4GA1UdDwEB/wQEAwIChDATBgNVHSUEDDAKBggrBgEF
BQcDATAXBgNVHREEEDAOggx0ZXN0LmludmFsaWQwCgYIKoZIzj0EAwIDSQAwRgIh
AOPfGQr+kYfO+YjXKi6B2+qZv3FfMhruuEQPkmNCTGsXAiEA5q5aVU9DFM6AN2VD
TUePrluTLDDMT3FF81sLZco8y1o=
-----END CERTIFICATE-----
`

func TestServerTLSConfigWithCACert(t *testing.T) {
	certPath := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(certPath, []byte(testCertPEM), 0o600); err != nil {
		t.Fatal(err)
	}

	s := Server{Host: "mail.corp.ru", CACert: certPath}
	cfg, err := s.TLSConfig(false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs == nil {
		t.Fatal("RootCAs should be set when ca_cert is given")
	}

	if _, err := (Server{CACert: filepath.Join(t.TempDir(), "missing.pem")}).TLSConfig(false); err == nil {
		t.Fatal("expected an error for a missing ca_cert file")
	}

	badPath := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Server{CACert: badPath}).TLSConfig(false); err == nil {
		t.Fatal("expected an error for a ca_cert file with no PEM certificate")
	}
}

func TestCACertValidation(t *testing.T) {
	certPath := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(certPath, []byte(testCertPEM), 0o600); err != nil {
		t.Fatal(err)
	}

	ok := fmt.Sprintf(`
server_a: {host: a.example, master_user: m, master_pass: p, ca_cert: %q}
server_b: {host: b.example, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`, certPath)
	if _, err := Load(writeTemp(t, ok)); err != nil {
		t.Fatalf("a valid ca_cert should be accepted: %v", err)
	}

	bad := `
server_a: {host: a.example, master_user: m, master_pass: p, ca_cert: /no/such/file.pem}
server_b: {host: b.example, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("expected an error for a missing ca_cert file")
	}
}

func TestTLSVersionValidation(t *testing.T) {
	// a legacy server (e.g. an unpatched Exchange 2013 stuck on TLS 1.0/1.1)
	// validates fine
	ok := `
server_a:
  host: a.example
  master_user: m
  master_pass: p
  min_tls_version: "1.0"
  max_tls_version: "1.1"
server_b: {host: b.example, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, ok)); err != nil {
		t.Fatalf("min/max_tls_version should be valid: %v", err)
	}

	bad := `
server_a: {host: a.example, master_user: m, master_pass: p, min_tls_version: bogus}
server_b: {host: b.example, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("expected an error for an unknown min_tls_version")
	}

	inverted := `
server_a: {host: a.example, master_user: m, master_pass: p, min_tls_version: "1.2", max_tls_version: "1.0"}
server_b: {host: b.example, master_user: m, master_pass: p}
folders: [{a: Sent, b: Sent}]
users:
  - {name: u1, user_a: a, user_b: b}
  - {name: u2, user_a: a, user_b: b}
workers: 1
`
	if _, err := Load(writeTemp(t, inverted)); err == nil {
		t.Fatal("expected an error: min_tls_version above max_tls_version")
	}
}

func TestDebugFlag(t *testing.T) {
	cfg, err := Load(writeTemp(t, validCfg+"\ndebug: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Debug {
		t.Error("debug: true should be honoured")
	}
}

func TestRejectsBadDuration(t *testing.T) {
	bad := validCfg + "\nstats_interval: \"nonsense\"\n"
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("expected a duration parse error")
	}
}
