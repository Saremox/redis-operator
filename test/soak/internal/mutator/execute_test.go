package mutator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/instances"
)

// recordedHold is a window that keeps its timeout and check, and asks the
// check once before the mutation is applied. It ends with converged at
// Applied unless never is set.
type recordedHold struct {
	timeout   time.Duration
	check     func(context.Context) error
	early     error
	never     bool
	converged bool
	done      chan bool
	once      sync.Once
}

func (h *recordedHold) Applied() {
	h.once.Do(func() {
		if !h.never {
			h.done <- h.converged
		}
	})
}

func (h *recordedHold) Done() <-chan bool { return h.done }

// executeEnv is a rotation whose windows are recordedHolds, and the ctx of
// its run.
func executeEnv(t *testing.T, never, converged bool) (*rotation, context.Context, *[]*recordedHold) {
	t.Helper()
	r, ctx := newRotation(t, redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState})
	r.m.observer = &stubObserver{}
	var holds []*recordedHold
	r.m.hold = func(d time.Duration, check func(context.Context) error) holdHandle {
		h := &recordedHold{timeout: d, check: check, never: never, converged: converged, done: make(chan bool, 1)}
		h.early = check(ctx)
		holds = append(holds, h)
		return h
	}
	return r, ctx, &holds
}

const executeTimeout = 40 * time.Millisecond

func withTimeout(r *rotation, kind config.Kind) {
	r.m.cfg.Timeouts = map[config.Kind]config.Timeout{kind: {Base: metav1.Duration{Duration: executeTimeout}}}
}

// execute holds the window for the timeout, twice that for an edge that may
// fail, and ends the wait at a bound for an edge that may fail and a reset.
func TestExecuteWindow(t *testing.T) {
	flip := &transition{edge: config.Edge{From: "a", To: "b", Expect: config.ExpectOK}}
	ok := &transition{edge: config.Edge{From: "a", To: "c", Expect: config.ExpectOK}}
	fails := &transition{edge: config.Edge{From: "a", To: "d", Expect: config.ExpectFail}}
	denied := func(context.Context) error { return errors.New("denied") }
	cases := []struct {
		name   string
		p      plan
		never  bool
		hold   time.Duration
		mixed  *transition
		result string
		least  time.Duration
	}{
		{name: "plain", p: plan{kind: config.KillMaster}, hold: executeTimeout, result: resultConverged},
		{name: "flip", p: plan{kind: config.SentinelImageFlip, flip: flip}, hold: executeTimeout, mixed: flip, result: resultConverged},
		{name: "ok edge", p: plan{kind: config.ImageUpgrade, edge: ok, action: denied}, hold: executeTimeout, mixed: ok, result: resultRejected},
		{name: "flip and edge", p: plan{kind: config.ImageUpgrade, flip: flip, edge: ok, action: denied}, hold: executeTimeout, mixed: ok, result: resultRejected},
		{name: "edge that may fail", p: plan{kind: config.ImageUpgrade, edge: fails, action: denied}, never: true,
			hold: 2 * executeTimeout, mixed: fails, result: resultRejected, least: executeTimeout},
		{name: "reset", p: plan{kind: config.Reset}, never: true, hold: executeTimeout, result: resultTimeout, least: 2 * executeTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ctx, holds := executeEnv(t, c.never, true)
			withTimeout(r, c.p.kind)
			c.p.converged = func(state) error { return nil }
			start := time.Now()
			if next := r.m.execute(ctx, 1, authState(), c.p, r.m.log); next != "" {
				t.Errorf("next %q", next)
			}
			if d := time.Since(start); d < c.least {
				t.Errorf("returned after %s, want at least %s", d, c.least)
			}
			if len(*holds) != 1 || (*holds)[0].timeout != c.hold {
				t.Fatalf("holds %+v, want one of %s", *holds, c.hold)
			}
			if got := r.m.mixed; c.mixed == nil && got != nil || c.mixed != nil && (got == nil || got.t != c.mixed) {
				t.Errorf("mixed %+v, want %+v", got, c.mixed)
			}
			if n := testutil.ToFloat64(r.m.total.WithLabelValues(string(c.p.kind), c.result)); n != 1 {
				t.Errorf("%s %v", c.result, n)
			}
			wantFinding := 0.0
			if c.p.kind == config.Reset {
				wantFinding = 1
			}
			if n := testutil.ToFloat64(r.m.findings.WithLabelValues(invResetIncomplete)); n != wantFinding {
				t.Errorf("findings %v, want %v", n, wantFinding)
			}
			if r.m.Current() != "" {
				t.Errorf("current %q", r.m.Current())
			}
			if _, ok := r.m.Allowed(); ok {
				t.Error("allowed after the mutation")
			}
			if n := testutil.ToFloat64(r.m.inProgress.WithLabelValues(string(c.p.kind))); n != 0 {
				t.Errorf("in progress %v", n)
			}
		})
	}
}

// The check of the window waits until the mutation is applied, accepts a
// rejected mutation, then asks the probe, reads the instance and asks the
// plan, in that order.
func TestExecuteCheck(t *testing.T) {
	probeErr, convErr := errors.New("probe"), errors.New("not yet")
	cases := []struct {
		name     string
		action   func(context.Context) error
		probe    func(context.Context) error
		failRead bool
		want     string
		reads    int
	}{
		{name: "rejected", action: func(context.Context) error { return errors.New("denied") }, want: "<nil>"},
		{name: "probe first", probe: func(context.Context) error { return probeErr }, failRead: true, want: "probe"},
		{name: "read fails", probe: func(context.Context) error { return nil }, failRead: true, want: "read", reads: 1},
		{name: "plan", want: "not yet", reads: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ctx, holds := executeEnv(t, false, true)
			p := plan{kind: config.KillMaster, action: c.action, probe: c.probe, converged: func(state) error { return convErr }}
			if c.action == nil {
				p.action = func(context.Context) error { return nil }
			}
			r.m.execute(ctx, 1, authState(), p, r.m.log)
			h := (*holds)[0]
			if h.early == nil || h.early.Error() != "still being applied" {
				t.Errorf("before the mutation: %v", h.early)
			}
			reads := 0
			r.rfs.(interface {
				PrependReactor(string, string, k8stesting.ReactionFunc)
			}).PrependReactor("get", "redisfailovers", func(k8stesting.Action) (bool, runtime.Object, error) {
				reads++
				if c.failRead {
					return true, nil, errors.New("read")
				}
				return false, nil, nil
			})
			got := "<nil>"
			if err := h.check(ctx); err != nil {
				got = err.Error()
			}
			if got != c.want || reads != c.reads {
				t.Errorf("check %s after %d reads, want %s after %d", got, reads, c.want, c.reads)
			}
		})
	}
}

// A mutation step whose expectation did not hold is a timeout with its error.
func TestExecuteExpectation(t *testing.T) {
	r, ctx, _ := executeEnv(t, false, false)
	p := plan{kind: config.KillMaster, action: func(context.Context) error { return expectation{errors.New("no failover")} },
		converged: func(state) error { return nil }}
	r.m.execute(ctx, 1, authState(), p, r.m.log)
	if n := testutil.ToFloat64(r.m.total.WithLabelValues(string(config.KillMaster), resultTimeout)); n != 1 {
		t.Errorf("timeouts %v", n)
	}
	if line := r.logged("mutation done"); !strings.Contains(line, `"error":"no failover"`) || !strings.Contains(line, `"result":"timeout"`) ||
		!strings.Contains(line, `"level":"WARN"`) {
		t.Errorf("log %s", line)
	}
}

// A version change that did not converge is judged, not counted, and resets
// an instance with a template next.
func TestExecuteJudgeReset(t *testing.T) {
	fails := &transition{edge: config.Edge{From: "a", To: "d", Expect: config.ExpectFail}}
	for name, inst := range map[string]*instances.Instance{"template": new(instances.Instance), "no template": nil} {
		t.Run(name, func(t *testing.T) {
			r, ctx, _ := executeEnv(t, true, false)
			withTimeout(r, config.ImageUpgrade)
			r.m.instance = inst
			p := plan{kind: config.ImageUpgrade, edge: fails, action: func(context.Context) error { return nil },
				converged: func(state) error { return nil }}
			next := r.m.execute(ctx, 1, authState(), p, r.m.log)
			want, why := config.Kind(""), ""
			if inst != nil {
				want, why = config.Reset, "after a -> d didn't converge"
			}
			if next != want || r.m.resetWhy != why {
				t.Errorf("next %q, why %q", next, r.m.resetWhy)
			}
			if n := testutil.ToFloat64(r.m.total.WithLabelValues(string(config.ImageUpgrade), resultTimeout)); n != 0 {
				t.Errorf("timeouts %v", n)
			}
			if n := testutil.ToFloat64(r.m.transitions.WithLabelValues("a", "d", config.ExpectFail, transitionFailedUnsafe)); n != 1 {
				t.Errorf("transitions %v", n)
			}
		})
	}
}
