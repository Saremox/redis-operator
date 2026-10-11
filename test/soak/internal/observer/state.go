package observer

import (
	"maps"
	"slices"
	"time"
)

// tracker follows the violations of each invariant and the convergence window
// that judges them. A violation that starts while a window is open is not a
// finding, because the instance converges after a change. A window closes when
// all invariants hold, or at the convergence timeout; violations still open at
// the timeout become findings. A window that a mutation holds also needs the
// mutation to converge and the minimum dwell to pass. Its timeout starts when
// the mutation is applied.
type tracker struct {
	timeout time.Duration
	dwell   time.Duration
	window  time.Time // zero when no window is open
	// held is when a mutation took the window, zero when none holds it;
	// applied is when the mutation was applied, zero until then, and
	// heldTimeout counts from it.
	held        time.Time
	applied     time.Time
	heldTimeout time.Duration
	violated    map[string]*violation
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

func newTracker(timeout, dwell time.Duration) *tracker {
	return &tracker{timeout: timeout, dwell: dwell, violated: map[string]*violation{}}
}

// openWindow starts a convergence window, or restarts the open one, for a
// change of the RedisFailover generation. It does not change a held window: an
// operator without the status subresource increases the generation with each
// status update, which must not extend or restart the timeout of a mutation.
func (t *tracker) openWindow(now time.Time) {
	if t.isHeld() {
		return
	}
	t.window = now
}

// extend starts or restarts a window while something outside the instance
// disturbs it, and restarts a held window's timeout once its mutation was
// applied.
func (t *tracker) extend(now time.Time) {
	if !t.isHeld() {
		t.window = now
	} else if !t.applied.IsZero() {
		t.applied = now
	}
}

// hold opens a window for a mutation, or restarts the open one, and keeps
// it open until the mutation has converged, for at most timeout after it
// was applied.
func (t *tracker) hold(now time.Time, timeout time.Duration) {
	t.window, t.held, t.applied, t.heldTimeout = now, now, time.Time{}, timeout
}

// apply starts the holding mutation's timeout.
func (t *tracker) apply(at time.Time) {
	if t.isHeld() && t.applied.IsZero() {
		t.applied = at
	}
}

func (t *tracker) isHeld() bool {
	return !t.held.IsZero()
}

func (t *tracker) windowOpen() bool {
	return !t.window.IsZero()
}

func (t *tracker) timedOut(now time.Time) bool {
	if t.isHeld() {
		return !t.applied.IsZero() && now.Sub(t.applied) >= t.heldTimeout
	}
	return now.Sub(t.window) >= t.timeout
}

// drop forgets an invariant that is no longer evaluated, and reports
// whether it was violated.
func (t *tracker) drop(invariant string) bool {
	_, ok := t.violated[invariant]
	delete(t.violated, invariant)
	return ok
}

func (t *tracker) closeWindow() {
	t.window, t.held, t.applied = time.Time{}, time.Time{}, time.Time{}
}

// update judges one round of checks. converged is whether the mutation
// holding the window, if any, has converged.
func (t *tracker) update(now time.Time, checks []check, converged bool) []event {
	var events []event
	if t.windowOpen() && t.timedOut(now) {
		events = append(events, event{kind: evWindowTimedOut, duration: now.Sub(t.window)})
		t.closeWindow()
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
	released := !t.isHeld() || converged && !t.applied.IsZero() && now.Sub(t.applied) >= t.dwell
	if t.windowOpen() && allOK && released {
		events = append(events, event{kind: evWindowClosed, duration: now.Sub(t.window)})
		t.closeWindow()
	}
	return events
}
