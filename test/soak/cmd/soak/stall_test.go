package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A tick more than 1 second after the tick before it and one period is a
// stall: it counts, and logs its length. A tick 1 second late is no stall.
func TestWatchStalls(t *testing.T) {
	start := time.Unix(1000, 0)
	// The first time is the start, each other time is the receipt of a tick.
	times := make(chan time.Time, 5)
	for _, d := range []time.Duration{0, 100 * time.Millisecond, 1300 * time.Millisecond, 2400 * time.Millisecond, 2500 * time.Millisecond} {
		times <- start.Add(d)
	}
	ticks := make(chan time.Time)
	stalls := prometheus.NewCounter(prometheus.CounterOpts{Name: "stalls_total", Help: "Stalls of the test."})
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { watchStalls(ctx, ticks, func() time.Time { return <-times }, stalls, log) })
	for range 4 {
		ticks <- time.Time{}
	}
	cancel()
	wg.Wait()
	if got := testutil.ToFloat64(stalls); got != 1 {
		t.Errorf("stalls %v, want 1", got)
	}
	if got := buf.String(); strings.Count(got, "soak process stalled") != 1 || !strings.Contains(got, "level=WARN") || !strings.Contains(got, "stall_seconds=1.1") {
		t.Errorf("log %q", got)
	}
}

// detectStalls returns when ctx is done.
func TestDetectStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	detectStalls(ctx, prometheus.NewCounter(prometheus.CounterOpts{Name: "stalls_total", Help: "Stalls of the test."}), slog.New(slog.DiscardHandler))
}
