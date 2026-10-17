package observer

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// slow is the duration above which the observer logs a round or a read at
// warn level.
const slow = time.Second

// The phases of a failed read.
const (
	// phaseDial is a read without a connection: the dial failed or did not
	// end in time.
	phaseDial = "dial"
	// phaseReply is a read whose connection was made, but no reply came.
	phaseReply = "reply"
	// phaseContext is a read that the end of the round's context stopped.
	phaseContext = "context"
)

// read is one read of a pod. It records the dials of its client, because a
// read whose dial did not end in time returns only the error of its
// context.
type read struct {
	kind, pod, ip string
	start         time.Time

	mu        sync.Mutex
	dials     int
	dialStart time.Time
	// dialEnd is zero while a dial runs.
	dialEnd time.Time
	// connected is the end of the first dial that made a connection, zero
	// before.
	connected time.Time
}

func (o *Observer) startRead(kind, pod, ip string) *read {
	return &read{kind: kind, pod: pod, ip: ip, start: o.now()}
}

// dialer wraps dial. go-redis dials in a goroutine of its own, which can
// continue after the read returned.
func (r *read) dialer(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		r.mu.Lock()
		if r.dials == 0 {
			r.dialStart = time.Now()
		}
		r.dials++
		r.dialEnd = time.Time{}
		r.mu.Unlock()
		c, err := dial(ctx, network, addr)
		r.mu.Lock()
		r.dialEnd = time.Now()
		if err == nil && r.connected.IsZero() {
			r.connected = r.dialEnd
		}
		r.mu.Unlock()
		return c, err
	}
}

// phase returns the phase in which a read that ended at end failed with err.
// A connection made after end did not serve the read.
func (r *read) phase(err error, end time.Time) string {
	var opErr *net.OpError
	isOp := errors.As(err, &opErr)
	var netErr net.Error
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case errors.Is(err, context.Canceled):
		// Only the end of the round's context cancels a read.
		return phaseContext
	case isOp && opErr.Op == "dial":
		return phaseDial
	case !isOp && errors.Is(err, context.DeadlineExceeded):
		// go-redis returns the error of the context when no connection
		// came in time.
		return phaseDial
	case errors.As(err, &netErr) && netErr.Timeout():
		return phaseReply
	case !r.connected.IsZero() && !r.connected.After(end):
		return phaseReply
	default:
		return phaseDial
	}
}

// dialTime returns the time from the first dial of r to the end of its last
// dial, or to now while a dial runs.
func (r *read) dialTime() (time.Duration, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dials == 0 {
		return 0, 0
	}
	end := r.dialEnd
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(r.dialStart), r.dials
}

// endRead logs r when it failed with err or was slow.
func (o *Observer) endRead(r *read, err error) {
	d := o.now().Sub(r.start)
	if err == nil && d <= slow {
		return
	}
	phase, reason := "", ""
	if err != nil {
		phase, reason = r.phase(err, time.Now()), err.Error()
	}
	dial, dials := r.dialTime()
	o.log.Warn("observer read slow", "pod", r.pod, "ip", r.ip, "kind", r.kind, "phase", phase,
		"duration_seconds", d.Seconds(), "dial_seconds", dial.Seconds(), "dials", dials, "error", reason)
}

// timeRound records the lateness and the phases of a round. tick is when the
// round was due, collected when the reads of the pods ended.
func (o *Observer) timeRound(ctx context.Context, tick, start, collected, end time.Time, s snapshot) {
	late, d := max(start.Sub(tick), 0), end.Sub(start)
	o.roundTime.Observe(d.Seconds())
	o.lateness.Observe(late.Seconds())
	level := slog.LevelDebug
	if d > slow || late > slow {
		level = slog.LevelWarn
	}
	// Each round is due every few seconds in each instance: do not build
	// the attributes for a disabled level.
	if !o.log.Enabled(ctx, level) {
		return
	}
	msg := "observer round"
	if level == slog.LevelWarn {
		msg = "observer round slow"
	}
	o.log.Log(ctx, level, msg, "lateness_seconds", late.Seconds(), "duration_seconds", d.Seconds(),
		"kube_seconds", s.kubeTime.Seconds(), "read_seconds", s.readTime.Seconds(), "apply_seconds", end.Sub(collected).Seconds())
}
