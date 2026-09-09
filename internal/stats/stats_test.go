package stats

import (
	"errors"
	"sync"
	"testing"
)

func TestConcurrentCountersAndAggregate(t *testing.T) {
	c := New()
	c.BeginCycle()

	names := []string{"u1", "u2", "u3"}
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			u := c.BeginUser(name)
			for range 100 {
				u.IncCopiedAToB(1)
				u.IncCopiedBToA(2)
				u.IncSkippedDup(1)
			}
			u.AddError(errors.New("бах"))
			c.EndUser(u)
		}(n)
	}
	wg.Wait()

	rep := c.Snapshot()
	if len(rep.Users) != 3 {
		t.Fatalf("юзеров в отчёте: %d", len(rep.Users))
	}
	if rep.Total.CopiedAToB != 300 || rep.Total.CopiedBToA != 600 || rep.Total.SkippedDup != 300 || rep.Total.Errors != 3 {
		t.Errorf("агрегат неверный: %+v", rep.Total)
	}
	for _, u := range rep.Users {
		if u.InProgress() {
			t.Errorf("%s всё ещё InProgress после EndUser", u.Name)
		}
		if u.LastErr != "бах" {
			t.Errorf("%s LastErr=%q", u.Name, u.LastErr)
		}
	}
}

func TestBeginCycleResets(t *testing.T) {
	c := New()
	c.BeginCycle()
	u := c.BeginUser("u1")
	u.IncCopiedAToB(5)
	c.EndUser(u)

	c.BeginCycle()
	rep := c.Snapshot()
	if len(rep.Users) != 0 || rep.Total.CopiedAToB != 0 {
		t.Errorf("BeginCycle не сбросил статистику: %+v", rep)
	}
	if rep.Cycle != 2 {
		t.Errorf("номер цикла = %d, ожидали 2", rep.Cycle)
	}
}

func TestInProgressVisibleBeforeEnd(t *testing.T) {
	c := New()
	c.BeginCycle()
	u := c.BeginUser("u1")
	u.IncCopiedAToB(3)

	rep := c.Snapshot()
	if len(rep.Users) != 1 || !rep.Users[0].InProgress() {
		t.Fatalf("юзер в работе не виден: %+v", rep.Users)
	}
	if rep.Users[0].CopiedAToB != 3 {
		t.Errorf("живой счётчик = %d", rep.Users[0].CopiedAToB)
	}
	c.EndUser(u)
}
