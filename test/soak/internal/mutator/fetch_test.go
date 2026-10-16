package mutator

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
)

// fetch reads the RedisFailover, its pods, its StatefulSet and the Sentinel
// objects in a fixed order, and stops at the first error.
func TestFetchOrder(t *testing.T) {
	const (
		// The auth source reads the Secret of the password.
		read     = "get redisfailovers x, get secrets x-auth, list pods "
		base     = read + ", get statefulsets rfr-x"
		sentinel = base + ", get deployments rfs-x"
		objects  = sentinel + ", get services rfs-x, get configmaps rfs-x"
	)
	denied := errors.New("denied")
	notFound := apierrors.NewNotFound(schema.GroupResource{}, "x")
	cases := []struct {
		name       string
		off        bool
		maxMemory  bool
		opts       fetchOpts
		fail       map[string]error
		calls      string
		err        error
		deployment bool
		service    bool
		configMap  bool
	}{
		{name: "sentinel", calls: sentinel, deployment: true},
		{name: "no sentinel", off: true, calls: base},
		{name: "maxMemory", maxMemory: true, calls: sentinel, deployment: true},
		{name: "objects", off: true, opts: fetchOpts{sentinelObjects: true}, calls: objects, deployment: true, service: true},
		{name: "RedisFailover fails", fail: map[string]error{"get redisfailovers": denied}, calls: "get redisfailovers x", err: denied},
		{name: "pods fail", fail: map[string]error{"list pods": denied}, calls: read, err: denied},
		{name: "StatefulSet fails", fail: map[string]error{"get statefulsets": denied}, calls: base, err: denied},
		{name: "no Deployment", fail: map[string]error{"get deployments": notFound}, calls: sentinel},
		{name: "Deployment fails", fail: map[string]error{"get deployments": denied}, calls: sentinel, err: denied},
		{name: "no Deployment, objects", opts: fetchOpts{sentinelObjects: true}, fail: map[string]error{"get deployments": notFound},
			calls: objects, service: true},
		{name: "Service fails", opts: fetchOpts{sentinelObjects: true}, fail: map[string]error{"get services": denied},
			calls: sentinel + ", get services rfs-x", err: denied},
		{name: "ConfigMap fails", opts: fetchOpts{sentinelObjects: true}, fail: map[string]error{"get configmaps": denied},
			calls: objects, err: denied},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRotation(t, redisfailoverv1.RedisFailoverStatus{})
			rf, err := r.rfs.DatabasesV1().RedisFailovers("ns").Get(context.Background(), "x", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rf.Spec.Sentinel.Enabled = new(!c.off)
			if c.maxMemory {
				rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Policy: "noeviction"}
			}
			if _, err := r.rfs.DatabasesV1().RedisFailovers("ns").Update(context.Background(), rf, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			labels := map[string]string{"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": "x"}
			for _, o := range []runtime.Object{
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "rfs-x", Namespace: "ns"}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "rfs-x", Namespace: "ns"}},
				podWith("rfr-x-1", labels, "redis"), podWith("rfr-x-0", labels, "redis"), podWith("rfs-x-a", labels, "sentinel"),
				podWith("rfr-x-exporter", labels, "exporter"),
			} {
				if err := r.kube.Tracker().Add(o); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			record := func(a k8stesting.Action) (bool, runtime.Object, error) {
				name := ""
				if g, ok := a.(k8stesting.GetAction); ok {
					name = g.GetName()
				}
				key := a.GetVerb() + " " + a.GetResource().Resource
				calls = append(calls, key+" "+name)
				if err := c.fail[key]; err != nil {
					return true, nil, err
				}
				return false, nil, nil
			}
			r.kube.PrependReactor("*", "*", record)
			r.rfs.(*rffake.Clientset).PrependReactor("*", "*", record)
			s, err := r.m.fetch(context.Background(), c.opts)
			if !errors.Is(err, c.err) {
				t.Errorf("error %v, want %v", err, c.err)
			}
			if got := strings.Join(calls, ", "); got != c.calls {
				t.Errorf("calls %s\nwant  %s", got, c.calls)
			}
			if err != nil {
				if s.rf != nil {
					t.Errorf("state %+v on an error", s)
				}
				return
			}
			if len(s.redis) != 2 || s.redis[0].Name != "rfr-x-0" || len(s.sentinels) != 1 || s.sts == nil || s.password != "old" || s.authSecret != "x-auth" {
				t.Errorf("state %+v", s)
			}
			if (s.sentinel != nil) != c.deployment || s.sentinelService != c.service || s.sentinelConfigMap != c.configMap {
				t.Errorf("sentinel objects %v %v %v", s.sentinel != nil, s.sentinelService, s.sentinelConfigMap)
			}
			// The pods have no IP, so the read of their config is empty but
			// done.
			if (s.config != nil) != c.maxMemory || len(s.config) != 0 {
				t.Errorf("config %+v", s.config)
			}
			if s.auth != nil || s.sentinelMasters != nil || s.servers != nil {
				t.Errorf("reads not asked for: %+v", s)
			}
		})
	}
}

func podWith(name string, labels map[string]string, component string) *corev1.Pod {
	l := map[string]string{"app.kubernetes.io/component": component}
	maps.Copy(l, labels)
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: l}}
}
