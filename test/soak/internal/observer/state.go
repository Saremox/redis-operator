package observer

import (
	"maps"
	"slices"
	"time"
)

// tracker follows each invariant's violations and the convergence window
// they are judged against. While a window is open, the instance is expected
// to be converging after a change, so a violation that starts then is not a
// finding. A window closes once every invariant holds, or after the
// convergence timeout; violations still open at the timeout become findings.
type tracker struct {
	timeout  time.Duration
	window   time.Time // zero when no window is open
	violated map[string]*violation
}

type violation struct {
	since   time.Time
	finding bool
}

type eventKind int

const (
	evViolated eventKind = iota
	evRestored
	// evFinding is a violation that became a finding when its window timed
	// out.
	evFinding
	evWindowClosed
	evWindowTimedOut
)

type event struct {
	kind      eventKind
	invariant string
	err       error
	// duration is how long the violation or the window lasted.
	duration time.Duration
	finding  bool
}

func newTracker(timeout time.Duration) *tracker {
	return &tracker{timeout: timeout, violated: map[string]*violation{}}
}

// openWindow starts a convergence window, or restarts the open one.
func (t *tracker) openWindow(now time.Time) {
	t.window = now
}

func (t *tracker) windowOpen() bool {
	return !t.window.IsZero()
}

func (t *tracker) update(now time.Time, checks []check) []event {
	var events []event
	if t.windowOpen() && now.Sub(t.window) >= t.timeout {
		events = append(events, event{kind: evWindowTimedOut, duration: now.Sub(t.window)})
		t.window = time.Time{}
		for _, name := range slices.Sorted(maps.Keys(t.violated)) {
			if v := t.violated[name]; !v.finding {
				v.finding = true
				events = append(events, event{kind: evFinding, invariant: name, duration: now.Sub(v.since), finding: true})
			}
		}
	}
	allOK := true
	for _, c := range checks {
		v := t.violated[c.invariant]
		switch {
		case c.err != nil && v == nil:
			v = &violation{since: now, finding: !t.windowOpen()}
			t.violated[c.invariant] = v
			events = append(events, event{kind: evViolated, invariant: c.invariant, err: c.err, finding: v.finding})
		case c.err == nil && v != nil:
			delete(t.violated, c.invariant)
			events = append(events, event{kind: evRestored, invariant: c.invariant, duration: now.Sub(v.since), finding: v.finding})
		}
		allOK = allOK && c.err == nil
	}
	if t.windowOpen() && allOK {
		events = append(events, event{kind: evWindowClosed, duration: now.Sub(t.window)})
		t.window = time.Time{}
	}
	return events
}
