package mutator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
)

// describePlan writes what a plan does, without its random password, and
// what its convergence check says of the state s.
func describePlan(b *strings.Builder, p plan, s state) {
	fmt.Fprintf(b, "  kind=%s skip=%q params=%q patch=%s pod=%s uid=%s force=%t reset=%t\n",
		p.kind, p.skip, p.params, p.patch, p.pod, p.uid, p.force, p.reset)
	if p.skip != "" {
		return
	}
	if p.allow != nil {
		fmt.Fprintf(b, "  allow=%+v\n", *p.allow)
	}
	if p.secret != nil {
		fmt.Fprintf(b, "  secret=%s\n", p.secret.name)
	}
	if p.restore != nil {
		fmt.Fprintf(b, "  restore=%s %q patch=%s\n", p.restore.name, p.restore.password, p.restore.patch)
	}
	if p.offline != nil {
		fmt.Fprintf(b, "  offline=%s previous=%q forget=%t\n", p.offline.secret, p.offline.previous, p.offline.forget)
	}
	fmt.Fprintf(b, "  fetch password=%t sentinelMaster=%t sentinelObjects=%t servers=%t\n",
		p.fetch.password != nil, p.fetch.sentinelMaster, p.fetch.sentinelObjects, p.fetch.servers)
	fmt.Fprintf(b, "  action=%t probe=%t recovery=%t converged=%t\n", p.action != nil, p.probe != nil, p.recovery != nil, p.converged != nil)
	if p.converged != nil {
		fmt.Fprintf(b, "  converged(s)=%v\n", p.converged(s))
	}
}

// goldenFile compares got with a file of testdata, which UPDATE_GOLDEN=1
// writes.
func goldenFile(t *testing.T, name, got string) {
	t.Helper()
	path := "testdata/" + name
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("plans differ from %s:\n%s", path, got)
	}
}

// newPlan plans each kind the same way for the same seed and state.
func TestNewPlanGolden(t *testing.T) {
	m := maxMemoryMutations()
	full := mutations()
	m.RedisReplicas, m.SentinelReplicas, m.Resources, m.ForceDeleteProbability =
		full.RedisReplicas, full.SentinelReplicas, full.Resources, 0.5
	noPods := testState()
	noPods.redis, noPods.sentinels = noPods.redis[:1], nil
	noAuth := testState()
	noAuth.password = ""
	unknown := authState()
	unknown.password = ""
	states := []struct {
		name string
		s    func() state
	}{
		{"plain", testState},
		{"noeviction", noeviction},
		{"allkeys-lru", func() state { return maxMemoryState("allkeys-lru") }},
		{"changed", func() state {
			s := maxMemoryState("volatile-lru")
			s.rf.Spec.Redis.MaxMemory.Percent = 40
			s.config = []maxmem.Pod{{Name: "rfr-x-0", Limit: 192 * mi, MaxMemory: 1, Policy: "volatile-lru"}}
			return s
		}},
		{"auth", authState},
		{"no pods", func() state { return noPods }},
		{"no auth", func() state { return noAuth }},
		{"unknown password", func() state { return unknown }},
	}
	kinds := []config.Kind{
		config.RedisReplicas, config.SentinelReplicas, config.RedisResources, config.KillMaster, config.KillMasterForce,
		config.KillReplica, config.KillSentinel, config.PasswordRotate, config.AuthAdd, config.AuthRemove,
		config.PasswordRotateOffline, config.PasswordRotateOfflineForget, config.SentinelToggle,
		config.RedisMemory, config.MaxMemoryPolicy, config.MaxMemoryPercent, config.FillBurst, config.Reset,
	}
	var b strings.Builder
	for _, st := range states {
		for _, kind := range kinds {
			for step := 1; step <= 3; step++ {
				s := st.s()
				p := newPlan(stepRand(7, testInstance, step), kind, m, s, "rfr-x-0", fakeData{})
				fmt.Fprintf(&b, "%s %s step %d\n", st.name, kind, step)
				describePlan(&b, p, s)
			}
		}
	}
	goldenFile(t, "newplan.golden", b.String())
}

// planAutoscale plans each autoscale kind the same way for the same seed
// and state.
func TestPlanAutoscaleGolden(t *testing.T) {
	kinds := []config.Kind{
		config.AutoscaleLoad, config.AutoscaleLimit, config.AutoscaleMemoryMax, config.AutoscaleMode,
		config.AutoscaleStateLost, config.AutoscaleKillMaster, config.AutoscaleKillReplica, config.KillMaster,
	}
	var b strings.Builder
	for _, mode := range []string{"Auto", "Recommend", "Off"} {
		for _, kind := range kinds {
			for step := 1; step <= 3; step++ {
				s := autoscaled(mode)
				mu, _, _ := autoscaleMutator(s, &fakeLoader{})
				mu.in.Autoscale.MemoryMax = autoscaleMemoryMax
				p := mu.planAutoscale(stepRand(7, mu.in, step), kind, s)
				fmt.Fprintf(&b, "%s %s step %d\n", mode, kind, step)
				describePlan(&b, p, s)
			}
		}
	}
	goldenFile(t, "planautoscale.golden", b.String())
}

// autoscaleMemoryMax gives memoryMax values below and above the limit.
var autoscaleMemoryMax = config.Range{Min: 128, Max: 512}

// The ConfigMap of the state is deleted only after a read of it.
func TestPlanAutoscaleStateLostReadFails(t *testing.T) {
	s := autoscaled("Auto")
	m, kube, _ := autoscaleMutator(s, nil)
	denied := errors.New("denied")
	kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, denied })
	p := m.plan(stepRand(1, m.in, 1), config.AutoscaleStateLost, s)
	var na notAppliedError
	if err := p.action(context.Background()); !errors.Is(err, denied) || errors.As(err, &na) {
		t.Errorf("error %v", err)
	}
	for _, a := range kube.Actions() {
		if a.GetVerb() == "delete" {
			t.Errorf("deleted after a failed read: %v", a)
		}
	}
}
