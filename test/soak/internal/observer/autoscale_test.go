package observer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/autoscale"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

func newAutoscaleObserver(t *testing.T) (*Observer, *prometheus.Registry) {
	t.Helper()
	cfg, err := config.Parse([]byte("instances: [{name: x, namespace: ns, maxMemoryPolicy: noeviction, autoscale: {}}]"))
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	o := New(cfg.Instances[0], cfg, nil, nil, nil, &global.Lock{}, metrics.New(reg, time.Minute), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return o, reg
}

// autoscaled is a healthy snapshot of an instance in mode Auto with a step
// in phase, and the limit 128Mi.
func autoscaled(phase, uid string) snapshot {
	s := healthy(false)
	s.rf = &redisfailoverv1.RedisFailover{}
	s.rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
	s.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: "Auto", MemoryMax: new(resource.MustParse("512Mi"))}
	if phase != "" {
		s.rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{Phase: phase, LastStep: "2026-10-01T12:00:00Z", EffectiveMemory: "192Mi"}
	}
	for i := range s.redis {
		s.redis[i].limit = 128 << 20
		s.redis[i].config = map[string]string{"maxmemory": "47185920"}
	}
	s.redis[1].UID = uid
	return s
}

func TestAutoscaleStep(t *testing.T) {
	o, _ := newAutoscaleObserver(t)
	finding := func(judge string) float64 {
		return testutil.ToFloat64(o.findings.WithLabelValues(judge))
	}
	started := func() float64 { return testutil.ToFloat64(o.steps.WithLabelValues(autoscale.ResultStarted)) }

	o.apply(at(0), autoscaled("", "u1"), 1, false)
	o.apply(at(5), autoscaled("", "u1"), 1, false)
	if o.tracker.windowOpen() {
		t.Fatal("the first window is still open")
	}
	// A step opens a window, so a violation in the step is no finding.
	applying := autoscaled(autoscale.PhaseApplying, "u1")
	applying.state = "NotHealthy"
	o.apply(at(10), applying, 1, false)
	if started() != 1 || finding(invHealthy) != 0 {
		t.Fatalf("started %v, healthy findings %v", started(), finding(invHealthy))
	}
	// A recreate during the step without a user path.
	o.apply(at(15), autoscaled(autoscale.PhaseApplying, "u1-new"), 1, false)
	if n := finding(autoscale.JudgeRestart); n != 1 {
		t.Errorf("autoscale_restart findings %v", n)
	}
	// A mutation that may recreate a pod.
	allowed := autoscale.Allow{Recreate: true}
	o.SetMutation(func() (autoscale.Allow, bool) { return allowed, true })
	o.apply(at(20), autoscaled(autoscale.PhaseApplying, "u1-third"), 1, false)
	// A change by hand may change each pod.
	o.SetMutation(func() (autoscale.Allow, bool) { return autoscale.Allow{}, false })
	o.apply(at(25), autoscaled(autoscale.PhaseApplying, "u1-fourth"), 2, false)
	if n := finding(autoscale.JudgeRestart); n != 1 {
		t.Errorf("autoscale_restart findings %v after the user paths", n)
	}
	done := autoscaled(autoscale.PhaseDone, "u1-fourth")
	o.apply(at(30), done, 2, false)
	if n := testutil.ToFloat64(o.steps.WithLabelValues(autoscale.ResultDone)); n != 1 || o.tracker.windowOpen() {
		t.Errorf("done %v, window open %t", n, o.tracker.windowOpen())
	}
	if n := finding(autoscale.JudgeSize) + finding(autoscale.JudgeShrink); n != 0 {
		t.Errorf("other findings %v", n)
	}
	// After the step, the same violation is a finding.
	unhealthy := autoscaled(autoscale.PhaseDone, "u1-fourth")
	unhealthy.state = "NotHealthy"
	o.apply(at(35), unhealthy, 2, false)
	if n := finding(invHealthy); n != 1 {
		t.Errorf("healthy findings %v after the step", n)
	}
}

// A step that stays Applying does not hold the window of a mutation for
// ever: after the step timeout the window times out, and the mutation ends.
func TestAutoscaleStepReleasesHold(t *testing.T) {
	for _, c := range []struct {
		name  string
		phase func(minute int) string
		// timedOut is whether the window times out within 40 minutes.
		timedOut bool
	}{
		{"a step that never ends", func(int) string { return autoscale.PhaseApplying }, true},
		{"a step that ends", func(m int) string {
			if m < 3 {
				return autoscale.PhaseApplying
			}
			return autoscale.PhaseDone
		}, false},
	} {
		o, _ := newAutoscaleObserver(t)
		base := time.Now().Truncate(time.Minute)
		round := func(m int) {
			s := autoscaled(c.phase(m), "u1")
			s.rf.Status.Autoscaling.LastStep = base.UTC().Format(time.RFC3339)
			o.takeHold()
			o.apply(base.Add(time.Duration(m)*time.Minute), s, 1, o.converged(context.Background()))
		}
		o.apply(base.Add(-time.Minute), autoscaled("", "u1"), 1, false)
		h := o.Hold(10*time.Minute, func(context.Context) error { return errors.New("the step is Applying") })
		o.takeHold()
		o.tracker.apply(base)
		ended := -1
		for m := 0; m <= 40 && ended < 0; m++ {
			round(m)
			select {
			case <-h.Done():
				ended = m
			default:
			}
		}
		switch {
		case c.timedOut && (ended < 18 || ended > 30):
			t.Errorf("%s: the window ended at minute %d", c.name, ended)
		case !c.timedOut && ended >= 0 && ended < 12:
			t.Errorf("%s: the window ended at minute %d, before the timeout of the mutation", c.name, ended)
		}
		if got := testutil.ToFloat64(o.findings.WithLabelValues(autoscale.JudgeTimeout)); got != map[bool]float64{true: 1, false: 0}[c.timedOut] {
			t.Errorf("%s: step timeout findings %v", c.name, got)
		}
	}
}

// A mutation that may recreate but not shrink a pod, for example a higher
// spec limit, does not excuse a lower pod limit, also in the next round.
func TestAutoscaleRaiseDoesNotExcuseShrink(t *testing.T) {
	o, _ := newAutoscaleObserver(t)
	// The first round is a change of the generation, which may do all of it.
	o.apply(at(0), autoscaled("", "u1"), 1, false)
	o.apply(at(5), autoscaled("", "u1"), 1, false)
	o.SetMutation(func() (autoscale.Allow, bool) { return autoscale.Allow{Recreate: true}, true })
	lowered := autoscaled("", "u1")
	lowered.redis[1].limit = 64 << 20
	o.apply(at(10), lowered, 2, false)
	if n := testutil.ToFloat64(o.findings.WithLabelValues(autoscale.JudgeShrink)); n != 1 {
		t.Errorf("autoscale_shrink findings %v", n)
	}
}

// The first round of the observer, for example after a restart of the
// tester, is no user change. A change of the generation after it is.
func TestAutoscaleFirstRoundIsNoUserChange(t *testing.T) {
	recreated := func(uid string, generation int64, want float64) {
		t.Helper()
		o, _ := newAutoscaleObserver(t)
		o.apply(at(0), autoscaled(autoscale.PhaseApplying, "u1"), 3, false)
		o.apply(at(5), autoscaled(autoscale.PhaseApplying, uid), generation, false)
		if n := testutil.ToFloat64(o.findings.WithLabelValues(autoscale.JudgeRestart)); n != want {
			t.Errorf("generation 3 -> %d: autoscale_restart findings %v, want %v", generation, n, want)
		}
	}
	recreated("u1-new", 3, 1)
	recreated("u1-new", 4, 0)
}

// A kill in a step excuses only the killed pod.
func TestAutoscaleKillExcusesThePod(t *testing.T) {
	for _, c := range []struct {
		killed string
		want   float64
	}{{"rfr-x-1", 0}, {"rfr-x-2", 1}, {"", 0}} {
		o, _ := newAutoscaleObserver(t)
		o.apply(at(0), autoscaled(autoscale.PhaseApplying, "u1"), 1, false)
		o.apply(at(5), autoscaled(autoscale.PhaseApplying, "u1"), 1, false)
		o.SetMutation(func() (autoscale.Allow, bool) { return autoscale.Allow{Recreate: true, Pod: c.killed}, true })
		o.apply(at(10), autoscaled(autoscale.PhaseApplying, "u1-new"), 1, false)
		if n := testutil.ToFloat64(o.findings.WithLabelValues(autoscale.JudgeRestart)); n != c.want {
			t.Errorf("killed %q, rfr-x-1 recreated: autoscale_restart findings %v, want %v", c.killed, n, c.want)
		}
	}
}

// A chaos action may change each pod.
func TestAutoscaleChaos(t *testing.T) {
	o, _ := newAutoscaleObserver(t)
	if a := o.allowed(false); a != (autoscale.Allow{}) {
		t.Errorf("without a user path: %+v", a)
	}
	o.lock.SetChaos("node_drain")
	if a := o.allowed(false); a != autoscale.AllowAll {
		t.Errorf("chaos: %+v", a)
	}
	o.lock.SetChaos("")
	o.lock.Disturb("operator stopped")
	if a := o.allowed(false); a != autoscale.AllowAll {
		t.Errorf("disturbance: %+v", a)
	}
}

// An instance without autoscale has no judge and no autoscale series.
func TestNoAutoscale(t *testing.T) {
	o, reg := newTestObserver(t)
	o.apply(at(0), autoscaled(autoscale.PhaseApplying, "u1"), 1, false)
	if o.judge != nil || hasSeries(t, reg, "redis_soak_autoscale_steps_total", "result", autoscale.ResultStarted) {
		t.Error("an instance without autoscale has a judge")
	}
}

// The round carries the restart count of the redis container.
func TestCollectRestarts(t *testing.T) {
	cfg, err := config.Parse([]byte("instances: [{name: x, namespace: ns}]"))
	if err != nil {
		t.Fatal(err)
	}
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "rfr-x-0", Namespace: "ns", Labels: map[string]string{
			"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": "x", "app.kubernetes.io/component": "redis",
		}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "exporter", RestartCount: 5}, {Name: "redis", RestartCount: 2}}},
	}
	o := New(cfg.Instances[0], cfg, fake.NewClientset(p), rffake.NewSimpleClientset(rf),
		auth.New(nil), nil, metrics.New(prometheus.NewRegistry(), time.Minute), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s, _, err := o.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.redis) != 1 || s.redis[0].restarts != 2 {
		t.Errorf("%+v", s.redis)
	}
	if r := autoscaleRound(at(0), s, 1, autoscale.Allow{}); len(r.Pods) != 1 || r.Pods[0].Restarts != 2 {
		t.Errorf("round %+v", r.Pods)
	}
}
