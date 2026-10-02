// Package chaos runs the global chaos lane: cluster-level actions that
// disturb every instance at once, one at a time. While an action runs,
// every mutator is paused and every instance is in a convergence window.
package chaos

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// Results, the values of chaos_total's result label.
const (
	resultConverged = "converged"
	resultTimeout   = "timeout"
	resultFailed    = "failed"
	resultSkipped   = "skipped"
)

// Watched is what the lane reads of an instance's observer.
type Watched interface {
	Report() observer.Report
}

// Data is what the lane needs of an instance's data.
type Data interface {
	Begin()
	Verify(ctx context.Context, event string, step int) (int, error)
}

// Instance is an instance every action disturbs.
type Instance struct {
	Name     string
	Observer Watched
	// Data is nil for an instance without data.
	Data Data
}

type Lane struct {
	cfg       config.Chaos
	operator  config.Operator
	seed      int64
	stopAfter time.Duration
	kube      kubernetes.Interface
	lock      *global.Lock
	instances []Instance
	// node is the tester's own node, which is never drained.
	node string
	helm func(ctx context.Context, args ...string) ([]byte, error)
	// actions run each kind, and poll is how often their waits check.
	actions map[config.ChaosKind]func(context.Context, *action) outcome
	poll    time.Duration
	log     *slog.Logger

	total           *prometheus.CounterVec
	converge        *prometheus.HistogramVec
	inProgress      *prometheus.GaugeVec
	operatorDown    *prometheus.HistogramVec
	evictionBlocked prometheus.Histogram
}

// New returns the chaos lane. node is the node the tester runs on.
func New(cfg *config.Config, kube kubernetes.Interface, lock *global.Lock, instances []Instance, node string, m *metrics.Metrics, log *slog.Logger) *Lane {
	l := &Lane{
		cfg:       cfg.Chaos,
		operator:  cfg.Operator,
		seed:      cfg.Mutation.Seed,
		stopAfter: cfg.Mutation.StopAfter.Duration,
		kube:      kube,
		lock:      lock,
		instances: instances,
		node:      node,
		poll:      time.Second,
		log:       log.With("lane", "chaos", "seed", cfg.Mutation.Seed),

		total:           m.ChaosTotal,
		converge:        m.ChaosConverge,
		inProgress:      m.ChaosInProgress,
		operatorDown:    m.ChaosOperatorDown,
		evictionBlocked: m.ChaosEvictionBlocked,
	}
	l.helm = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, l.cfg.Upgrade.Helm, args...).CombinedOutput()
	}
	l.actions = map[config.ChaosKind]func(context.Context, *action) outcome{
		config.OperatorRestart: l.restart,
		config.OperatorUpgrade: l.upgrade,
		config.NodeDrain:       l.drain,
	}
	// Alerts take the increase of chaos_total, which a series that first
	// appears at 1 wouldn't show.
	for _, k := range l.cfg.Sorted() {
		l.inProgress.WithLabelValues(string(k)).Set(0)
		for _, r := range []string{resultConverged, resultTimeout, resultFailed, resultSkipped} {
			l.total.WithLabelValues(string(k), r)
		}
	}
	return l
}

// stepRand returns the random source of one step of the lane, apart from
// every instance's.
func stepRand(seed int64, step int) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte("chaos"))
	return rand.New(rand.NewPCG(uint64(seed)^h.Sum64(), uint64(step)))
}

func pickKind(r *rand.Rand, kinds map[config.ChaosKind]int) config.ChaosKind {
	sorted := slices.Sorted(maps.Keys(kinds))
	total := 0
	for _, k := range sorted {
		total += kinds[k]
	}
	n := r.IntN(total)
	for _, k := range sorted {
		if n < kinds[k] {
			return k
		}
		n -= kinds[k]
	}
	panic("unreachable")
}

// Run takes an action every interval plus up to jitter, until
// mutation.stopAfter.
func (l *Lane) Run(ctx context.Context) {
	stop := time.Now().Add(l.stopAfter)
	for step := 1; ; step++ {
		r := stepRand(l.seed, step)
		d := l.cfg.Interval.Duration + time.Duration(r.Int64N(int64(l.cfg.Jitter.Duration)+1))
		if l.stopAfter > 0 && time.Now().Add(d).After(stop) {
			l.log.Info("chaos stopped", "steps", step-1)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
		l.act(ctx, step, r, pickKind(r, l.cfg.Kinds))
	}
}

// action is one action in progress.
type action struct {
	kind config.ChaosKind
	step int
	r    *rand.Rand
	log  *slog.Logger
	// begin marks the start of the disruption for every instance's data;
	// an action calls it once it is sure to disturb them.
	begin func()
}

// outcome is what an action did.
type outcome struct {
	// skip is why it wasn't taken; err why it failed; timeout what didn't
	// converge in time.
	skip    string
	err     error
	timeout error
	// converged are the times from each disruption to every instance's
	// convergence.
	converged []time.Duration
}

func (o outcome) result() string {
	switch {
	case o.skip != "":
		return resultSkipped
	case o.err != nil:
		return resultFailed
	case o.timeout != nil:
		return resultTimeout
	}
	return resultConverged
}

// act takes one action: it pauses every mutator, waits until every
// instance is quiet, runs the action, and verifies every instance's data
// after it.
func (l *Lane) act(ctx context.Context, step int, r *rand.Rand, kind config.ChaosKind) {
	log := l.log.With("kind", kind, "step", step)
	log.Info("waiting for every mutation to finish")
	unlock := l.lock.Exclusive()
	defer unlock()
	// A change in flight when the action starts would be another's: the
	// lock and this wait should leave none.
	inFlight := l.waitQuiet(ctx)
	if ctx.Err() != nil {
		return
	}
	l.lock.SetChaos(string(kind))
	inProgress := l.inProgress.WithLabelValues(string(kind))
	inProgress.Set(1)
	defer func() {
		inProgress.Set(0)
		l.lock.SetChaos("")
	}()
	began := false
	a := &action{kind: kind, step: step, r: r, log: log, begin: func() {
		if began {
			return
		}
		began = true
		for _, in := range l.instances {
			if in.Data != nil {
				in.Data.Begin()
			}
		}
	}}
	log.Info("chaos", "in_flight", inFlight)
	start := time.Now()
	o := l.actions[kind](ctx, a)
	if ctx.Err() != nil {
		return
	}
	result := o.result()
	l.total.WithLabelValues(string(kind), result).Inc()
	for _, d := range o.converged {
		l.converge.WithLabelValues(string(kind)).Observe(d.Seconds())
	}
	log = log.With("result", result, "duration_seconds", time.Since(start).Seconds())
	switch result {
	case resultSkipped:
		log.Info("chaos skipped", "reason", o.skip)
	case resultFailed:
		log.Warn("chaos done", "error", o.err.Error())
	case resultTimeout:
		log.Warn("chaos done", "error", o.timeout.Error())
	default:
		log.Info("chaos done")
	}
	if began {
		l.verify(ctx, kind, step, log)
	}
}

// waitQuiet waits until every instance is quiet, for at most the timeout,
// and returns those that weren't.
func (l *Lane) waitQuiet(ctx context.Context) []string {
	since := time.Now()
	err := l.await(ctx, func(context.Context) error { return quiet(l.reports(), since) })
	if err == nil {
		return nil
	}
	var names []string
	for name, r := range l.reports() {
		if !r.Quiet {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// verify verifies every instance's data after the action, event being its
// kind.
func (l *Lane) verify(ctx context.Context, kind config.ChaosKind, step int, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, l.cfg.Timeout.Duration)
	defer cancel()
	var mu sync.Mutex
	lost := map[string]int{}
	var wg sync.WaitGroup
	for _, in := range l.instances {
		if in.Data == nil {
			continue
		}
		wg.Go(func() {
			n, err := in.Data.Verify(ctx, string(kind), step)
			if err != nil {
				log.Warn("verifying the data after chaos", "rf", in.Name, "error", err.Error())
				return
			}
			mu.Lock()
			lost[in.Name] = n
			mu.Unlock()
		})
	}
	wg.Wait()
	log.Info("chaos verified", "lost", lost)
}

func (l *Lane) reports() map[string]observer.Report {
	out := make(map[string]observer.Report, len(l.instances))
	for _, in := range l.instances {
		out[in.Name] = in.Observer.Report()
	}
	return out
}

// await polls f until it returns nil, for at most the timeout, and returns
// its last error then.
func (l *Lane) await(ctx context.Context, f func(context.Context) error) error {
	return l.awaitFor(ctx, l.cfg.Timeout.Duration, f)
}

func (l *Lane) awaitFor(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	deadline := time.Now().Add(d)
	for {
		err := f(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not within %s: %w", d, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.poll):
		}
	}
}

// quiet holds once every instance was observed after since with no
// convergence window open and every invariant holding.
func quiet(reports map[string]observer.Report, since time.Time) error {
	return judge(reports, since, func(r observer.Report) bool { return r.Quiet })
}

// settled holds once every instance was observed after since with every
// invariant holding, in a window or not.
func settled(reports map[string]observer.Report, since time.Time) error {
	return judge(reports, since, func(r observer.Report) bool { return len(r.Failing) == 0 })
}

func judge(reports map[string]observer.Report, since time.Time, ok func(observer.Report) bool) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		r := reports[name]
		switch {
		case !r.At.After(since):
			errs = append(errs, fmt.Errorf("%s not observed yet", name))
		case ok(r):
		case len(r.Failing) > 0:
			errs = append(errs, fmt.Errorf("%s violates %s", name, strings.Join(r.Failing, ", ")))
		default:
			errs = append(errs, fmt.Errorf("%s is in a convergence window", name))
		}
	}
	return errors.Join(errs...)
}
