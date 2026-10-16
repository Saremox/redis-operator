package mutator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/autoscale"
	"github.com/saremox/redis-operator/test/soak/internal/config"
)

// autoscaled is noeviction in mode, with the limit 192Mi as the request,
// memoryMax 384Mi, and the StatefulSet template on the limit.
func autoscaled(mode string) state {
	s := noeviction()
	res := corev1.ResourceRequirements{Requests: quantities("memory", "192Mi"), Limits: quantities("memory", "192Mi")}
	s.rf.Name, s.rf.Namespace = "x", "ns"
	s.rf.Spec.Redis.Resources = res
	s.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: mode, MemoryMax: new(resource.MustParse("384Mi"))}
	s.sts.Spec.Template.Spec.Containers = []corev1.Container{{Name: redisName, Resources: res}}
	return s
}

// autoscaleMutator is a mutator of instance x in namespace ns with the
// state s in its clients, and the state ConfigMap.
func autoscaleMutator(s state, d Data) (*Mutator, *fake.Clientset, versioned.Interface) {
	in := config.Instance{Name: "x", Namespace: "ns", Autoscale: &config.Autoscale{
		Limit: config.Range{Min: 128, Max: 512}, MemoryMax: config.Range{Min: 64, Max: 128},
	}}
	kube := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rfa-x", Namespace: "ns", UID: "rfa-1"}})
	for i := range s.redis {
		p := s.redis[i].DeepCopy()
		p.Namespace = "ns"
		_, _ = kube.CoreV1().Pods("ns").Create(context.Background(), p, metav1.CreateOptions{})
	}
	rfs := rffake.NewSimpleClientset(s.rf.DeepCopy())
	m := &Mutator{in: in, kube: kube, rfs: rfs, observer: &stubObserver{master: "rfr-x-0"}, data: d, log: slog.New(slog.DiscardHandler)}
	return m, kube, rfs
}

func TestPlanAutoscaleMode(t *testing.T) {
	for _, c := range []struct {
		from, to string
		allow    *autoscale.Allow
	}{
		{"Auto", "Recommend", allowNone},
		{"Recommend", "Off", allowResize},
		{"Off", "Auto", allowNone},
	} {
		s := autoscaled(c.from)
		m, _, _ := autoscaleMutator(s, nil)
		p := m.plan(stepRand(1, m.in, 1), config.AutoscaleMode, s)
		if p.skip != "" || p.allow != c.allow || !strings.Contains(string(p.patch), `"mode":"`+c.to+`"`) {
			t.Errorf("%s: %+v, patch %s", c.from, p, p.patch)
		}
	}
}

func TestPlanAutoscaleSkips(t *testing.T) {
	noAutoscaling := autoscaled("Auto")
	noAutoscaling.rf.Spec.Redis.Autoscaling = nil
	noReplica := autoscaled("Auto")
	noReplica.redis = noReplica.redis[:1]
	for _, c := range []struct {
		name string
		kind config.Kind
		s    state
		d    Data
	}{
		{"no autoscaling", config.AutoscaleMode, noAutoscaling, nil},
		{"mode Recommend", config.AutoscaleLimit, autoscaled("Recommend"), nil},
		{"memoryMax below the limit", config.AutoscaleMemoryMax, autoscaled("Auto"), nil},
		{"load without data", config.AutoscaleLoad, autoscaled("Auto"), nil},
		{"no replica", config.AutoscaleKillReplica, noReplica, &fakeLoader{}},
	} {
		m, _, _ := autoscaleMutator(c.s, c.d)
		if p := m.plan(stepRand(1, m.in, 1), c.kind, c.s); p.skip == "" {
			t.Errorf("%s: not skipped: %+v", c.name, p)
		}
	}
	// The observer saw another master.
	s := autoscaled("Auto")
	m, _, _ := autoscaleMutator(s, &fakeLoader{})
	m.observer = &stubObserver{master: "rfr-x-1"}
	if p := m.plan(stepRand(1, m.in, 1), config.AutoscaleKillMaster, s); p.skip == "" {
		t.Errorf("a stale master label: %+v", p)
	}
	if p := m.planAutoscale(stepRand(1, m.in, 1), config.KillMaster, s); p.skip != "unknown kind" {
		t.Errorf("another kind: %+v", p)
	}
}

// The limit stays within its range and at most memoryMax, and the request
// follows it.
func TestPlanAutoscaleLimit(t *testing.T) {
	s := autoscaled("Auto")
	m, _, _ := autoscaleMutator(s, nil)
	planned, skipped := 0, 0
	for step := 1; step <= 50; step++ {
		p := m.plan(stepRand(1, m.in, step), config.AutoscaleLimit, s)
		if p.skip != "" {
			skipped++
			continue
		}
		planned++
		if !strings.Contains(string(p.patch), `"requests":{"memory":`) {
			t.Fatalf("%+v %s", p, p.patch)
		}
		var want string
		for part := range strings.SplitSeq(p.params, " ") {
			want = part
		}
		q := resource.MustParse(want)
		if q.Value() > 384*mi || q.Value() < 128*mi {
			t.Errorf("limit %s", want)
		}
		// Only a lower limit may shrink a pod. A raise changes the template,
		// so it may recreate a pod.
		if wantAllow := map[bool]*autoscale.Allow{true: allowResize, false: allowRaise}[q.Value() < 192*mi]; p.allow != wantAllow {
			t.Errorf("limit %s: allow %+v", want, p.allow)
		}
		if err := p.converged(s); err == nil {
			t.Error("converged before the change")
		}
		changed := autoscaled("Auto")
		resized(&changed, want, want)
		changed.sts.Spec.Template.Spec.Containers[0].Resources = changed.rf.Spec.Redis.Resources
		for i := range changed.config {
			changed.config[i].MaxMemory = changed.rf.MaxMemoryFor(changed.config[i].Limit)
		}
		if err := p.converged(changed); err != nil {
			t.Errorf("after the change: %v", err)
		}
	}
	if planned == 0 || skipped == 0 {
		t.Errorf("%d planned, %d skipped above memoryMax", planned, skipped)
	}
}

func TestPlanAutoscaleMemoryMax(t *testing.T) {
	s := autoscaled("Auto")
	m, _, _ := autoscaleMutator(s, nil)
	m.in.Autoscale.MemoryMax = config.Range{Min: 192, Max: 512}
	p := m.plan(stepRand(1, m.in, 1), config.AutoscaleMemoryMax, s)
	if p.skip != "" || p.allow != allowNone || !strings.Contains(string(p.patch), `"memoryMax":"`) {
		t.Fatalf("%+v %s", p, p.patch)
	}
	if err := p.converged(s); err == nil {
		t.Error("converged before the change")
	}
	changed := autoscaled("Auto")
	q := resource.MustParse(strings.Fields(p.params)[3])
	changed.rf.Spec.Redis.Autoscaling.MemoryMax = &q
	if err := p.converged(changed); err != nil {
		t.Errorf("after the change: %v", err)
	}
}

// A memoryMax below the largest pod limit may recreate and shrink a pod. A
// memoryMax at or above it changes no pod.
func TestPlanAutoscaleMemoryMaxAllow(t *testing.T) {
	for _, c := range []struct {
		name      string
		pod       string
		memoryMax config.Range
		allow     *autoscale.Allow
	}{
		{"above the pods", "192Mi", config.Range{Min: 256, Max: 257}, allowNone},
		{"at the pods", "192Mi", config.Range{Min: 192, Max: 193}, allowNone},
		{"below a pod", "320Mi", config.Range{Min: 256, Max: 257}, allowResize},
	} {
		s := autoscaled("Auto")
		s.redis[2].Status.ContainerStatuses[0].Resources.Limits = quantities("memory", c.pod)
		m, _, _ := autoscaleMutator(s, nil)
		m.in.Autoscale.MemoryMax = c.memoryMax
		for step := 1; step <= 10; step++ {
			p := m.plan(stepRand(1, m.in, step), config.AutoscaleMemoryMax, s)
			if p.skip != "" || p.allow != c.allow {
				t.Errorf("%s, step %d: %+v", c.name, step, p)
			}
		}
	}
}

func TestPlanAutoscaleStateLost(t *testing.T) {
	s := autoscaled("Auto")
	m, kube, _ := autoscaleMutator(s, nil)
	p := m.plan(stepRand(1, m.in, 1), config.AutoscaleStateLost, s)
	if p.allow != allowNone {
		t.Errorf("allow %+v", p.allow)
	}
	if err := p.action(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().ConfigMaps("ns").Get(context.Background(), "rfa-x", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the ConfigMap is still there: %v", err)
	}
	// The mutation converges only after the operator wrote the state again,
	// because the unchanged instance converges before the operator sees the
	// loss.
	probe := func() error {
		if p.probe == nil {
			return errors.New("no probe")
		}
		return p.probe(context.Background())
	}
	if err := p.converged(s); err != nil {
		t.Fatalf("the instance: %v", err)
	}
	if probe() == nil {
		t.Error("converged without a state ConfigMap")
	}
	// The deleted ConfigMap is no write of the operator.
	deleted := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rfa-x", Namespace: "ns", UID: "rfa-1"}}
	if _, err := kube.CoreV1().ConfigMaps("ns").Create(context.Background(), deleted, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if probe() == nil {
		t.Error("converged with the deleted ConfigMap")
	}
	if err := kube.CoreV1().ConfigMaps("ns").Delete(context.Background(), "rfa-x", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	written := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rfa-x", Namespace: "ns", UID: "rfa-2"}}
	if _, err := kube.CoreV1().ConfigMaps("ns").Create(context.Background(), written, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := probe(); err != nil {
		t.Errorf("after the write of the operator: %v", err)
	}
	if err := kube.CoreV1().ConfigMaps("ns").Delete(context.Background(), "rfa-x", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	var na notAppliedError
	if err := p.action(context.Background()); !errors.As(err, &na) {
		t.Errorf("a second delete: %v", err)
	}
}

// fakeLoader gives the RedisFailover the next status at each call of until,
// and ends the hold after the last one.
type fakeLoader struct {
	fakeData
	skip     string
	err      error
	rfs      versioned.Interface
	statuses []redisfailoverv1.AutoscalingStatus
	calls    int
}

func (f *fakeLoader) Load(ctx context.Context, percent int64, _ time.Duration, until func(context.Context) (bool, error)) (string, error) {
	if f.skip != "" || f.err != nil || percent != loadPercent {
		return f.skip, f.err
	}
	for _, st := range f.statuses {
		f.calls++
		rf, err := f.rfs.DatabasesV1().RedisFailovers("ns").Get(ctx, "x", metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		rf.Status.Autoscaling = st
		if _, err := f.rfs.DatabasesV1().RedisFailovers("ns").UpdateStatus(ctx, rf, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
		if ok, err := until(ctx); ok || err != nil {
			return "", err
		}
	}
	return "", nil
}

func TestPlanLoad(t *testing.T) {
	const earlier, later = "2026-10-01T12:00:00Z", "2026-10-01T13:00:00Z"
	idle := redisfailoverv1.AutoscalingStatus{LastStep: earlier, Phase: autoscale.PhaseDone}
	applying := redisfailoverv1.AutoscalingStatus{LastStep: later, Phase: autoscale.PhaseApplying}
	done := redisfailoverv1.AutoscalingStatus{LastStep: later, Phase: autoscale.PhaseDone}
	for _, c := range []struct {
		name     string
		kind     config.Kind
		skip     string
		statuses []redisfailoverv1.AutoscalingStatus
		calls    int
		// notAppliedError is a skipped result, and deleted is the pod that the
		// mutation deleted.
		notAppliedError bool
		deleted         string
		// unreadable is a RedisFailover that the mutator cannot read.
		unreadable bool
	}{
		{"a step", config.AutoscaleLoad, "", []redisfailoverv1.AutoscalingStatus{idle, applying, done}, 2, false, "", false},
		{"no step", config.AutoscaleLoad, "", []redisfailoverv1.AutoscalingStatus{idle, idle}, 2, false, "", false},
		{"not loaded", config.AutoscaleLoad, "policy allkeys-lru", nil, 0, true, "", false},
		{"a replica in a step", config.AutoscaleKillReplica, "", []redisfailoverv1.AutoscalingStatus{idle, applying}, 2, false, "rfr-x-", false},
		{"the master in a step", config.AutoscaleKillMaster, "", []redisfailoverv1.AutoscalingStatus{applying}, 1, false, "rfr-x-0", false},
		{"a step that ended before", config.AutoscaleKillMaster, "", []redisfailoverv1.AutoscalingStatus{done, applying}, 1, true, "", false},
		{"no step to disturb", config.AutoscaleKillMaster, "", []redisfailoverv1.AutoscalingStatus{idle}, 1, true, "", false},
		{"no read", config.AutoscaleLoad, "", []redisfailoverv1.AutoscalingStatus{applying}, 1, false, "", true},
	} {
		s := autoscaled("Auto")
		s.rf.Status.Autoscaling = idle
		l := &fakeLoader{skip: c.skip, statuses: c.statuses}
		m, kube, rfs := autoscaleMutator(s, l)
		l.rfs = rfs
		if c.unreadable {
			m.rfs = rffake.NewSimpleClientset()
		}
		p := m.plan(stepRand(1, m.in, 1), c.kind, s)
		err := p.action(context.Background())
		var na notAppliedError
		if errors.As(err, &na) != c.notAppliedError || err != nil && !c.notAppliedError || l.calls != c.calls {
			t.Errorf("%s: %v, %d calls", c.name, err, l.calls)
		}
		pods, _ := kube.CoreV1().Pods("ns").List(context.Background(), metav1.ListOptions{})
		gone := ""
		for _, name := range []string{"rfr-x-0", "rfr-x-1", "rfr-x-2"} {
			found := false
			for _, p := range pods.Items {
				found = found || p.Name == name
			}
			if !found {
				gone = name
			}
		}
		if c.deleted == "" && gone != "" || c.deleted != "" && !strings.HasPrefix(gone, c.deleted) {
			t.Errorf("%s: deleted %q", c.name, gone)
		}
		if c.kind == config.AutoscaleLoad && p.allow != allowNone {
			t.Errorf("%s: allow %+v", c.name, p.allow)
		}
		// A kill may recreate only the killed pod, and kill_master may move
		// the master role.
		if want := (autoscale.Allow{Recreate: true, Handover: c.kind == config.AutoscaleKillMaster, Pod: gone}); c.deleted != "" && *p.allow != want {
			t.Errorf("%s: allow %+v, want %+v", c.name, *p.allow, want)
		}
	}
}

// A kill kind converges after the pod is back and the step ended.
func TestPlanLoadKillConverged(t *testing.T) {
	s := autoscaled("Auto")
	m, _, _ := autoscaleMutator(s, &fakeLoader{})
	p := m.plan(stepRand(1, m.in, 1), config.AutoscaleKillMaster, s)
	if err := p.converged(s); err == nil {
		t.Error("converged before the delete")
	}
	replaced := autoscaled("Auto")
	replaced.redis[0].UID = "u0-new"
	if err := p.converged(replaced); err != nil {
		t.Error(err)
	}
	replaced.rf.Status.Autoscaling.Phase = autoscale.PhaseApplying
	if err := p.converged(replaced); err == nil {
		t.Error("converged while the step is Applying")
	}
}

// A failed load fails the mutation.
func TestPlanLoadFailed(t *testing.T) {
	s := autoscaled("Auto")
	failed := errors.New("failed")
	m, _, _ := autoscaleMutator(s, &fakeLoader{err: failed})
	if err := m.plan(stepRand(1, m.in, 1), config.AutoscaleLoad, s).action(context.Background()); !errors.Is(err, failed) {
		t.Errorf("%v", err)
	}
}

func TestAutoscaleConverged(t *testing.T) {
	runPredicateOn(t, func() state { return autoscaled("Auto") }, autoscaleConverged(3), []predicateCase{
		{"converged", func(*state) {}, true},
		{"a step is Applying", func(s *state) { s.rf.Status.Autoscaling.Phase = autoscale.PhaseApplying }, false},
		{"a step is Held", func(s *state) { s.rf.Status.Autoscaling.Phase = autoscale.PhaseHeld }, true},
		{"a resize is pending", func(s *state) {
			s.redis[1].Annotations = map[string]string{resizeRequest: "now"}
		}, false},
		{"a pod below the template", func(s *state) {
			s.sts.Spec.Template.Spec.Containers[0].Resources.Limits = quantities("memory", "256Mi")
		}, false},
		// Kubernetes 1.33 and 1.34 cannot lower a limit in place.
		{"a pod above the template", func(s *state) {
			s.sts.Spec.Template.Spec.Containers[0].Resources.Limits = quantities("memory", "128Mi")
		}, true},
		{"rollout not complete", func(s *state) { s.sts.Status.UpdatedReplicas = 2 }, false},
		{"a pod not Ready", func(s *state) { s.redis[2].Status.Conditions[0].Status = corev1.ConditionFalse }, false},
		{"a template without redis", func(s *state) { s.sts.Spec.Template.Spec.Containers = nil }, true},
		// The kubelet did not apply a lower limit yet.
		{"a resize in progress", func(s *state) {
			s.redis[1].Spec.Containers[0].Resources.Limits = quantities("memory", "128Mi")
		}, false},
		{"maxmemory not applied", func(s *state) { s.config[2].MaxMemory = 100 * mi }, false},
		{"not Healthy", func(s *state) { s.rf.Status.State = "NotHealthy" }, false},
	})
}

func TestModeConverged(t *testing.T) {
	off := func() state { return autoscaled("Off") }
	runPredicateOn(t, off, modeConverged("Off", 3), []predicateCase{
		{"converged", func(*state) {}, true},
		{"mode not changed", func(s *state) { s.rf.Spec.Redis.Autoscaling.Mode = "Auto" }, false},
		{"a status", func(s *state) { s.rf.Status.Autoscaling.Phase = autoscale.PhaseDone }, false},
		{"a pod above the spec limit", func(s *state) {
			s.rf.Spec.Redis.Resources.Limits = quantities("memory", "128Mi")
			s.sts.Spec.Template.Spec.Containers[0].Resources.Limits = quantities("memory", "128Mi")
			for i := range s.config {
				s.config[i].MaxMemory = 96 * mi
			}
		}, false},
	})
	// Mode Recommend keeps the status and a larger pod.
	runPredicateOn(t, func() state { return autoscaled("Recommend") }, modeConverged("Recommend", 3), []predicateCase{
		{"with a status", func(s *state) { s.rf.Status.Autoscaling.Phase = autoscale.PhaseDone }, true},
		{"mode not changed", func(s *state) { s.rf.Spec.Redis.Autoscaling.Mode = "Auto" }, false},
	})
}

func TestAllowed(t *testing.T) {
	m := &Mutator{}
	if _, ok := m.Allowed(); ok {
		t.Error("no mutation runs")
	}
	kind := config.KillMaster
	m.current.Store(&kind)
	if a, ok := m.Allowed(); !ok || a != autoscale.AllowAll {
		t.Errorf("a kind without its own rule: %+v", a)
	}
	killed := &autoscale.Allow{Recreate: true, Pod: "rfr-x-1"}
	m.allowed.Store(killed)
	if a, ok := m.Allowed(); !ok || a != *killed {
		t.Errorf("autoscale_kill_replica: %+v", a)
	}
}

// A mutation that found nothing to do counts as skipped.
func TestExecuteNotApplied(t *testing.T) {
	r, ctx := newRotation(t, redisfailoverv1.RedisFailoverStatus{}, true)
	p := plan{kind: config.KillMaster, action: func(context.Context) error {
		return notAppliedError{errors.New("no step")}
	}, converged: func(state) error { return nil }}
	r.m.execute(ctx, 1, authState(), p, r.m.log)
	if n := testutil.ToFloat64(r.m.total.WithLabelValues(string(config.KillMaster), resultSkipped)); n != 1 {
		t.Errorf("skipped %v", n)
	}
	if n := testutil.ToFloat64(r.m.total.WithLabelValues(string(config.KillMaster), resultRejected)); n != 0 {
		t.Errorf("rejected %v", n)
	}
}
