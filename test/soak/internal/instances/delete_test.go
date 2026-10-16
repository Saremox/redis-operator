package instances

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/config"
)

// deleteEnv records the calls of both fakes in one order, without repeats in
// a row. It cancels the run at the 40th call, so that a wait that never ends
// repeats a few times.
type deleteEnv struct {
	kube   *fake.Clientset
	rfs    *rffake.Clientset
	mu     sync.Mutex
	calls  []string
	n      int
	cancel context.CancelFunc
}

func (e *deleteEnv) record(a k8stesting.Action) (bool, runtime.Object, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	if e.n == 40 {
		e.cancel()
	}
	c := a.GetVerb() + " " + a.GetResource().Resource
	if n := len(e.calls); n == 0 || e.calls[n-1] != c {
		e.calls = append(e.calls, c)
	}
	return false, nil, nil
}

func (e *deleteEnv) got() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.calls, ", ")
}

func fail(err error) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, err }
}

// Delete calls the API in a fixed order, and stops at the first fault. A
// fault in a wait repeats the wait until ctx is canceled.
func TestDeleteOrder(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": "chain"}
	denied := errors.New("denied")
	notFound := apierrors.NewNotFound(schema.GroupResource{}, "x")
	const (
		waitObjects = "get redisfailovers, get statefulsets, get deployments, list pods"
		waitVolumes = "list persistentvolumeclaims"
	)
	cases := []struct {
		name    string
		objects []runtime.Object
		setup   func(e *deleteEnv)
		calls   string
		err     string
	}{
		{name: "gone", calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes + ", delete secrets"},
		{name: "delete fails", setup: func(e *deleteEnv) { e.rfs.PrependReactor("delete", "redisfailovers", fail(denied)) },
			calls: "delete redisfailovers", err: "denied"},
		{name: "delete not found", setup: func(e *deleteEnv) {
			e.rfs.PrependReactor("delete", "redisfailovers", fail(notFound))
			e.rfs.PrependReactor("get", "redisfailovers", fail(notFound))
		},
			calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes + ", delete secrets"},
		{name: "RedisFailover stays", setup: func(e *deleteEnv) { e.rfs.PrependReactor("get", "redisfailovers", fail(denied)) },
			calls: "delete redisfailovers, get redisfailovers", err: context.Canceled.Error()},
		{name: "StatefulSet stays", objects: []runtime.Object{&appsv1.StatefulSet{ObjectMeta: object("sts", "rfr-chain", labels)}},
			calls: "delete redisfailovers, get redisfailovers, get statefulsets, get redisfailovers, get statefulsets", err: context.Canceled.Error()},
		{name: "Deployment stays", objects: []runtime.Object{&appsv1.Deployment{ObjectMeta: object("deploy", "rfs-chain", labels)}},
			calls: "delete redisfailovers, get redisfailovers, get statefulsets, get deployments, get redisfailovers", err: context.Canceled.Error()},
		{name: "pod list fails", setup: func(e *deleteEnv) { e.kube.PrependReactor("list", "pods", fail(denied)) },
			calls: "delete redisfailovers, " + waitObjects + ", get redisfailovers", err: context.Canceled.Error()},
		{name: "pod stays", objects: []runtime.Object{&corev1.Pod{ObjectMeta: object("pod", "rfr-chain-0", labels)}},
			calls: "delete redisfailovers, " + waitObjects + ", get redisfailovers", err: context.Canceled.Error()},
		{name: "volume list fails", setup: func(e *deleteEnv) { e.kube.PrependReactor("list", "persistentvolumeclaims", fail(denied)) },
			calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes, err: context.Canceled.Error()},
		{name: "volume delete fails", objects: []runtime.Object{&corev1.PersistentVolumeClaim{ObjectMeta: object("pvc", "data-0", labels)}},
			setup: func(e *deleteEnv) { e.kube.PrependReactor("delete", "persistentvolumeclaims", fail(denied)) },
			calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes + ", delete persistentvolumeclaims, " + waitVolumes, err: context.Canceled.Error()},
		{name: "volume delete not found", objects: []runtime.Object{&corev1.PersistentVolumeClaim{ObjectMeta: object("pvc", "data-0", labels)}},
			setup: func(e *deleteEnv) { e.kube.PrependReactor("delete", "persistentvolumeclaims", fail(notFound)) },
			calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes + ", delete persistentvolumeclaims, " + waitVolumes, err: context.Canceled.Error()},
		{name: "volume in deletion", objects: []runtime.Object{&corev1.PersistentVolumeClaim{ObjectMeta: func() metav1.ObjectMeta {
			m := object("pvc", "data-0", labels)
			m.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			m.Finalizers = []string{"x"}
			return m
		}()}}, calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes, err: context.Canceled.Error()},
		{name: "secret delete fails", setup: func(e *deleteEnv) { e.kube.PrependReactor("delete", "secrets", fail(denied)) },
			calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes + ", delete secrets", err: "denied"},
		{name: "secret not found", setup: func(e *deleteEnv) { e.kube.PrependReactor("delete", "secrets", fail(notFound)) },
			calls: "delete redisfailovers, " + waitObjects + ", " + waitVolumes + ", delete secrets"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			e := &deleteEnv{kube: fake.NewClientset(c.objects...), rfs: rffake.NewSimpleClientset(&redisfailoverv1.RedisFailover{ObjectMeta: object("rf", "chain", nil)}),
				cancel: cancel}
			if c.setup != nil {
				c.setup(e)
			}
			e.kube.PrependReactor("*", "*", e.record)
			e.rfs.PrependReactor("*", "*", e.record)
			i, err := New(config.Instance{Name: "chain", Namespace: "ns", Template: "testdata/chain.yaml"}, e.kube, e.rfs, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			i.poll = time.Millisecond
			got := ""
			if err := i.Delete(ctx); err != nil {
				got = err.Error()
			}
			if got != c.err {
				t.Errorf("error %q, want %q", got, c.err)
			}
			if got := e.got(); !strings.HasPrefix(got, c.calls) {
				t.Errorf("calls %s\nwant  %s", got, c.calls)
			}
			if c.err == "" && e.got() != c.calls {
				t.Errorf("calls %s\nwant  %s", e.got(), c.calls)
			}
			// A wait that fails never reaches a later step.
			if c.err == context.Canceled.Error() && slices.Contains(e.calls, "delete secrets") {
				t.Errorf("calls %s reach the Secret", e.got())
			}
		})
	}
}
