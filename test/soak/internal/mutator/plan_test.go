package mutator

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/config"
)

var testInstance = config.Instance{Name: "x", Namespace: "ns", Mode: config.ModeSentinel}

func mutations() config.Mutations {
	return config.Mutations{
		Kinds: map[config.Kind]int{
			config.RedisReplicas: 3, config.SentinelReplicas: 1, config.RedisResources: 2,
			config.KillMaster: 1, config.KillReplica: 1, config.KillSentinel: 1,
		},
		RedisReplicas:    config.Range{Min: 1, Max: 5},
		SentinelReplicas: config.Range{Min: 3, Max: 5},
		Resources: config.Resources{
			Requests: config.ResourceRanges{CPU: config.Range{Min: 25, Max: 100}, Memory: config.Range{Min: 32, Max: 96}},
			Limits:   config.ResourceRanges{Memory: config.Range{Min: 128, Max: 256}},
		},
		ForceDeleteProbability: 0.5,
	}
}

func kinds(seed int64, in config.Instance, n int) []config.Kind {
	var ks []config.Kind
	for step := 1; step <= n; step++ {
		ks = append(ks, pickKind(stepRand(seed, in, step), mutations()))
	}
	return ks
}

func TestPickIsDeterministic(t *testing.T) {
	a := kinds(42, testInstance, 200)
	if b := kinds(42, testInstance, 200); !slices.Equal(a, b) {
		t.Error("the same seed picked different kinds")
	}
	if b := kinds(43, testInstance, 200); slices.Equal(a, b) {
		t.Error("another seed picked the same kinds")
	}
	other := testInstance
	other.Name = "y"
	if b := kinds(42, other, 200); slices.Equal(a, b) {
		t.Error("another instance picked the same kinds")
	}
	counts := map[config.Kind]int{}
	for _, k := range kinds(42, testInstance, 9000) {
		counts[k]++
	}
	// Weights 3:1:2:1:1:1 of 9000.
	for k, w := range mutations().Kinds {
		if c := counts[k]; c < w*800 || c > w*1200 {
			t.Errorf("%s picked %d times, want about %d", k, c, w*1000)
		}
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	plans := func() []string {
		var out []string
		for step := 1; step <= 50; step++ {
			r := stepRand(7, testInstance, step)
			p := newPlan(r, pickKind(r, mutations()), mutations(), testState(), "rfr-x-0", nil)
			out = append(out, string(p.kind)+" "+p.params+" "+p.skip)
		}
		return out
	}
	if a, b := plans(), plans(); !slices.Equal(a, b) {
		t.Errorf("plans differ:\n%v\n%v", a, b)
	}
}

func TestPickOther(t *testing.T) {
	rg := config.Range{Min: 1, Max: 5}
	for step := range 200 {
		r := stepRand(1, testInstance, step)
		for current := int64(0); current <= 7; current++ {
			v := pickOther(r, rg, current)
			if v == current || v < rg.Min || v > rg.Max {
				t.Fatalf("pickOther(%d) = %d", current, v)
			}
		}
	}
}

func quantities(kv ...string) corev1.ResourceList {
	l := corev1.ResourceList{}
	for i := 0; i < len(kv); i += 2 {
		l[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return l
}

func readyPod(name, uid, role string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), Labels: map[string]string{}},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	if role != "" {
		p.Labels[roleLabel] = role
	}
	return p
}

// testState is a converged sentinel instance with 3 redis pods (master
// rfr-x-0) and 3 Sentinels.
func testState() state {
	resources := corev1.ResourceRequirements{
		Requests: quantities("cpu", "50m", "memory", "64Mi"),
		Limits:   quantities("memory", "256Mi"),
	}
	rf := &redisfailoverv1.RedisFailover{Spec: redisfailoverv1.RedisFailoverSpec{
		Redis:    redisfailoverv1.RedisSettings{Replicas: 3, Resources: resources},
		Sentinel: redisfailoverv1.SentinelSettings{Replicas: 3},
	}}
	three := int32(3)
	s := state{
		rf: rf,
		sts: &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Spec:       appsv1.StatefulSetSpec{Replicas: &three},
			Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 3,
				CurrentRevision: "a", UpdateRevision: "b"},
		},
		sentinel: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Spec:       appsv1.DeploymentSpec{Replicas: &three},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 3, ReadyReplicas: 3,
				UpdatedReplicas: 3, AvailableReplicas: 3},
		},
	}
	for i, role := range []string{roleMaster, roleReplica, roleReplica} {
		p := readyPod("rfr-x-"+string(rune('0'+i)), "u"+string(rune('0'+i)), role)
		p.Spec.Containers = []corev1.Container{{Name: redisName, Resources: resources}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: redisName, Resources: resources.DeepCopy()}}
		s.redis = append(s.redis, p)
	}
	for _, n := range []string{"a", "b", "c"} {
		s.sentinels = append(s.sentinels, readyPod("rfs-x-"+n, "s"+n, ""))
	}
	return s
}

// planFor returns the first plan of kind with the seed, from step 1 on.
func planFor(t *testing.T, kind config.Kind, m config.Mutations, s state, master string) plan {
	t.Helper()
	m.Kinds = map[config.Kind]int{kind: 1}
	r := stepRand(1, testInstance, 1)
	p := newPlan(r, pickKind(r, m), m, s, master, nil)
	if p.kind != kind {
		t.Fatalf("picked %s", p.kind)
	}
	return p
}

func TestReplicaPatches(t *testing.T) {
	for kind, field := range map[config.Kind]string{config.RedisReplicas: "redis", config.SentinelReplicas: "sentinel"} {
		p := planFor(t, kind, mutations(), testState(), "rfr-x-0")
		var want int
		if _, err := fmt.Sscanf(p.params, field+".replicas 3 -> %d", &want); err != nil {
			t.Fatalf("%s params %q: %v", kind, p.params, err)
		}
		body := `{"spec":{"` + field + `":{"replicas":` + strconv.Itoa(want) + `}}}`
		if string(p.patch) != body {
			t.Errorf("%s patch %s, want %s", kind, p.patch, body)
		}
		if want == 3 {
			t.Errorf("%s keeps 3 replicas", kind)
		}
	}
}

func TestResourcesPatch(t *testing.T) {
	seen := map[string]bool{}
	for step := 1; step <= 100; step++ {
		r := stepRand(1, testInstance, step)
		p := planResources(r, mutations().Resources, testState())
		if p.skip != "" {
			t.Fatalf("skipped: %s", p.skip)
		}
		body := string(p.patch)
		switch {
		case strings.Contains(body, `"cpu"`) && strings.Contains(body, `"memory"`):
			seen["both"] = true
		case strings.Contains(body, `"cpu"`):
			seen["cpu"] = true
		default:
			seen["memory"] = true
		}
		// Only configured values the RedisFailover sets, and nothing else.
		if strings.Contains(body, `"limits":{"cpu"`) || !strings.HasPrefix(body, `{"spec":{"redis":{"resources":{`) {
			t.Fatalf("patch %s", body)
		}
	}
	if len(seen) != 3 {
		t.Errorf("changed %v, want cpu, memory and both", seen)
	}

	r := stepRand(1, testInstance, 5)
	p := planResources(r, mutations().Resources, testState())
	want := `{"spec":{"redis":{"resources":{"limits":{"memory":"199Mi"},"requests":{"cpu":"33m","memory":"84Mi"}}}}}`
	if string(p.patch) != want || p.params != "requests.cpu 50m -> 33m, requests.memory 64Mi -> 84Mi, limits.memory 256Mi -> 199Mi" {
		t.Errorf("patch %s, params %q", p.patch, p.params)
	}
}

func TestResourcesKeepQoS(t *testing.T) {
	// Guaranteed: requests equal limits.
	s := testState()
	s.rf.Spec.Redis.Resources = corev1.ResourceRequirements{
		Requests: quantities("cpu", "100m", "memory", "128Mi"),
		Limits:   quantities("cpu", "100m", "memory", "128Mi"),
	}
	m := mutations()
	m.Resources = config.Resources{
		Requests: config.ResourceRanges{CPU: config.Range{Min: 25, Max: 50}},
		Limits:   config.ResourceRanges{CPU: config.Range{Min: 100, Max: 200}},
	}
	for step := 1; step <= 20; step++ {
		p := planResources(stepRand(1, testInstance, step), m.Resources, s)
		if !strings.Contains(p.skip, "QoS class from Guaranteed to Burstable") {
			t.Fatalf("not skipped: %q %s", p.skip, p.patch)
		}
	}

	// A request above a limit the config doesn't bound.
	s = testState()
	m.Resources = config.Resources{Requests: config.ResourceRanges{Memory: config.Range{Min: 300, Max: 400}}}
	if p := planResources(stepRand(1, testInstance, 1), m.Resources, s); !strings.Contains(p.skip, "would exceed limits.memory") {
		t.Errorf("not skipped: %q %s", p.skip, p.patch)
	}

	// Nothing configured is set.
	m.Resources = config.Resources{Limits: config.ResourceRanges{CPU: config.Range{Min: 100, Max: 200}}}
	if p := planResources(stepRand(1, testInstance, 1), m.Resources, s); p.skip == "" {
		t.Errorf("not skipped: %s", p.patch)
	}
}

func TestQoSClass(t *testing.T) {
	cases := []struct {
		r    corev1.ResourceRequirements
		want corev1.PodQOSClass
	}{
		{corev1.ResourceRequirements{}, corev1.PodQOSBestEffort},
		{corev1.ResourceRequirements{Requests: quantities("cpu", "1")}, corev1.PodQOSBurstable},
		{corev1.ResourceRequirements{Limits: quantities("cpu", "1", "memory", "1Gi")}, corev1.PodQOSGuaranteed},
		{corev1.ResourceRequirements{Requests: quantities("cpu", "1", "memory", "1Gi"), Limits: quantities("cpu", "1", "memory", "1Gi")}, corev1.PodQOSGuaranteed},
		{corev1.ResourceRequirements{Requests: quantities("cpu", "500m"), Limits: quantities("cpu", "1", "memory", "1Gi")}, corev1.PodQOSBurstable},
	}
	for i, c := range cases {
		if got := qosClass(c.r); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}

func TestKillPlans(t *testing.T) {
	m := mutations()
	m.ForceDeleteProbability = 1
	p := planFor(t, config.KillMasterForce, m, testState(), "rfr-x-0")
	if p.pod != "rfr-x-0" || p.uid != "u0" || !p.force || p.patch != nil || p.params != "delete pod rfr-x-0 (force)" || p.reset {
		t.Errorf("kill_master_force: %+v", p)
	}
	// The probability is for replicas and Sentinels only.
	p = planFor(t, config.KillMaster, m, testState(), "rfr-x-0")
	if p.pod != "rfr-x-0" || p.force || p.params != "delete pod rfr-x-0 (graceful)" {
		t.Errorf("kill_master: %+v", p)
	}
	// The only pod without a volume loses the data by design.
	s := testState()
	s.rf.Spec.Redis.Replicas, s.redis = 1, s.redis[:1]
	if p := planFor(t, config.KillMaster, m, s, "rfr-x-0"); !p.reset {
		t.Errorf("only pod: %+v", p)
	}
	s.rf.Spec.Redis.Storage.PersistentVolumeClaim = &redisfailoverv1.EmbeddedPersistentVolumeClaim{}
	if p := planFor(t, config.KillMaster, m, s, "rfr-x-0"); p.reset {
		t.Errorf("only pod with a volume: %+v", p)
	}
	m.ForceDeleteProbability = 0
	p = planFor(t, config.KillReplica, m, testState(), "rfr-x-0")
	if (p.pod != "rfr-x-1" && p.pod != "rfr-x-2") || p.force {
		t.Errorf("kill_replica: %+v", p)
	}
	p = planFor(t, config.KillSentinel, m, testState(), "rfr-x-0")
	if !strings.HasPrefix(p.pod, "rfs-x-") || !strings.HasSuffix(p.params, "(graceful)") {
		t.Errorf("kill_sentinel: %+v", p)
	}

	// The label and the observer disagree on the master.
	if p := planFor(t, config.KillMaster, m, testState(), "rfr-x-1"); p.skip == "" {
		t.Errorf("not skipped: %+v", p)
	}
	s = testState()
	s.redis[1].Labels[roleLabel] = roleMaster
	if p := planFor(t, config.KillMaster, m, s, "rfr-x-0"); p.skip == "" {
		t.Errorf("not skipped with two labelled masters: %+v", p)
	}
	s = testState()
	s.redis = s.redis[:1]
	if p := planFor(t, config.KillReplica, m, s, "rfr-x-0"); p.skip == "" {
		t.Errorf("not skipped without replicas: %+v", p)
	}
}

// The tester, not the alert or the e2e script, decides which mutations must
// lose no acknowledged write.
func TestLossless(t *testing.T) {
	ok := &transition{edge: config.Edge{Expect: config.ExpectOK}}
	unknown := &transition{edge: config.Edge{Expect: config.ExpectUnknown}}
	cases := []struct {
		p                 plan
		emptyDir, volumes bool
	}{
		{plan{kind: config.PasswordRotate}, true, true},
		{plan{kind: config.SentinelToggle}, true, true},
		{plan{kind: config.KillMaster}, false, true},
		{plan{kind: config.KillMasterForce}, false, false},
		// A replica that lags can lack writes at the SIGTERM wait, also on a
		// volume.
		{plan{kind: config.SentinelResetKillMaster}, false, false},
		{plan{kind: config.RedisReplicas}, false, true},
		{plan{kind: config.KillReplica}, false, false},
		{plan{kind: config.ImageUpgrade, edge: ok}, false, true},
		{plan{kind: config.ImageUpgrade, edge: unknown}, false, false},
	}
	for _, c := range cases {
		s := testState()
		if got := lossless(c.p, s); got != c.emptyDir {
			t.Errorf("%s on emptyDir: %v", c.p.kind, got)
		}
		s.rf.Spec.Redis.Storage.PersistentVolumeClaim = &redisfailoverv1.EmbeddedPersistentVolumeClaim{}
		if got := lossless(c.p, s); got != c.volumes {
			t.Errorf("%s on volumes: %v", c.p.kind, got)
		}
	}
}
