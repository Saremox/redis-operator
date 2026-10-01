package global

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func acquired(t *testing.T, ch <-chan func(), want bool, what string) func() {
	t.Helper()
	wait := 50 * time.Millisecond
	if want {
		wait = 5 * time.Second
	}
	select {
	case unlock := <-ch:
		if !want {
			t.Fatalf("%s acquired", what)
		}
		return unlock
	case <-time.After(wait):
		if want {
			t.Fatalf("%s not acquired", what)
		}
		return nil
	}
}

func TestExclusivePausesTheOthers(t *testing.T) {
	var l Lock
	running := l.Shared()

	exclusive := make(chan func())
	go func() { exclusive <- l.Exclusive() }()
	acquired(t, exclusive, false, "exclusive while a mutation runs")

	// Mutations that start meanwhile wait for the exclusive one.
	shared := make(chan func())
	go func() { shared <- l.Shared() }()
	acquired(t, shared, false, "a new mutation while an exclusive one waits")

	running()
	unlock := acquired(t, exclusive, true, "exclusive after the running mutation")
	l.SetOperatorDown(true)
	if !l.OperatorDown() {
		t.Error("the operator isn't down")
	}
	acquired(t, shared, false, "a mutation during the exclusive one")
	l.SetOperatorDown(false)
	unlock()
	acquired(t, shared, true, "a mutation after the exclusive one")()
	if l.OperatorDown() {
		t.Error("the operator is still down")
	}
	var none *Lock
	if none.OperatorDown() {
		t.Error("a nil lock has the operator down")
	}
}

func TestExclusiveRunsAlone(t *testing.T) {
	var l Lock
	var shared, exclusive atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			for range 50 {
				if i%10 == 0 {
					unlock := l.Exclusive()
					if exclusive.Add(1) != 1 || shared.Load() != 0 {
						t.Error("exclusive overlaps")
					}
					exclusive.Add(-1)
					unlock()
					continue
				}
				unlock := l.Shared()
				shared.Add(1)
				if exclusive.Load() != 0 {
					t.Error("shared overlaps an exclusive")
				}
				shared.Add(-1)
				unlock()
			}
		})
	}
	wg.Wait()
}
