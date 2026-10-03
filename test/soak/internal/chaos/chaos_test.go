package chaos

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// fakeObserver reports a quiet instance, observed now, unless failing.
type fakeObserver struct {
	mu        sync.Mutex
	failing   []string
	ephemeral bool
	volumes   bool
}

func (f *fakeObserver) Report() observer.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return observer.Report{At: time.Now(), Quiet: len(f.failing) == 0, Failing: f.failing, Ephemeral: f.ephemeral, Volumes: f.volumes}
}

type fakeData struct {
	mu       sync.Mutex
	calls    []string
	mutating bool
}

func (f *fakeData) Begin() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "begin")
	f.mutating = true
}

func (f *fakeData) Verify(_ context.Context, event string, step int, lossless bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lossless {
		event += " lossless"
	}
	f.calls = append(f.calls, "verify "+event)
	f.mutating = false
	return 0, nil
}

func testLane(t *testing.T, yaml string, instances []Instance) (*Lane, *global.Lock, *metrics.Metrics) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml + "\ninstances: [{name: a, namespace: a}]"))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New(prometheus.NewRegistry(), time.Minute)
	lock := &global.Lock{}
	l := New(cfg, nil, lock, instances, "", m, slog.New(slog.DiscardHandler))
	l.poll = 5 * time.Millisecond
	return l, lock, m
}

func TestActPausesMutators(t *testing.T) {
	data := &fakeData{}
	instances := []Instance{{Name: "a", Observer: &fakeObserver{}, Data: data}, {Name: "b", Observer: &fakeObserver{}}}
	l, lock, m := testLane(t, "chaos: {kinds: {operator_restart: 1}}", instances)

	var running atomic.Int32
	var overlaps atomic.Int32
	inAction := atomic.Bool{}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				unlock := lock.Shared()
				running.Add(1)
				if inAction.Load() {
					overlaps.Add(1)
				}
				time.Sleep(time.Millisecond)
				running.Add(-1)
				unlock()
			}
		})
	}
	var seen struct {
		running            int32
		disturbance, chaos string
		mutating           bool
	}
	l.actions[config.OperatorRestart] = func(ctx context.Context, a *action) outcome {
		inAction.Store(true)
		defer inAction.Store(false)
		a.begin()
		lock.Disturb("test")
		defer lock.Disturb("")
		for range 20 {
			seen.running = max(seen.running, running.Load())
			time.Sleep(time.Millisecond)
		}
		seen.disturbance, seen.chaos = lock.Disturbance(), lock.Chaos()
		data.mu.Lock()
		seen.mutating = data.mutating
		data.mu.Unlock()
		return outcome{converged: []time.Duration{time.Second}}
	}
	time.Sleep(10 * time.Millisecond)
	l.act(ctx, 3, stepRand(1, 3), config.OperatorRestart)
	cancel()
	wg.Wait()

	if seen.running != 0 || overlaps.Load() != 0 {
		t.Errorf("%d mutations ran during the action, %d started", seen.running, overlaps.Load())
	}
	if seen.disturbance != "test" || seen.chaos != string(config.OperatorRestart) || !seen.mutating {
		t.Errorf("during the action: %+v", seen)
	}
	if lock.Disturbance() != "" || lock.Chaos() != "" {
		t.Errorf("after the action: disturbance %q, chaos %q", lock.Disturbance(), lock.Chaos())
	}
	if !slices.Equal(data.calls, []string{"begin", "verify operator_restart lossless"}) {
		t.Errorf("data calls %v", data.calls)
	}
	if v := testutil.ToFloat64(m.ChaosTotal.WithLabelValues("operator_restart", resultConverged)); v != 1 {
		t.Errorf("chaos_total converged = %v", v)
	}
	if v := testutil.ToFloat64(m.ChaosTotal.WithLabelValues("operator_restart", resultTimeout)); v != 0 {
		t.Errorf("chaos_total timeout = %v", v)
	}
	if v := testutil.ToFloat64(m.ChaosInProgress.WithLabelValues("operator_restart")); v != 0 {
		t.Errorf("chaos_in_progress = %v after the action", v)
	}
}

// An action waits for every instance to be quiet first, and a skipped one
// verifies nothing.
func TestActWaitsForQuiet(t *testing.T) {
	busy := &fakeObserver{failing: []string{"pods"}}
	data := &fakeData{}
	l, _, m := testLane(t, "chaos: {kinds: {node_drain: 1}}", []Instance{{Name: "a", Observer: busy, Data: data}})
	started := make(chan time.Time, 1)
	l.actions[config.NodeDrain] = func(context.Context, *action) outcome {
		started <- time.Now()
		return outcome{skip: "no node to drain"}
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		busy.mu.Lock()
		busy.failing = nil
		busy.mu.Unlock()
	}()
	start := time.Now()
	l.act(context.Background(), 1, stepRand(1, 1), config.NodeDrain)
	if d := (<-started).Sub(start); d < 50*time.Millisecond {
		t.Errorf("the action started after %s, before the instance was quiet", d)
	}
	if len(data.calls) != 0 {
		t.Errorf("data calls %v for a skipped action", data.calls)
	}
	if v := testutil.ToFloat64(m.ChaosTotal.WithLabelValues("node_drain", resultSkipped)); v != 1 {
		t.Errorf("chaos_total skipped = %v", v)
	}
}

// Run takes one action at a time, each interval, and stops at stopAfter.
func TestRunSchedule(t *testing.T) {
	l, _, m := testLane(t, `
mutation: {seed: 3, stopAfter: 300ms}
chaos: {kinds: {operator_restart: 1, node_drain: 1}, interval: 40ms, jitter: 20ms}`, []Instance{{Name: "a", Observer: &fakeObserver{}}})
	var mu sync.Mutex
	var kinds []config.ChaosKind
	var active atomic.Int32
	for _, k := range []config.ChaosKind{config.OperatorRestart, config.NodeDrain} {
		l.actions[k] = func(_ context.Context, a *action) outcome {
			if active.Add(1) != 1 {
				t.Error("two actions at once")
			}
			defer active.Add(-1)
			mu.Lock()
			kinds = append(kinds, a.kind)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			return outcome{}
		}
	}
	start := time.Now()
	l.Run(context.Background())
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Errorf("ran %s, past stopAfter", d)
	}
	if len(kinds) < 3 || len(kinds) > 7 {
		t.Errorf("%d actions in 300ms at 40-60ms intervals: %v", len(kinds), kinds)
	}
	var want []config.ChaosKind
	for step := 1; step <= len(kinds); step++ {
		r := stepRand(3, step)
		r.Int64N(int64(20*time.Millisecond) + 1)
		want = append(want, config.Pick(r, l.cfg.Kinds))
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("kinds %v, want the seed's %v", kinds, want)
	}
	total := testutil.ToFloat64(m.ChaosTotal.WithLabelValues("operator_restart", resultConverged)) +
		testutil.ToFloat64(m.ChaosTotal.WithLabelValues("node_drain", resultConverged))
	if int(total) != len(kinds) {
		t.Errorf("chaos_total converged = %v for %d actions", total, len(kinds))
	}
}

func TestQuietAndSettled(t *testing.T) {
	since := time.Now()
	after := since.Add(time.Second)
	reports := map[string]observer.Report{
		"a": {At: after, Quiet: true},
		"b": {At: after, Quiet: false},
		"c": {At: after, Failing: []string{"pods", "healthy"}},
		"d": {At: since, Quiet: true},
	}
	err := quiet(reports, since)
	for _, want := range []string{"b is in a convergence window", "c violates pods, healthy", "d not observed yet"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("quiet: %v, want %q", err, want)
		}
	}
	if err != nil && strings.Contains("\n"+err.Error(), "\na ") {
		t.Errorf("quiet: %v blames a", err)
	}
	// In a window, but every invariant holds: settled.
	err = settled(reports, since)
	if err == nil || strings.Contains("\n"+err.Error(), "\nb ") || !strings.Contains(err.Error(), "c violates") {
		t.Errorf("settled: %v", err)
	}
	delete(reports, "c")
	delete(reports, "d")
	if err := settled(reports, since); err != nil {
		t.Errorf("settled: %v", err)
	}
}
