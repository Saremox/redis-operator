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
	tr := newTracker(time.Minute)
	expect(t, tr.update(at(0), checksFor()))
	expect(t, tr.update(at(5), checksFor(invOneMaster)),
		event{kind: evViolated, invariant: invOneMaster, finding: true})
	expect(t, tr.update(at(10), checksFor(invOneMaster)))
	expect(t, tr.update(at(15), checksFor()),
		event{kind: evRestored, invariant: invOneMaster, duration: 10 * time.Second, finding: true})
}

func TestViolationInsideWindow(t *testing.T) {
	tr := newTracker(time.Minute)
	tr.openWindow(at(0))
	expect(t, tr.update(at(0), checksFor(invPods)),
		event{kind: evViolated, invariant: invPods})
	expect(t, tr.update(at(5), checksFor(invPods, invOneMaster)),
		event{kind: evViolated, invariant: invOneMaster})
	expect(t, tr.update(at(10), checksFor(invPods)),
		event{kind: evRestored, invariant: invOneMaster, duration: 5 * time.Second})
	expect(t, tr.update(at(20), checksFor()),
		event{kind: evRestored, invariant: invPods, duration: 20 * time.Second},
		event{kind: evWindowClosed, duration: 20 * time.Second})
	// The window is closed now.
	expect(t, tr.update(at(25), checksFor(invPods)),
		event{kind: evViolated, invariant: invPods, finding: true})
}

func TestWindowClosesWhenAlreadyConverged(t *testing.T) {
	tr := newTracker(time.Minute)
	tr.openWindow(at(0))
	expect(t, tr.update(at(0), checksFor()),
		event{kind: evWindowClosed})
	if tr.windowOpen() {
		t.Error("window still open")
	}
}

func TestWindowTimeout(t *testing.T) {
	tr := newTracker(time.Minute)
	tr.openWindow(at(0))
	expect(t, tr.update(at(5), checksFor(invPods, invOneMaster)),
		event{kind: evViolated, invariant: invPods},
		event{kind: evViolated, invariant: invOneMaster})
	expect(t, tr.update(at(30), checksFor(invPods)),
		event{kind: evRestored, invariant: invOneMaster, duration: 25 * time.Second})
	expect(t, tr.update(at(60), checksFor(invPods)),
		event{kind: evWindowTimedOut, duration: time.Minute},
		event{kind: evFinding, invariant: invPods, duration: 55 * time.Second, finding: true})
	expect(t, tr.update(at(65), checksFor(invPods)))
	expect(t, tr.update(at(70), checksFor()),
		event{kind: evRestored, invariant: invPods, duration: 65 * time.Second, finding: true})
}

func TestReopeningRestartsTheTimeout(t *testing.T) {
	tr := newTracker(time.Minute)
	tr.openWindow(at(0))
	expect(t, tr.update(at(0), checksFor(invPods)),
		event{kind: evViolated, invariant: invPods})
	tr.openWindow(at(50))
	expect(t, tr.update(at(70), checksFor(invPods)))
	expect(t, tr.update(at(110), checksFor(invPods)),
		event{kind: evWindowTimedOut, duration: time.Minute},
		event{kind: evFinding, invariant: invPods, duration: 110 * time.Second, finding: true})
}

// A violation that began outside a window stays a finding when a window
// opens, and isn't counted again at the timeout.
func TestFindingIsCountedOnce(t *testing.T) {
	tr := newTracker(time.Minute)
	expect(t, tr.update(at(0), checksFor(invPods)),
		event{kind: evViolated, invariant: invPods, finding: true})
	tr.openWindow(at(5))
	expect(t, tr.update(at(65), checksFor(invPods)),
		event{kind: evWindowTimedOut, duration: time.Minute})
	expect(t, tr.update(at(70), checksFor()),
		event{kind: evRestored, invariant: invPods, duration: 70 * time.Second, finding: true})
}
