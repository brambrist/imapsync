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
		t.Errorf("filterFlags(%v) = %v, ожидали %v", in, got, want)
	}
}

// selfSignedCert - самоподписанный сертификат для тестового TLS-сервера.
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

// startIMAP поднимает in-memory IMAP-сервер поверх TLS и возвращает его
// координаты как config.Server (мастер = username/password из memory-бэкенда).
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

// appendMsg дописывает письмо в INBOX указанного сервера.
func appendMsg(t *testing.T, srv config.Server, subject, msgID string) {
	t.Helper()
	cl, err := mailbox.Connect(srv, "username", 5*time.Second, true)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cl.Logout()

	body := fmt.Sprintf("From: a@example.org\r\nTo: b@example.org\r\nSubject: %s\r\n"+
		"Date: Wed, 09 Sep 2026 12:00:00 +0000\r\nMessage-ID: <%s>\r\n\r\nтело", subject, msgID)
	if err := cl.Append("INBOX", nil, time.Now(), []byte(body)); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// inboxMessageIDs возвращает нормализованные Message-ID всех писем в INBOX.
func inboxMessageIDs(t *testing.T, srv config.Server) []string {
	t.Helper()
	cl, err := mailbox.Connect(srv, "username", 5*time.Second, true)
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

	// Оба бэкенда стартуют с одинаковым письмом <0000000@localhost/>.
	appendMsg(t, srvA, "только на A", "only-a@corp")
	appendMsg(t, srvB, "только на B", "only-b@corp")

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
		t.Errorf("A после синка = %v, ожидали %v", got, want)
	}
	if got := inboxMessageIDs(t, srvB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("B после синка = %v, ожидали %v", got, want)
	}

	rep := coll.Snapshot()
	if rep.Total.CopiedAToB != 1 || rep.Total.CopiedBToA != 1 {
		t.Errorf("счётчики копирования: A->B=%d B->A=%d, ожидали 1/1", rep.Total.CopiedAToB, rep.Total.CopiedBToA)
	}
	if rep.Total.Errors != 0 {
		t.Errorf("ошибок: %d (%+v)", rep.Total.Errors, rep.Users)
	}

	// Идемпотентность: второй прогон ничего не копирует.
	coll2 := stats.New()
	coll2.BeginCycle()
	New(cfg, coll2, t.Logf).SyncUser(context.Background(), config.User{Name: "u", UserA: "username", UserB: "username"})
	rep2 := coll2.Snapshot()
	if rep2.Total.CopiedAToB != 0 || rep2.Total.CopiedBToA != 0 {
		t.Errorf("второй прогон скопировал лишнее: A->B=%d B->A=%d", rep2.Total.CopiedAToB, rep2.Total.CopiedBToA)
	}
}

func TestSyncUserIncrementalWithCache(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	srvB := startIMAP(t, cert)
	appendMsg(t, srvA, "инкр A", "inc-a@corp")
	appendMsg(t, srvB, "инкр B", "inc-b@corp")

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

	// цикл 1
	c1 := stats.New()
	c1.BeginCycle()
	NewWithState(cfg, c1, t.Logf, st).SyncUser(context.Background(), usr)

	want := []string{"0000000@localhost/", "inc-a@corp", "inc-b@corp"}
	if got := inboxMessageIDs(t, srvA); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("A = %v, ожидали %v", got, want)
	}
	if got := inboxMessageIDs(t, srvB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("B = %v, ожидали %v", got, want)
	}
	if r := c1.Snapshot(); r.Total.CopiedAToB != 1 || r.Total.CopiedBToA != 1 || r.Total.Errors != 0 {
		t.Fatalf("цикл 1: %+v (%+v)", r.Total, r.Users)
	}

	// кэш заполнен
	pair := store.PairKey("INBOX", "INBOX")
	uidv, msgs, err := st.LoadEndpoint("u", pair, "a")
	if err != nil || uidv == 0 || len(msgs) != 2 {
		t.Fatalf("кэш A: uidv=%d msgs=%d err=%v", uidv, len(msgs), err)
	}

	// статус записан
	statuses, _ := st.UserStatuses()
	if s := statuses["u"]; s.Status != "ok" || s.CopiedAToB != 1 || s.LastOK.IsZero() {
		t.Errorf("user_status: %+v", s)
	}

	// цикл 2 - ничего не копируется
	c2 := stats.New()
	c2.BeginCycle()
	NewWithState(cfg, c2, t.Logf, st).SyncUser(context.Background(), usr)
	if r := c2.Snapshot(); r.Total.CopiedAToB != 0 || r.Total.CopiedBToA != 0 || r.Total.Errors != 0 {
		t.Errorf("цикл 2 не идемпотентен: %+v", r.Total)
	}
	// после цикла 2 в кэше по 3 письма на сторону (добавились скопированные)
	_, msgsA2, _ := st.LoadEndpoint("u", pair, "a")
	_, msgsB2, _ := st.LoadEndpoint("u", pair, "b")
	if len(msgsA2) != 3 || len(msgsB2) != 3 {
		t.Errorf("кэш после цикла 2: A=%d B=%d, ожидали 3/3", len(msgsA2), len(msgsB2))
	}
}
