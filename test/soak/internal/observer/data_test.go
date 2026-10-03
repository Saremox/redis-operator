package observer

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

func TestEvictionsAcrossRestarts(t *testing.T) {
	var e evictions
	all := []string{"rfr-x-0", "rfr-x-1", "rfr-x-2"}
	steps := []struct {
		name   string
		counts map[string]evictionCount
		pods   []string
		want   int64
	}{
		{"first round is the baseline", map[string]evictionCount{"rfr-x-0": {"a", 500}, "rfr-x-1": {"b", 0}}, all, 0},
		{"master evicts", map[string]evictionCount{"rfr-x-0": {"a", 700}, "rfr-x-1": {"b", 0}}, all, 200},
		{"master unreachable", map[string]evictionCount{"rfr-x-1": {"b", 0}}, all, 0},
		{"back, same process", map[string]evictionCount{"rfr-x-0": {"a", 750}, "rfr-x-1": {"b", 0}}, all, 50},
		{"restarted, counter reset", map[string]evictionCount{"rfr-x-0": {"c", 30}, "rfr-x-1": {"b", 0}}, all, 30},
		{"promoted replica evicts", map[string]evictionCount{"rfr-x-0": {"c", 30}, "rfr-x-1": {"b", 40}}, all, 40},
		{"new pod", map[string]evictionCount{"rfr-x-0": {"c", 30}, "rfr-x-1": {"b", 40}, "rfr-x-2": {"d", 5}}, all, 5},
		{"pod gone", map[string]evictionCount{"rfr-x-0": {"c", 30}, "rfr-x-1": {"b", 40}}, all[:2], 0},
		{"recreated under the same name", map[string]evictionCount{"rfr-x-0": {"c", 30}, "rfr-x-1": {"b", 40}, "rfr-x-2": {"e", 7}}, all, 7},
	}
	for _, s := range steps {
		if got := e.update(s.counts, s.pods); got != s.want {
			t.Errorf("%s: %d, want %d", s.name, got, s.want)
		}
	}
}

func TestOOMKills(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("u0")}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "redis", LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: at}}},
		{Name: "exporter", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", FinishedAt: at}}},
	}
	want := []string{"u0/redis@2026-10-01T12:00:00Z"}
	if got := oomKills(p); !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}
	// Terminated and not yet restarted: the same kill in both states.
	p.Status.ContainerStatuses[0].State = p.Status.ContainerStatuses[0].LastTerminationState
	if got := oomKills(p); !slices.Equal(got, want) {
		t.Errorf("%v, want %v", got, want)
	}

	o, _ := newTestObserver(t)
	o.started = at.Add(-time.Minute)
	s := healthy(true)
	o.apply(at.Time, s, 1, false)
	s.redis[1].OOMKills = want
	// Inside a convergence window too, and only once.
	o.tracker.openWindow(at.Time)
	o.apply(at.Add(5*time.Second), s, 1, false)
	o.apply(at.Add(10*time.Second), s, 1, false)
	if n := testutil.ToFloat64(o.findings.WithLabelValues(invOOMKilled)); n != 1 {
		t.Errorf("findings = %v, want 1", n)
	}
	if v := testutil.ToFloat64(o.ok.WithLabelValues(invOOMKilled)); v != 0 {
		t.Errorf("invariant_ok = %v, want 0", v)
	}
	s.redis[1].OOMKills = nil
	o.apply(at.Add(15*time.Second), s, 1, false)
	if v := testutil.ToFloat64(o.ok.WithLabelValues(invOOMKilled)); v != 1 {
		t.Errorf("invariant_ok = %v after the pod was replaced, want 1", v)
	}
}

// A restarted tester sees the kills that its earlier run counted, in the
// last state of a container. They are not findings again.
func TestOOMKillBeforeStart(t *testing.T) {
	o, _ := newTestObserver(t)
	at := o.started.Add(-time.Hour).UTC().Format(time.RFC3339)
	s := healthy(true)
	s.redis[1].OOMKills = []string{"u0/redis@" + at}
	o.apply(o.started, s, 1, false)
	if n := testutil.ToFloat64(o.findings.WithLabelValues(invOOMKilled)); n != 0 {
		t.Errorf("findings = %v, want 0", n)
	}
	if v := testutil.ToFloat64(o.ok.WithLabelValues(invOOMKilled)); v != 1 {
		t.Errorf("invariant_ok = %v, want 1", v)
	}
}

func TestEvaluateConfig(t *testing.T) {
	withMaxMemory := func(policy string) snapshot {
		s := healthy(false)
		s.rf = &redisfailoverv1.RedisFailover{Spec: redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{
			MaxMemory: &redisfailoverv1.MaxMemorySettings{Policy: policy},
		}}}
		s.rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("192Mi")}
		_ = s.rf.Validate()
		for i := range s.redis {
			s.redis[i].limit = 192 << 20
			s.redis[i].config = map[string]string{"maxmemory": "150994944", "maxmemory-policy": policy, "replica-priority": "100"}
		}
		return s
	}
	if v := violated(evaluate(withMaxMemory("noeviction"))); v != nil {
		t.Errorf("applied: %v", v)
	}
	s := withMaxMemory("noeviction")
	s.redis[2].config["maxmemory"] = "0"
	if v := violated(evaluate(s)); !slices.Equal(v, []string{invConfig}) {
		t.Errorf("unset on a pod: %v", v)
	}
	s = withMaxMemory("noeviction")
	s.rf.Spec.Redis.MaxMemory.Policy = "volatile-lru"
	if err := s.checkConfig(); err == nil || !strings.Contains(err.Error(), "want volatile-lru") {
		t.Errorf("policy: %v", err)
	}
	// A pod that isn't scheduled yet has no IP and isn't configured.
	s = withMaxMemory("noeviction")
	s.redis[2].IP, s.redis[2].config = "", nil
	if err := s.checkConfig(); err != nil {
		t.Errorf("pod without IP: %v", err)
	}
	if v := violated(evaluate(healthy(false))); v != nil {
		t.Errorf("without maxMemory: %v", v)
	}
}

func TestObserveData(t *testing.T) {
	o, _ := newTestObserver(t)
	s := healthy(false)
	s.redis[0].info["used_memory"] = "1048576"
	s.redis[0].info["maxmemory"] = "2097152"
	s.redis[0].info["db0"] = "keys=42,expires=40,avg_ttl=1000"
	s.redis[0].info["run_id"] = "a"
	s.redis[0].info["evicted_keys"] = "10"
	o.observeData(s)
	s.redis[0].info["evicted_keys"] = "15"
	o.observeData(s)
	for name, c := range map[string]float64{
		"keys": testutil.ToFloat64(o.datasetKeys), "used": testutil.ToFloat64(o.usedMemory),
		"max": testutil.ToFloat64(o.maxMemory), "evicted": testutil.ToFloat64(o.evicted),
	} {
		want := map[string]float64{"keys": 42, "used": 1 << 20, "max": 2 << 20, "evicted": 5}[name]
		if c != want {
			t.Errorf("%s = %v, want %v", name, c, want)
		}
	}
}
