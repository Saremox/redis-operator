package prober

import "time"

// outage tracks the window from the first failed probe to the next
// successful one.
type outage struct {
	start time.Time
	// event is what ran when the outage started.
	event string
}

// fail records a failed probe and reports whether it started an outage.
func (o *outage) fail(now time.Time) bool {
	if !o.start.IsZero() {
		return false
	}
	o.start = now
	return true
}

// succeed records a successful probe and returns the length of the outage
// it ended, if any.
func (o *outage) succeed(now time.Time) (time.Duration, bool) {
	if o.start.IsZero() {
		return 0, false
	}
	d := now.Sub(o.start)
	o.start = time.Time{}
	return d, true
}
