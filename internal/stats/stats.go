// Package stats - потокобезопасные счётчики синхронизации: по каждому юзеру и
// агрегат по всем, плюс периодический вывод сводки и текущих юзеров.
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

// Logf - функция логирования (обычно log.Printf).
type Logf func(format string, args ...any)

// Counters - набор атомарных счётчиков по одному направлению работы.
type Counters struct {
	CopiedAToB atomic.Int64 // скопировано писем A -> B
	CopiedBToA atomic.Int64 // скопировано писем B -> A
	SkippedDup atomic.Int64 // пропущено как уже существующие (дубли)
	Errors     atomic.Int64 // ошибок при обработке
}

// UserStats - счётчики и состояние обработки одного юзера за текущий цикл.
type UserStats struct {
	Name string

	c Counters

	mu      sync.Mutex
	lastErr string
	started time.Time
	done    time.Time
}

// IncCopiedAToB и прочие Inc* - потокобезопасное увеличение счётчиков.
func (u *UserStats) IncCopiedAToB(n int) { u.c.CopiedAToB.Add(int64(n)) }
func (u *UserStats) IncCopiedBToA(n int) { u.c.CopiedBToA.Add(int64(n)) }
func (u *UserStats) IncSkippedDup(n int) { u.c.SkippedDup.Add(int64(n)) }

// AddError увеличивает счётчик ошибок и запоминает текст последней.
func (u *UserStats) AddError(err error) {
	u.c.Errors.Add(1)
	u.mu.Lock()
	u.lastErr = err.Error()
	u.mu.Unlock()
}

// Report - потокобезопасный снимок статистики юзера.
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

// UserReport - неизменяемый снимок статистики юзера.
type UserReport struct {
	Name                                       string
	CopiedAToB, CopiedBToA, SkippedDup, Errors int64
	Started, Done                              time.Time
	LastErr                                    string
}

// InProgress - true если юзер начат, но ещё не завершён.
func (r UserReport) InProgress() bool { return !r.Started.IsZero() && r.Done.IsZero() }

// Collector агрегирует статистику по всем юзерам за текущий цикл.
type Collector struct {
	mu     sync.RWMutex
	byName map[string]*UserStats
	order  []string
	cycle  int
	cycleT time.Time
}

// New создаёт коллектор.
func New() *Collector {
	return &Collector{byName: make(map[string]*UserStats)}
}

// BeginCycle сбрасывает всю статистику и начинает новый цикл синхронизации.
func (c *Collector) BeginCycle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byName = make(map[string]*UserStats)
	c.order = nil
	c.cycle++
	c.cycleT = time.Now()
}

// BeginUser регистрирует юзера в текущем цикле и помечает начало обработки.
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

// EndUser помечает завершение обработки юзера.
func (c *Collector) EndUser(u *UserStats) {
	u.mu.Lock()
	u.done = time.Now()
	u.mu.Unlock()
}

// Report - агрегированный снимок за текущий цикл.
type Report struct {
	Cycle   int
	Elapsed time.Duration
	Users   []UserReport
	Total   struct {
		CopiedAToB, CopiedBToA, SkippedDup, Errors int64
	}
}

// Snapshot собирает снимок статистики по всем юзерам.
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

// LogSummary выводит сводку по всем юзерам и агрегат.
func (c *Collector) LogSummary(logf Logf) {
	rep := c.Snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "сводка (цикл %d, прошло %s): всего A->B=%d B->A=%d дублей=%d ошибок=%d; юзеров=%d",
		rep.Cycle, rep.Elapsed.Round(time.Second),
		rep.Total.CopiedAToB, rep.Total.CopiedBToA, rep.Total.SkippedDup, rep.Total.Errors, len(rep.Users))

	inProg := make([]string, 0)
	for _, u := range rep.Users {
		if u.InProgress() {
			inProg = append(inProg, fmt.Sprintf("%s(A->B=%d,B->A=%d,дубли=%d,ош=%d)",
				u.Name, u.CopiedAToB, u.CopiedBToA, u.SkippedDup, u.Errors))
		}
	}
	sort.Strings(inProg)
	if len(inProg) > 0 {
		fmt.Fprintf(&b, "; в работе: %s", strings.Join(inProg, " "))
	}
	logf("%s", b.String())
}

// LogUser выводит итог по одному юзеру (вызывать после EndUser).
func (c *Collector) LogUser(u *UserStats, logf Logf) {
	r := u.snapshot()
	dur := ""
	if !r.Started.IsZero() && !r.Done.IsZero() {
		dur = " за " + r.Done.Sub(r.Started).Round(time.Millisecond).String()
	}
	msg := fmt.Sprintf("юзер %s%s: скопировано A->B=%d B->A=%d, пропущено дублей=%d, ошибок=%d",
		r.Name, dur, r.CopiedAToB, r.CopiedBToA, r.SkippedDup, r.Errors)
	if r.LastErr != "" {
		msg += fmt.Sprintf(" (последняя ошибка: %s)", r.LastErr)
	}
	logf("%s", msg)
}

// StartReporter запускает горутину, которая раз в every выводит сводку.
// Возвращает функцию остановки; она дожидается завершения горутины.
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
