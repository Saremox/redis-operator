package observer

import (
	"errors"
	"slices"
	"testing"
	"time"
)

var errBad = errors.New("bad")

func at(s int) time.Time { return time.Unix(1000, 0).Add(time.Duration(s) * time.Second) }

// checksFor returns the pods and one_master checks, with the given ones
// violated.
func checksFor(bad ...string) []check {
	var checks []check
	for _, name := range []string{invPods, invOneMaster} {
		c := check{invariant: name}
		if slices.Contains(bad, name) {
			c.err = errBad
		}
		checks = append(checks, c)
	}
	return checks
}

func expect(t *testing.T, got []event, want ...event) {
	t.Helper()
	for i := range got {
		got[i].err = nil
	}
	if !slices.Equal(got, want) {
		t.Errorf("events\n got %+v\nwant %+v", got, want)
	}
}

func TestViolationOutsideWindowIsFinding(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	expect(t, tr.update(at(0), checksFor(), false))
	expect(t, tr.update(at(5), checksFor(invOneMaster), false),
		event{kind: evViolated, invariant: invOneMaster, finding: true})
	expect(t, tr.update(at(10), checksFor(invOneMaster), false))
	expect(t, tr.update(at(15), checksFor(), false),
		event{kind: evRestored, invariant: invOneMaster, duration: 10 * time.Second, finding: true})
}

func TestViolationInsideWindow(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.openWindow(at(0))
	expect(t, tr.update(at(0), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods})
	expect(t, tr.update(at(5), checksFor(invPods, invOneMaster), false),
		event{kind: evViolated, invariant: invOneMaster})
	expect(t, tr.update(at(10), checksFor(invPods), false),
		event{kind: evRestored, invariant: invOneMaster, duration: 5 * time.Second})
	expect(t, tr.update(at(20), checksFor(), false),
		event{kind: evRestored, invariant: invPods, duration: 20 * time.Second},
		event{kind: evWindowClosed, duration: 20 * time.Second})
	// The window is closed now.
	expect(t, tr.update(at(25), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods, finding: true})
}

func TestWindowClosesWhenAlreadyConverged(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.openWindow(at(0))
	expect(t, tr.update(at(0), checksFor(), false),
		event{kind: evWindowClosed})
	if tr.windowOpen() {
		t.Error("window still open")
	}
}

func TestWindowTimeout(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.openWindow(at(0))
	expect(t, tr.update(at(5), checksFor(invPods, invOneMaster), false),
		event{kind: evViolated, invariant: invPods},
		event{kind: evViolated, invariant: invOneMaster})
	expect(t, tr.update(at(30), checksFor(invPods), false),
		event{kind: evRestored, invariant: invOneMaster, duration: 25 * time.Second})
	expect(t, tr.update(at(60), checksFor(invPods), false),
		event{kind: evWindowTimedOut, duration: time.Minute},
		event{kind: evFinding, invariant: invPods, duration: 55 * time.Second, finding: true})
	expect(t, tr.update(at(65), checksFor(invPods), false))
	expect(t, tr.update(at(70), checksFor(), false),
		event{kind: evRestored, invariant: invPods, duration: 65 * time.Second, finding: true})
}

func TestReopeningRestartsTheTimeout(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.openWindow(at(0))
	expect(t, tr.update(at(0), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods})
	tr.openWindow(at(50))
	expect(t, tr.update(at(70), checksFor(invPods), false))
	expect(t, tr.update(at(110), checksFor(invPods), false),
		event{kind: evWindowTimedOut, duration: time.Minute},
		event{kind: evFinding, invariant: invPods, duration: 110 * time.Second, finding: true})
}

// A violation that began outside a window stays a finding when a window
// opens, and isn't counted again at the timeout.
func TestFindingIsCountedOnce(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	expect(t, tr.update(at(0), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods, finding: true})
	tr.openWindow(at(5))
	expect(t, tr.update(at(65), checksFor(invPods), false),
		event{kind: evWindowTimedOut, duration: time.Minute})
	expect(t, tr.update(at(70), checksFor(), false),
		event{kind: evRestored, invariant: invPods, duration: 70 * time.Second, finding: true})
}

// A held window doesn't close on the first tick where every invariant
// holds: the mutation must have converged, and the dwell passed.
func TestHeldWindow(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	expect(t, tr.update(at(0), checksFor(), false))
	tr.hold(at(1), time.Minute)
	tr.apply(at(1))
	// Converged before the operator picked the change up.
	expect(t, tr.update(at(5), checksFor(), true))
	expect(t, tr.update(at(10), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods})
	expect(t, tr.update(at(15), checksFor(), false),
		event{kind: evRestored, invariant: invPods, duration: 5 * time.Second})
	expect(t, tr.update(at(20), checksFor(invPods), true),
		event{kind: evViolated, invariant: invPods})
	expect(t, tr.update(at(25), checksFor(), true),
		event{kind: evRestored, invariant: invPods, duration: 5 * time.Second},
		event{kind: evWindowClosed, duration: 24 * time.Second})
	if tr.windowOpen() || !tr.held.IsZero() {
		t.Error("window still open or held")
	}
	// A plain window closes on the first tick again.
	tr.openWindow(at(30))
	expect(t, tr.update(at(30), checksFor(), false),
		event{kind: evWindowClosed})
}

func TestHeldWindowDwell(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.hold(at(0), time.Minute)
	tr.apply(at(0))
	expect(t, tr.update(at(5), checksFor(), true))
	expect(t, tr.update(at(10), checksFor(), true))
	expect(t, tr.update(at(15), checksFor(), true),
		event{kind: evWindowClosed, duration: 15 * time.Second})
}

func TestHeldWindowTimeout(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.hold(at(0), time.Minute)
	tr.apply(at(0))
	expect(t, tr.update(at(5), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods})
	expect(t, tr.update(at(30), checksFor(), false),
		event{kind: evRestored, invariant: invPods, duration: 25 * time.Second})
	// Every invariant holds, but the mutation never converged.
	expect(t, tr.update(at(60), checksFor(), false),
		event{kind: evWindowTimedOut, duration: time.Minute})
	if !tr.held.IsZero() {
		t.Error("still held")
	}
}

// A held window times out its own timeout after the mutation was applied,
// however long applying it took, not after the window opened.
func TestHeldTimeoutCountsFromApplied(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.hold(at(0), 3*time.Minute)
	expect(t, tr.update(at(5), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods})
	// Still being applied: no timeout yet.
	expect(t, tr.update(at(600), checksFor(invPods), false))
	tr.apply(at(600))
	expect(t, tr.update(at(779), checksFor(invPods), false))
	expect(t, tr.update(at(780), checksFor(invPods), false),
		event{kind: evWindowTimedOut, duration: 780 * time.Second},
		event{kind: evFinding, invariant: invPods, duration: 775 * time.Second, finding: true})
}

// A generation change inside a held window, like the operator's every
// status update, neither extends nor restarts it.
func TestGenerationWhileHeld(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.hold(at(0), 2*time.Minute)
	tr.apply(at(0))
	for s := 10; s < 120; s += 10 {
		tr.openWindow(at(s))
		expect(t, tr.update(at(s), checksFor(invPods), false), func() []event {
			if s == 10 {
				return []event{{kind: evViolated, invariant: invPods}}
			}
			return nil
		}()...)
	}
	expect(t, tr.update(at(120), checksFor(invPods), false),
		event{kind: evWindowTimedOut, duration: 2 * time.Minute},
		event{kind: evFinding, invariant: invPods, duration: 110 * time.Second, finding: true})
}

// Outside a hold, a generation change opens a window, and a later one
// restarts it, as before.
func TestGenerationOutsideHold(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.openWindow(at(0))
	expect(t, tr.update(at(0), checksFor(invPods), false),
		event{kind: evViolated, invariant: invPods})
	tr.openWindow(at(40))
	expect(t, tr.update(at(90), checksFor(invPods), false))
	expect(t, tr.update(at(100), checksFor(invPods), false),
		event{kind: evWindowTimedOut, duration: time.Minute},
		event{kind: evFinding, invariant: invPods, duration: 100 * time.Second, finding: true})
}

// Something outside the instance restarts a held window's timeout.
func TestExtendWhileHeld(t *testing.T) {
	tr := newTracker(time.Minute, 15*time.Second)
	tr.hold(at(0), time.Minute)
	tr.apply(at(0))
	tr.extend(at(50))
	expect(t, tr.update(at(100), checksFor(), false))
	expect(t, tr.update(at(110), checksFor(), false),
		event{kind: evWindowTimedOut, duration: 110 * time.Second})
}
