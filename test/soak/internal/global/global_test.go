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
	l.Disturb("operator stopped")
	l.SetChaos("operator_restart")
	if l.Disturbance() != "operator stopped" || l.Chaos() != "operator_restart" {
		t.Errorf("disturbance %q, chaos %q", l.Disturbance(), l.Chaos())
	}
	acquired(t, shared, false, "a mutation during the exclusive one")
	l.Disturb("")
	l.SetChaos("")
	unlock()
	acquired(t, shared, true, "a mutation after the exclusive one")()
	if l.Disturbance() != "" || l.Chaos() != "" {
		t.Errorf("still disturbed: %q, chaos %q", l.Disturbance(), l.Chaos())
	}
	var none *Lock
	if none.Disturbance() != "" || none.Chaos() != "" {
		t.Error("a nil lock is disturbed")
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
