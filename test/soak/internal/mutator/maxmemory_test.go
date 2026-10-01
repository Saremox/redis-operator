package mutator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
)

// maxMemoryState is testState with maxMemory at 75% under policy, a 192Mi
// limit and a 64Mi request, applied on every pod.
func maxMemoryState(policy string) state {
	s := testState()
	resources := corev1.ResourceRequirements{
		Requests: quantities("cpu", "50m", "memory", "64Mi"),
		Limits:   quantities("memory", "192Mi"),
	}
	s.rf.Spec.Redis.Resources = resources
	s.rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: policy}
	s.rf.Status.State = redisfailoverv1.HealthyState
	for i := range s.redis {
		s.redis[i].Spec.Containers[0].Resources = resources
		s.redis[i].Status.ContainerStatuses[0].Resources = resources.DeepCopy()
		s.config = append(s.config, maxmem.Pod{Name: s.redis[i].Name, Limit: 192 * mi, MaxMemory: 144 * mi, Policy: policy})
	}
	return s
}

func noeviction() state { return maxMemoryState("noeviction") }

func maxMemoryMutations() config.Mutations {
	return config.Mutations{
		RedisMemory:       config.Range{Min: 96, Max: 320},
		MaxMemoryPolicies: []string{"noeviction", "volatile-lru", "volatile-ttl"},
		MaxMemoryPercent:  config.Range{Min: 10, Max: 95},
		FillBurstHold:     metav1.Duration{Duration: time.Second},
	}
}

type fakeData struct {
	burst    func() (string, error)
	writable error
}

func (fakeData) Filled() bool                                           { return true }
func (fakeData) Begin()                                                 {}
func (fakeData) Verify(context.Context, config.Kind, int)               {}
func (f fakeData) Writable(context.Context) error                       { return f.writable }
func (f fakeData) Burst(context.Context, time.Duration) (string, error) { return f.burst() }

func planWith(t *testing.T, kind config.Kind, s state, step int, d Data) plan {
	t.Helper()
	m := maxMemoryMutations()
	m.Kinds = map[config.Kind]int{kind: 1}
	r := stepRand(1, testInstance, step)
	return newPlan(r, pickKind(r, m), m, s, "rfr-x-0", d)
}

func TestMemoryPatch(t *testing.T) {
	for step := 1; step <= 50; step++ {
		p := planWith(t, config.RedisMemory, maxMemoryState("noeviction"), step, nil)
		var limit, req int64
		if _, err := fmt.Sscanf(p.params, "limits.memory 192Mi -> %dMi, requests.memory 64Mi -> %dMi", &limit, &req); err != nil {
			t.Fatalf("params %q: %v", p.params, err)
		}
		body := fmt.Sprintf(`{"spec":{"redis":{"resources":{"limits":{"memory":"%dMi"},"requests":{"memory":"%dMi"}}}}}`, limit, req)
		if string(p.patch) != body {
			t.Fatalf("patch %s, want %s", p.patch, body)
		}
		// The request keeps its share of the limit.
		if limit == 192 || limit < 96 || limit > 320 || req != 64*limit/192 {
			t.Errorf("limit %dMi, request %dMi", limit, req)
		}
		if want := maxmem.FormatBytes(maxMemoryState("").rf.MaxMemoryFor(limit * mi)); !strings.HasSuffix(p.params, "(maxmemory "+want+")") {
			t.Errorf("params %q, want maxmemory %s", p.params, want)
		}
	}

	// Guaranteed stays Guaranteed.
	s := maxMemoryState("noeviction")
	s.rf.Spec.Redis.Resources.Requests = quantities("memory", "192Mi")
	s.rf.Spec.Redis.Resources.Limits = quantities("memory", "192Mi")
	p := planWith(t, config.RedisMemory, s, 1, nil)
	var limit int64
	if _, err := fmt.Sscanf(p.params, "limits.memory 192Mi -> %dMi", &limit); err != nil || p.skip != "" ||
		!strings.Contains(string(p.patch), fmt.Sprintf(`"requests":{"memory":"%dMi"}`, limit)) {
		t.Errorf("guaranteed: %s %q %q", p.patch, p.params, p.skip)
	}

	s = maxMemoryState("noeviction")
	delete(s.rf.Spec.Redis.Resources.Limits, corev1.ResourceMemory)
	if p := planWith(t, config.RedisMemory, s, 1, nil); p.skip == "" {
		t.Errorf("not skipped without a limit: %s", p.patch)
	}
}

func TestMaxMemoryPatches(t *testing.T) {
	seen := map[string]bool{}
	for step := 1; step <= 30; step++ {
		p := planWith(t, config.MaxMemoryPolicy, maxMemoryState("noeviction"), step, nil)
		var want string
		if _, err := fmt.Sscanf(p.params, "redis.maxMemory.policy noeviction -> %s", &want); err != nil {
			t.Fatalf("params %q: %v", p.params, err)
		}
		if want == "noeviction" || string(p.patch) != `{"spec":{"redis":{"maxMemory":{"policy":"`+want+`"}}}}` {
			t.Fatalf("patch %s", p.patch)
		}
		seen[want] = true
	}
	if len(seen) != 2 {
		t.Errorf("switched to %v", seen)
	}

	p := planWith(t, config.MaxMemoryPercent, maxMemoryState("noeviction"), 1, nil)
	var want int
	if _, err := fmt.Sscanf(p.params, "redis.maxMemory.percent 75 -> %d", &want); err != nil || want == 75 || want < 10 || want > 95 {
		t.Fatalf("params %q", p.params)
	}
	if string(p.patch) != fmt.Sprintf(`{"spec":{"redis":{"maxMemory":{"percent":%d}}}}`, want) {
		t.Errorf("patch %s", p.patch)
	}

	if p := planWith(t, config.MaxMemoryPercent, testState(), 1, nil); p.skip == "" {
		t.Errorf("not skipped without maxMemory: %s", p.patch)
	}
}

func TestFillBurstPlan(t *testing.T) {
	burst := errors.New("no write was rejected")
	d := fakeData{burst: func() (string, error) { return "", burst }, writable: errors.New("OOM")}
	p := planWith(t, config.FillBurst, maxMemoryState("noeviction"), 1, d)
	if p.action == nil || p.patch != nil || p.pod != "" {
		t.Fatalf("plan %+v", p)
	}
	if err := p.action(context.Background()); !errors.Is(err, burst) {
		t.Errorf("action: %v", err)
	}
	if err := p.probe(context.Background()); err == nil {
		t.Error("probe passes while writes are rejected")
	}
	if err := p.converged(maxMemoryState("noeviction")); err != nil {
		t.Error(err)
	}
	d.burst = func() (string, error) { return "policy volatile-lru", nil }
	if err := planWith(t, config.FillBurst, maxMemoryState("noeviction"), 1, d).action(context.Background()); err == nil {
		t.Error("a burst that didn't start succeeded")
	}
	if p := planWith(t, config.FillBurst, maxMemoryState("volatile-lru"), 1, d); p.skip == "" {
		t.Error("not skipped under volatile-lru")
	}
}

// resized gives every pod and the spec a new memory limit and request, as
// a converged in-place resize does.
func resized(s *state, limit, request string) {
	s.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(limit)
	s.rf.Spec.Redis.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(request)
	r := s.rf.Spec.Redis.Resources
	l := resource.MustParse(limit)
	for i := range s.redis {
		s.redis[i].Spec.Containers[0].Resources = *r.DeepCopy()
		s.redis[i].Status.ContainerStatuses[0].Resources = r.DeepCopy()
		s.config[i].Limit = l.Value()
	}
}

func TestMemoryConverged(t *testing.T) {
	want := corev1.ResourceRequirements{
		Requests: quantities("cpu", "50m", "memory", "42Mi"),
		Limits:   quantities("memory", "128Mi"),
	}
	before := map[string]int64{"rfr-x-0": 192 * mi, "rfr-x-1": 192 * mi, "rfr-x-2": 192 * mi}
	lowered := func(s *state) {
		resized(s, "128Mi", "42Mi")
		for i := range s.config {
			s.config[i].MaxMemory = 96 * mi
		}
	}
	kept := func(s *state) {
		s.rf.Spec.Redis.Resources = *want.DeepCopy()
		s.rf.Status.Message = "maxmemory kept at 144.0Mi: lowering it to 96.0Mi would not fit the memory in use under policy noeviction"
	}

	t.Run("allkeys", func(t *testing.T) {
		policy := func(s *state) {
			s.rf.Spec.Redis.MaxMemory.Policy = "allkeys-lru"
			for i := range s.config {
				s.config[i].Policy = "allkeys-lru"
			}
		}
		runPredicateOn(t, noeviction, memoryConverged(want, 3, before, 96*mi, true), []predicateCase{
			{"lowered and resized", func(s *state) { policy(s); lowered(s) }, true},
			{"not resized", func(s *state) {
				policy(s)
				lowered(s)
				resized(s, "192Mi", "64Mi")
			}, false},
			{"maxmemory not lowered", func(s *state) {
				policy(s)
				lowered(s)
				s.config[0].MaxMemory = 144 * mi
			}, false},
			{"kept is not enough", func(s *state) { policy(s); kept(s) }, false},
		})
	})
	runPredicateOn(t, noeviction, memoryConverged(want, 3, before, 96*mi, false), []predicateCase{
		{"fits: lowered and resized", lowered, true},
		{"kept, pods not shrunk", kept, true},
		{"not picked up yet", func(s *state) { s.rf.Spec.Redis.Resources = *want.DeepCopy() }, false},
		{"kept for another target", func(s *state) {
			kept(s)
			s.rf.Status.Message = strings.Replace(s.rf.Status.Message, "96.0Mi", "120.0Mi", 1)
		}, false},
		{"kept, but a pod shrunk", func(s *state) {
			kept(s)
			s.redis[1].Status.ContainerStatuses[0].Resources = want.DeepCopy()
		}, false},
		{"kept, but maxmemory lowered on a pod", func(s *state) {
			kept(s)
			s.config[2].MaxMemory = 96 * mi
		}, false},
		{"kept, a pod not ready", func(s *state) {
			kept(s)
			s.redis[0].Status.Conditions[0].Status = corev1.ConditionFalse
		}, false},
	})
}

func TestMaxMemoryConverged(t *testing.T) {
	policyIs := func(mm *redisfailoverv1.MaxMemorySettings) bool { return mm.Policy == "volatile-lru" }
	switched := func(s *state) {
		s.rf.Spec.Redis.MaxMemory.Policy = "volatile-lru"
		for i := range s.config {
			s.config[i].Policy = "volatile-lru"
		}
	}
	runPredicateOn(t, noeviction, maxMemoryConverged(3, policyIs), []predicateCase{
		{"applied", switched, true},
		{"spec not changed", func(*state) {}, false},
		{"not applied on a pod", func(s *state) {
			switched(s)
			s.config[1].Policy = "noeviction"
		}, false},
		{"a pod unreachable", func(s *state) {
			switched(s)
			s.config[1].Err = errors.New("refused")
		}, false},
	})

	percentIs := func(mm *redisfailoverv1.MaxMemorySettings) bool { return mm.Percent == 50 }
	runPredicateOn(t, noeviction, maxMemoryConverged(3, percentIs), []predicateCase{
		{"lowered", func(s *state) {
			s.rf.Spec.Redis.MaxMemory.Percent = 50
			for i := range s.config {
				s.config[i].MaxMemory = 96 * mi
			}
		}, true},
		{"kept above the data", func(s *state) {
			s.rf.Spec.Redis.MaxMemory.Percent = 50
			s.rf.Status.Message = "maxmemory kept at 144.0Mi: lowering it to 96.0Mi would not fit the memory in use under policy noeviction"
		}, true},
		{"not lowered", func(s *state) { s.rf.Spec.Redis.MaxMemory.Percent = 50 }, false},
	})
}
