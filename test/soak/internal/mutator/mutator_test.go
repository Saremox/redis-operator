package mutator

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/instances"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// failedResetMutator returns the mutator of the chain instance, whose reset
// cannot create the RedisFailover again. Its observer does not run: it
// never sees the RedisFailover again.
func failedResetMutator(t *testing.T, d Data) *Mutator {
	t.Helper()
	cfg, err := config.Parse([]byte(versionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Instances[slices.IndexFunc(cfg.Instances, func(in config.Instance) bool { return in.Name == "chain" })]
	log := slog.New(slog.DiscardHandler)
	kube := fake.NewClientset(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "rfr-chain", Namespace: in.Namespace}})
	inst, err := instances.New(in, kube, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	rf := inst.Build("", "")
	rf.UID = "old"
	rfs := rffake.NewSimpleClientset(rf)
	// The operator deletes what it made with the RedisFailover.
	rfs.PrependReactor("delete", "redisfailovers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return false, nil, kube.AppsV1().StatefulSets(in.Namespace).Delete(context.Background(), "rfr-chain", metav1.DeleteOptions{})
	})
	rfs.PrependReactor("create", "redisfailovers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("denied")
	})
	if inst, err = instances.New(in, kube, rfs, log); err != nil {
		t.Fatal(err)
	}
	a := auth.New(func(name string) (*corev1.Secret, error) {
		return kube.CoreV1().Secrets(in.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	})
	mt := metrics.New(prometheus.NewRegistry(), time.Minute)
	lock := &global.Lock{}
	o := observer.New(in, cfg, kube, rfs, a, lock, mt, log)
	m := New(in, cfg, kube, rfs, o, d, a, lock, inst, mt, log)
	m.cfg.Timeouts = map[config.Kind]config.Timeout{config.Reset: {Base: metav1.Duration{Duration: 50 * time.Millisecond}}}
	m.observerCfg.Interval.Duration = 10 * time.Millisecond
	return m
}

// mutateWithin runs a mutation, and fails the test if it does not return
// within a short time.
func mutateWithin(t *testing.T, m *Mutator, kind config.Kind) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.mutate(ctx, 1, stepRand(1, m.in, 1), kind)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatalf("the %s did not return", kind)
	}
}

// A reset that cannot create the instance again leaves nothing to observe.
// It must end as a finding, and not hold the global lock forever.
func TestResetNotCreated(t *testing.T) {
	m := failedResetMutator(t, fakeData{})
	mutateWithin(t, m, config.Reset)
	if got := testutil.ToFloat64(m.findings.WithLabelValues("reset_incomplete")); got != 1 {
		t.Errorf("findings = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.total.WithLabelValues(string(config.Reset), resultRejected)); got != 1 {
		t.Errorf("rejected resets = %v, want 1", got)
	}
}
