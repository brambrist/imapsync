package syncer

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
	"sort"
	"testing"
	"time"

	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"

	"imapsync/config"
	"imapsync/internal/mailbox"
	"imapsync/internal/stats"
	"imapsync/internal/store"
)

func TestFilterFlags(t *testing.T) {
	in := []string{`\Seen`, `\Recent`, `\Deleted`, `\Flagged`, `custom`}
	got := filterFlags(in)
	want := []string{`\Seen`, `\Flagged`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("filterFlags(%v) = %v, expected %v", in, got, want)
	}
}

// selfSignedCert - a self-signed certificate for the test TLS server.
func selfSignedCert(t *testing.T) tls.Certificate {
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
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startIMAP starts an in-memory IMAP server over TLS and returns its address as
// a config.Server (master = username/password from the memory backend).
func startIMAP(t *testing.T, cert tls.Certificate) config.Server {
	t.Helper()
	be := memory.New()
	srv := server.New(be)
	srv.AllowInsecureAuth = true
	srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}

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

// appendMsg adds a message to the given server's INBOX.
func appendMsg(t *testing.T, srv config.Server, subject, msgID string) {
	t.Helper()
	cl, err := mailbox.Connect(context.Background(), srv, "username", 5*time.Second, 5*time.Second, true, false, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cl.Logout()

	body := fmt.Sprintf("From: a@example.org\r\nTo: b@example.org\r\nSubject: %s\r\n"+
		"Date: Wed, 09 Sep 2026 12:00:00 +0000\r\nMessage-ID: <%s>\r\n\r\nbody", subject, msgID)
	if err := cl.Append("INBOX", nil, time.Now(), []byte(body)); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// inboxMessageIDs returns the normalized Message-IDs of every message in INBOX.
func inboxMessageIDs(t *testing.T, srv config.Server) []string {
	t.Helper()
	cl, err := mailbox.Connect(context.Background(), srv, "username", 5*time.Second, 5*time.Second, true, false, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cl.Logout()

	st, err := cl.Select("INBOX")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	msgs, err := cl.FetchHeaders(st.Messages, 50)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var ids []string
	for _, m := range msgs {
		f, err := mailbox.ParseFields(m.Header, "X-Imapsync-Hash")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ids = append(ids, f.MessageID)
	}
	sort.Strings(ids)
	return ids
}

func TestSyncUserConvergesBothSides(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	srvB := startIMAP(t, cert)

	// Both backends start with the same message <0000000@localhost/>.
	appendMsg(t, srvA, "only on A", "only-a@corp")
	appendMsg(t, srvB, "only on B", "only-b@corp")

	cfg := &config.Config{
		ServerA:        srvA,
		ServerB:        srvB,
		InsecureTLS:    true,
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		DialTimeout:    config.Duration(5 * time.Second),
		Folders:        []config.FolderPair{{A: "INBOX", B: "INBOX"}},
	}

	coll := stats.New()
	coll.BeginCycle()
	s := New(cfg, coll, t.Logf)
	s.SyncUser(context.Background(), config.User{Name: "u", UserA: "username", UserB: "username"})

	want := []string{"0000000@localhost/", "only-a@corp", "only-b@corp"}
	if got := inboxMessageIDs(t, srvA); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("A after sync = %v, expected %v", got, want)
	}
	if got := inboxMessageIDs(t, srvB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("B after sync = %v, expected %v", got, want)
	}

	rep := coll.Snapshot()
	if rep.Total.CopiedAToB != 1 || rep.Total.CopiedBToA != 1 {
		t.Errorf("copy counters: A->B=%d B->A=%d, expected 1/1", rep.Total.CopiedAToB, rep.Total.CopiedBToA)
	}
	if rep.Total.Errors != 0 {
		t.Errorf("errors: %d (%+v)", rep.Total.Errors, rep.Users)
	}

	// Idempotency: the second run copies nothing.
	coll2 := stats.New()
	coll2.BeginCycle()
	New(cfg, coll2, t.Logf).SyncUser(context.Background(), config.User{Name: "u", UserA: "username", UserB: "username"})
	rep2 := coll2.Snapshot()
	if rep2.Total.CopiedAToB != 0 || rep2.Total.CopiedBToA != 0 {
		t.Errorf("the second run copied extra: A->B=%d B->A=%d", rep2.Total.CopiedAToB, rep2.Total.CopiedBToA)
	}
}

func TestSyncUserResolvesFolderCaseInsensitive(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	srvB := startIMAP(t, cert)
	appendMsg(t, srvA, "a", "ci-a@corp")

	cfg := &config.Config{
		ServerA: srvA, ServerB: srvB,
		InsecureTLS:    true,
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		DialTimeout:    config.Duration(5 * time.Second),
		Folders:        []config.FolderPair{{A: "inbox", B: "InBoX"}}, // differs in case from "INBOX"
	}
	coll := stats.New()
	coll.BeginCycle()
	New(cfg, coll, t.Logf).SyncUser(context.Background(), config.User{Name: "u", UserA: "username", UserB: "username"})

	if r := coll.Snapshot(); r.Total.Errors != 0 {
		t.Fatalf("errors syncing with a differently-cased folder: %+v", r.Users)
	}
	if got := inboxMessageIDs(t, srvB); fmt.Sprint(got) != fmt.Sprint([]string{"0000000@localhost/", "ci-a@corp"}) {
		t.Errorf("B = %v", got)
	}
}

func TestSyncUserIncrementalWithCache(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	srvB := startIMAP(t, cert)
	appendMsg(t, srvA, "inc A", "inc-a@corp")
	appendMsg(t, srvB, "inc B", "inc-b@corp")

	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		ServerA: srvA, ServerB: srvB,
		InsecureTLS:    true,
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		DialTimeout:    config.Duration(5 * time.Second),
		Folders:        []config.FolderPair{{A: "INBOX", B: "INBOX"}},
		StateCache:     true,
		SQLitePath:     "unused-in-test",
	}
	usr := config.User{Name: "u", UserA: "username", UserB: "username"}

	// cycle 1
	c1 := stats.New()
	c1.BeginCycle()
	NewWithState(cfg, c1, t.Logf, st).SyncUser(context.Background(), usr)

	want := []string{"0000000@localhost/", "inc-a@corp", "inc-b@corp"}
	if got := inboxMessageIDs(t, srvA); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("A = %v, expected %v", got, want)
	}
	if got := inboxMessageIDs(t, srvB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("B = %v, expected %v", got, want)
	}
	if r := c1.Snapshot(); r.Total.CopiedAToB != 1 || r.Total.CopiedBToA != 1 || r.Total.Errors != 0 {
		t.Fatalf("cycle 1: %+v (%+v)", r.Total, r.Users)
	}

	// cache populated
	pair := store.PairKey("INBOX", "INBOX")
	ep, msgs, err := st.LoadEndpoint("u", pair, "a")
	if err != nil || !ep.Exists || len(msgs) < 2 {
		t.Fatalf("cache A: %+v msgs=%d err=%v", ep, len(msgs), err)
	}

	// status recorded
	statuses, _ := st.UserStatuses()
	if s := statuses["u"]; s.Status != "ok" || s.CopiedAToB != 1 || s.LastOK.IsZero() {
		t.Errorf("user_status: %+v", s)
	}

	// cycle 2 - nothing is copied
	c2 := stats.New()
	c2.BeginCycle()
	NewWithState(cfg, c2, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c2.Snapshot(); r.Total.CopiedAToB != 0 || r.Total.CopiedBToA != 0 || r.Total.Errors != 0 {
		t.Errorf("cycle 2 is not idempotent: %+v", r.Total)
	}
	// after cycle 2 the cache has 3 messages per side (the copied ones were added)
	_, msgsA2, _ := st.LoadEndpoint("u", pair, "a")
	_, msgsB2, _ := st.LoadEndpoint("u", pair, "b")
	if len(msgsA2) != 3 || len(msgsB2) != 3 {
		t.Errorf("cache after cycle 2: A=%d B=%d, expected 3/3", len(msgsA2), len(msgsB2))
	}
}

func TestMaxFailStreakStopsUser(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	srvB := startIMAP(t, cert)
	appendMsg(t, srvA, "x", "mfs-a@corp")

	st, err := store.Open(t.TempDir() + "/mfs.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// accumulate an error streak
	for range 3 {
		if err := st.RecordRun("u", store.RunResult{At: time.Now(), Status: "error", Errors: 1, LastError: "pain"}); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		ServerA: srvA, ServerB: srvB,
		InsecureTLS:    true,
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		DialTimeout:    config.Duration(5 * time.Second),
		Folders:        []config.FolderPair{{A: "INBOX", B: "INBOX"}},
		MaxFailStreak:  3,
	}
	usr := config.User{Name: "u", UserA: "username", UserB: "username"}

	coll := stats.New()
	coll.BeginCycle()
	NewWithState(cfg, coll, t.Logf, st).SyncUser(context.Background(), usr)

	if rep := coll.Snapshot(); len(rep.Users) != 0 {
		t.Fatalf("the stopped user was still processed: %+v", rep.Users)
	}
	if got := inboxMessageIDs(t, srvB); len(got) != 1 { // only the original message
		t.Errorf("B changed even though sync should be stopped: %v", got)
	}

	// lift the stop
	if err := st.ResumeUser("u"); err != nil {
		t.Fatal(err)
	}
	coll2 := stats.New()
	coll2.BeginCycle()
	NewWithState(cfg, coll2, t.Logf, st).SyncUser(context.Background(), usr)
	if rep := coll2.Snapshot(); len(rep.Users) != 1 || rep.Total.CopiedAToB != 1 {
		t.Errorf("sync did not run after resume: %+v", rep)
	}
}

func TestIncrementalFullResync(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	srvB := startIMAP(t, cert)
	appendMsg(t, srvA, "fr", "fr-a@corp")

	st, err := store.Open(t.TempDir() + "/fr.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := &config.Config{
		ServerA: srvA, ServerB: srvB,
		InsecureTLS:     true,
		FetchBatchSize:  10,
		HashHeader:      "X-Imapsync-Hash",
		DialTimeout:     config.Duration(5 * time.Second),
		Folders:         []config.FolderPair{{A: "INBOX", B: "INBOX"}},
		StateCache:      true,
		SQLitePath:      "unused",
		FullResyncEvery: config.Duration(time.Nanosecond), // every cycle
	}
	usr := config.User{Name: "u", UserA: "username", UserB: "username"}
	pair := store.PairKey("INBOX", "INBOX")

	run := func() {
		c := stats.New()
		c.BeginCycle()
		NewWithState(cfg, c, t.Logf, st).SyncUser(context.Background(), usr)
		if r := c.Snapshot(); r.Total.Errors != 0 {
			t.Fatalf("errors: %+v", r.Users)
		}
	}

	run()
	ep1, _, _ := st.LoadEndpoint("u", pair, "a")
	time.Sleep(5 * time.Millisecond)
	run()
	ep2, _, _ := st.LoadEndpoint("u", pair, "a")

	if !ep2.FullResyncAt.After(ep1.FullResyncAt) {
		t.Errorf("full_resync_at did not advance: %v -> %v", ep1.FullResyncAt, ep2.FullResyncAt)
	}
}
