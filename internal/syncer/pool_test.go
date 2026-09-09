package syncer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/stats"
)

// fakeSyncer считает вызовы и следит за максимальной параллельностью.
type fakeSyncer struct {
	mu       sync.Mutex
	seen     map[string]int
	cur, max int32
	delay    time.Duration
}

func newFake(delay time.Duration) *fakeSyncer {
	return &fakeSyncer{seen: map[string]int{}, delay: delay}
}

func (f *fakeSyncer) SyncUser(ctx context.Context, u config.User) {
	n := atomic.AddInt32(&f.cur, 1)
	for {
		old := atomic.LoadInt32(&f.max)
		if n <= old || atomic.CompareAndSwapInt32(&f.max, old, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.cur, -1)

	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
	}

	f.mu.Lock()
	f.seen[u.Name]++
	f.mu.Unlock()
}

func mkUsers(n int) []config.User {
	us := make([]config.User, n)
	for i := range us {
		us[i] = config.User{Name: string(rune('a' + i)), UserA: "x", UserB: "y"}
	}
	return us
}

func newTestPool(cfg *config.Config, fs userSyncer) (*Pool, *stats.Collector) {
	coll := stats.New()
	return &Pool{cfg: cfg, sync: fs, stats: coll, logf: func(string, ...any) {}}, coll
}

func TestRunCycleProcessesAllUsersWithinWorkerLimit(t *testing.T) {
	cfg := &config.Config{
		Users:          mkUsers(5),
		Workers:        2,
		PerUserTimeout: config.Duration(time.Minute),
	}
	fs := newFake(20 * time.Millisecond)
	p, _ := newTestPool(cfg, fs)

	p.RunCycle(context.Background())

	if len(fs.seen) != 5 {
		t.Fatalf("обработано юзеров: %d, ожидали 5 (%v)", len(fs.seen), fs.seen)
	}
	for name, c := range fs.seen {
		if c != 1 {
			t.Errorf("юзер %s обработан %d раз", name, c)
		}
	}
	if fs.max > 2 {
		t.Errorf("параллельность %d превысила workers=2", fs.max)
	}
}

func TestWorkersCappedAtUserCount(t *testing.T) {
	cfg := &config.Config{Users: mkUsers(3), Workers: 10, PerUserTimeout: config.Duration(time.Minute)}
	p, _ := newTestPool(cfg, newFake(0))
	if got := p.workers(); got != 3 {
		t.Errorf("workers() = %d, ожидали 3", got)
	}
}

func TestRunCycleStopsOnContextCancel(t *testing.T) {
	cfg := &config.Config{
		Users:          mkUsers(20),
		Workers:        2,
		PerUserTimeout: config.Duration(time.Minute),
	}
	fs := newFake(50 * time.Millisecond)
	p, _ := newTestPool(cfg, fs)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(60 * time.Millisecond); cancel() }()

	start := time.Now()
	p.RunCycle(ctx)
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("RunCycle не остановился быстро после cancel: %s", time.Since(start))
	}

	fs.mu.Lock()
	processed := len(fs.seen)
	fs.mu.Unlock()
	if processed == 20 {
		t.Errorf("после отмены обработаны все 20 юзеров - отмена не сработала")
	}
}

// end-to-end: пул с настоящим Syncer против двух in-memory IMAP-серверов.
func TestPoolRunConvergesRealServers(t *testing.T) {
	cert := selfSignedCert(t)
	srvA := startIMAP(t, cert)
	srvB := startIMAP(t, cert)
	appendMsg(t, srvA, "на A", "pool-a@corp")
	appendMsg(t, srvB, "на B", "pool-b@corp")

	cfg := &config.Config{
		ServerA: srvA, ServerB: srvB,
		InsecureTLS:    true,
		FetchBatchSize: 10,
		HashHeader:     "X-Imapsync-Hash",
		DialTimeout:    config.Duration(5 * time.Second),
		Folders:        []config.FolderPair{{A: "INBOX", B: "INBOX"}},
		Users: []config.User{
			{Name: "u1", UserA: "username", UserB: "username"},
			{Name: "u2", UserA: "username", UserB: "username"},
		},
		Workers:        1,
		PerUserTimeout: config.Duration(10 * time.Second),
		SyncInterval:   config.Duration(20 * time.Millisecond),
	}

	coll := stats.New()
	pool := NewPool(cfg, coll, func(string, ...any) {})

	// один полный цикл без отмены - все юзеры должны отработать целиком
	pool.RunCycle(context.Background())

	want := []string{"0000000@localhost/", "pool-a@corp", "pool-b@corp"}
	if got := inboxMessageIDs(t, srvA); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("A = %v, ожидали %v", got, want)
	}
	if got := inboxMessageIDs(t, srvB); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("B = %v, ожидали %v", got, want)
	}
	if rep := coll.Snapshot(); rep.Total.Errors != 0 {
		t.Errorf("ошибок в последнем цикле: %d (%+v)", rep.Total.Errors, rep.Users)
	}
}

func TestRunLoopsUntilCancel(t *testing.T) {
	cfg := &config.Config{
		Users:          mkUsers(3),
		Workers:        2,
		PerUserTimeout: config.Duration(time.Minute),
		SyncInterval:   config.Duration(10 * time.Millisecond),
		StatsInterval:  config.Duration(0),
	}
	fs := newFake(time.Millisecond)
	p, _ := newTestPool(cfg, fs)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	p.Run(ctx)

	fs.mu.Lock()
	total := 0
	for _, c := range fs.seen {
		total += c
	}
	fs.mu.Unlock()
	if total < 6 { // минимум два цикла по 3 юзера
		t.Errorf("за время работы обработано %d (юзер*цикл), ожидали >= 6", total)
	}
}
