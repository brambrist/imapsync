package mailbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"

	"imapsync/config"
)

// startLegacyIMAP starts an in-memory IMAP server over TLS restricted to
// [minVersion, maxVersion] - simulating a legacy server such as an unpatched
// Exchange 2013 that only speaks TLS 1.0/1.1.
func startLegacyIMAP(t *testing.T, minVersion, maxVersion uint16) config.Server {
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
	return config.Server{Host: "127.0.0.1", Port: port, MasterUser: "username", MasterPass: "password"}
}

func TestConnectRejectsLegacyTLSWithoutTheKnob(t *testing.T) {
	// A server that only speaks TLS 1.0/1.1 (like an unpatched Exchange 2013):
	// without min_tls_version the client's default floor (TLS 1.2) can't reach
	// it at all.
	srv := startLegacyIMAP(t, tls.VersionTLS10, tls.VersionTLS11)
	_, err := Connect(context.Background(), srv, "username", 2*time.Second, 2*time.Second, false)
	if err == nil {
		t.Fatal("expected a TLS handshake failure against a TLS-1.0/1.1-only server")
	}
}

func TestConnectHonoursMinMaxTLSVersion(t *testing.T) {
	srv := startLegacyIMAP(t, tls.VersionTLS10, tls.VersionTLS11)
	srv.MinTLSVersion = "1.0"
	srv.MaxTLSVersion = "1.1"

	cl, err := Connect(context.Background(), srv, "username", 2*time.Second, 2*time.Second, true)
	if err != nil {
		t.Fatalf("min_tls_version/max_tls_version should let the client reach a legacy server: %v", err)
	}
	cl.Logout()
}
