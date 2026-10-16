package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	stallPeriod = 100 * time.Millisecond
	stallLimit  = time.Second
)

// detectStalls logs each stall of the whole tester process until ctx is
// done. Such a stall delays the rounds of all observers at once, and the
// rounds alone do not show its cause.
func detectStalls(ctx context.Context, stalls prometheus.Counter, log *slog.Logger) {
	t := time.NewTicker(stallPeriod)
	defer t.Stop()
	watchStalls(ctx, t.C, time.Now, stalls, log)
}

// watchStalls counts a stall when a tick comes more than stallLimit after
// the tick before it and one stallPeriod. It compares the times of
// receipt, because a stalled process also fires its timers late.
func watchStalls(ctx context.Context, ticks <-chan time.Time, now func() time.Time, stalls prometheus.Counter, log *slog.Logger) {
	last := now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		t := now()
		if late := t.Sub(last) - stallPeriod; late > stallLimit {
			stalls.Inc()
			log.Warn("soak process stalled", "stall_seconds", late.Seconds())
		}
		last = t
	}
}
