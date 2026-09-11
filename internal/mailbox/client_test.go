package mailbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"

	"imapsync/config"
)

// startLegacyIMAP starts an in-memory IMAP server over TLS with a self-signed
// certificate, restricted to [minVersion, maxVersion] (0, 0 - no restriction) -
// simulating a legacy server such as an unpatched Exchange 2013 that only
// speaks TLS 1.0/1.1. Returns the server address and its certificate as PEM
// (for testing ca_cert).
func startLegacyIMAP(t *testing.T, minVersion, maxVersion uint16) (config.Server, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	be := memory.New()
	srv := server.New(be)
	srv.AllowInsecureAuth = true
	srv.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
		MaxVersion:   maxVersion,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tl := tls.NewListener(l, srv.TLSConfig)
	go srv.Serve(tl) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })

	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return config.Server{Host: "127.0.0.1", Port: port, MasterUser: "username", MasterPass: "password"}, certPEM
}

func TestConnectRejectsLegacyTLSWithoutTheKnob(t *testing.T) {
	// A server that only speaks TLS 1.0/1.1 (like an unpatched Exchange 2013):
	// without min_tls_version the client's default floor (TLS 1.2) can't reach
	// it at all.
	srv, _ := startLegacyIMAP(t, tls.VersionTLS10, tls.VersionTLS11)
	_, err := Connect(context.Background(), srv, "username", 2*time.Second, 2*time.Second, false)
	if err == nil {
		t.Fatal("expected a TLS handshake failure against a TLS-1.0/1.1-only server")
	}
}

func TestConnectHonoursMinMaxTLSVersion(t *testing.T) {
	srv, _ := startLegacyIMAP(t, tls.VersionTLS10, tls.VersionTLS11)
	srv.MinTLSVersion = "1.0"
	srv.MaxTLSVersion = "1.1"

	cl, err := Connect(context.Background(), srv, "username", 2*time.Second, 2*time.Second, true)
	if err != nil {
		t.Fatalf("min_tls_version/max_tls_version should let the client reach a legacy server: %v", err)
	}
	cl.Logout()
}

func TestConnectRejectsSelfSignedWithoutCACert(t *testing.T) {
	srv, _ := startLegacyIMAP(t, 0, 0)
	// insecureTLS is false and ca_cert is unset - a self-signed cert is not in
	// the system trust store.
	_, err := Connect(context.Background(), srv, "username", 2*time.Second, 2*time.Second, false)
	if err == nil {
		t.Fatal("expected a certificate verification failure against a self-signed cert")
	}
}

func TestConnectHonoursCACert(t *testing.T) {
	srv, certPEM := startLegacyIMAP(t, 0, 0)
	srv.CACert = filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(srv.CACert, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// insecureTLS stays false: ca_cert should be enough to verify the
	// self-signed certificate properly (hostname/expiry checks still apply).
	cl, err := Connect(context.Background(), srv, "username", 2*time.Second, 2*time.Second, false)
	if err != nil {
		t.Fatalf("ca_cert should let the client verify a self-signed cert: %v", err)
	}
	cl.Logout()
}
