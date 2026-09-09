// Package stats provides thread-safe sync counters: per-user and an aggregate
// over all, plus periodic printing of the summary and the in-progress users.
package stats

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Logf is a logging function (usually log.Printf).
type Logf func(format string, args ...any)

// Counters is a set of atomic counters for one direction of work.
type Counters struct {
	CopiedAToB atomic.Int64 // messages copied A -> B
	CopiedBToA atomic.Int64 // messages copied B -> A
	SkippedDup atomic.Int64 // skipped as already present (duplicates)
	Errors     atomic.Int64 // errors during processing
}

// UserStats holds the counters and processing state of one user for the current
// cycle.
type UserStats struct {
	Name string

	c Counters

	mu      sync.Mutex
	lastErr string
	started time.Time
	done    time.Time
}

// IncCopiedAToB and the other Inc* methods bump counters thread-safely.
func (u *UserStats) IncCopiedAToB(n int) { u.c.CopiedAToB.Add(int64(n)) }
func (u *UserStats) IncCopiedBToA(n int) { u.c.CopiedBToA.Add(int64(n)) }
func (u *UserStats) IncSkippedDup(n int) { u.c.SkippedDup.Add(int64(n)) }

// AddError bumps the error counter and remembers the last error text.
func (u *UserStats) AddError(err error) {
	u.c.Errors.Add(1)
	u.mu.Lock()
	u.lastErr = err.Error()
	u.mu.Unlock()
}

// Report is a thread-safe snapshot of a user's stats.
func (u *UserStats) Report() UserReport { return u.snapshot() }

func (u *UserStats) snapshot() UserReport {
	u.mu.Lock()
	started, done, lastErr := u.started, u.done, u.lastErr
	u.mu.Unlock()
	return UserReport{
		Name:       u.Name,
		CopiedAToB: u.c.CopiedAToB.Load(),
		CopiedBToA: u.c.CopiedBToA.Load(),
		SkippedDup: u.c.SkippedDup.Load(),
		Errors:     u.c.Errors.Load(),
		Started:    started,
		Done:       done,
		LastErr:    lastErr,
	}
}

// UserReport is an immutable snapshot of a user's stats.
type UserReport struct {
	Name                                       string
	CopiedAToB, CopiedBToA, SkippedDup, Errors int64
	Started, Done                              time.Time
	LastErr                                    string
}

// InProgress is true if the user has started but not yet finished.
func (r UserReport) InProgress() bool { return !r.Started.IsZero() && r.Done.IsZero() }

// Collector aggregates stats over all users for the current cycle.
type Collector struct {
	mu     sync.RWMutex
	byName map[string]*UserStats
	order  []string
	cycle  int
	cycleT time.Time
}

// New creates a collector.
func New() *Collector {
	return &Collector{byName: make(map[string]*UserStats)}
}

// BeginCycle resets all stats and starts a new sync cycle.
func (c *Collector) BeginCycle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byName = make(map[string]*UserStats)
	c.order = nil
	c.cycle++
	c.cycleT = time.Now()
}

// BeginUser registers a user in the current cycle and marks processing start.
func (c *Collector) BeginUser(name string) *UserStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.byName[name]
	if !ok {
		u = &UserStats{Name: name}
		c.byName[name] = u
		c.order = append(c.order, name)
	}
	u.mu.Lock()
	u.started = time.Now()
	u.done = time.Time{}
	u.mu.Unlock()
	return u
}

// EndUser marks the end of a user's processing.
func (c *Collector) EndUser(u *UserStats) {
	u.mu.Lock()
	u.done = time.Now()
	u.mu.Unlock()
}

// Report is an aggregated snapshot for the current cycle.
type Report struct {
	Cycle   int
	Elapsed time.Duration
	Users   []UserReport
	Total   struct {
		CopiedAToB, CopiedBToA, SkippedDup, Errors int64
	}
}

// Snapshot collects a stats snapshot over all users.
func (c *Collector) Snapshot() Report {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var rep Report
	rep.Cycle = c.cycle
	if !c.cycleT.IsZero() {
		rep.Elapsed = time.Since(c.cycleT)
	}
	for _, name := range c.order {
		ur := c.byName[name].snapshot()
		rep.Users = append(rep.Users, ur)
		rep.Total.CopiedAToB += ur.CopiedAToB
		rep.Total.CopiedBToA += ur.CopiedBToA
		rep.Total.SkippedDup += ur.SkippedDup
		rep.Total.Errors += ur.Errors
	}
	return rep
}

// LogSummary prints a per-user summary and the aggregate.
func (c *Collector) LogSummary(logf Logf) {
	rep := c.Snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "summary (cycle %d, elapsed %s): total A->B=%d B->A=%d dups=%d errors=%d; users=%d",
		rep.Cycle, rep.Elapsed.Round(time.Second),
		rep.Total.CopiedAToB, rep.Total.CopiedBToA, rep.Total.SkippedDup, rep.Total.Errors, len(rep.Users))

	inProg := make([]string, 0)
	for _, u := range rep.Users {
		if u.InProgress() {
			inProg = append(inProg, fmt.Sprintf("%s(A->B=%d,B->A=%d,dups=%d,err=%d)",
				u.Name, u.CopiedAToB, u.CopiedBToA, u.SkippedDup, u.Errors))
		}
	}
	sort.Strings(inProg)
	if len(inProg) > 0 {
		fmt.Fprintf(&b, "; in progress: %s", strings.Join(inProg, " "))
	}
	logf("%s", b.String())
}

// LogUser prints the result for one user (call it after EndUser).
func (c *Collector) LogUser(u *UserStats, logf Logf) {
	r := u.snapshot()
	dur := ""
	if !r.Started.IsZero() && !r.Done.IsZero() {
		dur = " in " + r.Done.Sub(r.Started).Round(time.Millisecond).String()
	}
	msg := fmt.Sprintf("user %s%s: copied A->B=%d B->A=%d, skipped dups=%d, errors=%d",
		r.Name, dur, r.CopiedAToB, r.CopiedBToA, r.SkippedDup, r.Errors)
	if r.LastErr != "" {
		msg += fmt.Sprintf(" (last error: %s)", r.LastErr)
	}
	logf("%s", msg)
}

// StartReporter starts a goroutine that prints the summary every `every`.
// Returns a stop function; it waits for the goroutine to finish.
func (c *Collector) StartReporter(ctx context.Context, every time.Duration, logf Logf) (stop func()) {
	if every <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.LogSummary(logf)
			}
		}
	}()
	return func() { <-done }
}
