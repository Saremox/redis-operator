package observer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

func collectPod(name, component, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{
			"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": "x", "app.kubernetes.io/component": component,
		}},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

func endpointSlice(name string, eps ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{discoveryv1.LabelServiceName: "rfrm-x"}},
		Endpoints:  eps,
	}
}

func collectObserver(t *testing.T, yaml string, rf *redisfailoverv1.RedisFailover, objects ...runtime.Object) (*Observer, *fake.Clientset, *rffake.Clientset) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset(objects...)
	rfs := rffake.NewSimpleClientset(rf)
	o := New(cfg.Instances[0], cfg, kube, rfs, auth.New(nil), nil, metrics.New(prometheus.NewRegistry(), time.Minute),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return o, kube, rfs
}

// collect reads the RedisFailover, the pods, the EndpointSlices and then each
// pod, and returns the first error of a read of the API.
func TestCollect(t *testing.T) {
	mr := miniredis.RunT(t)
	port, err := strconv.Atoi(mr.Port())
	if err != nil {
		t.Fatal(err)
	}
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", UID: "u1", Generation: 4}}
	rf.Spec.Redis.Port = int32(port)
	rf.Spec.Redis.Replicas = 2
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Policy: "noeviction"}
	rf.Spec.Sentinel.Enabled = new(true)
	rf.Status.State, rf.Status.Message = "Healthy", "ok"
	ready, notReady := true, false
	o, kube, _ := collectObserver(t, "instances: [{name: x, namespace: ns, mode: sentinel}]", rf,
		collectPod("rfr-x-1", "redis", ""),
		collectPod("rfr-x-0", "redis", "127.0.0.1"),
		collectPod("rfs-x-0", "sentinel", ""),
		collectPod("other", "exporter", "10.0.0.9"),
		endpointSlice("a", discoveryv1.Endpoint{Addresses: []string{"10.0.0.2", "10.0.0.1"}},
			discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}}),
		endpointSlice("b", discoveryv1.Endpoint{Addresses: []string{"10.0.0.1", "10.0.0.4"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}),
	)
	s, generation, err := o.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if generation != 4 || s.uid != "u1" || s.port != port || s.state != "Healthy" || s.message != "ok" || !s.sentinel || s.redisReplicas != 2 || s.sentinelReplicas != 3 {
		t.Errorf("snapshot %+v", s)
	}
	if !slices.Equal(s.endpoints, []string{"10.0.0.2", "10.0.0.1", "10.0.0.4"}) {
		t.Errorf("endpoints %v", s.endpoints)
	}
	if len(s.redis) != 2 || s.redis[0].Name != "rfr-x-0" || s.redis[1].Name != "rfr-x-1" || len(s.sentinels) != 1 {
		t.Fatalf("pods %+v %+v", s.redis, s.sentinels)
	}
	if p := s.redis[1]; !errors.Is(p.err, errNoIP) || !errors.Is(p.configErr, errNoIP) {
		t.Errorf("pod without an IP: %+v", p)
	}
	// miniredis has no CONFIG: its error names the keys. With maxMemory,
	// collect also asks for maxmemory and maxmemory-policy.
	if p := s.redis[0]; p.err != nil || p.info == nil || p.configErr == nil ||
		!strings.Contains(p.configErr.Error(), "`get`, `replica-priority`, `maxmemory`, `maxmemory-policy`, ") {
		t.Errorf("pod: %+v", p)
	}
	if p := s.sentinels[0]; !errors.Is(p.err, errNoIP) {
		t.Errorf("sentinel: %+v", p)
	}
	if s.bootstrap != nil || s.sourceErr != nil {
		t.Errorf("source %+v", s)
	}
	actions := kube.Actions()
	calls := make([]string, 0, len(actions))
	for _, a := range actions {
		calls = append(calls, a.GetVerb()+" "+a.GetResource().Resource)
	}
	if got := strings.Join(calls, ", "); got != "list pods, list endpointslices" {
		t.Errorf("calls %s", got)
	}

	// Without maxMemory, collect asks for the custom config keys only.
	rf.Spec.Redis.MaxMemory = nil
	o, _, _ = collectObserver(t, "instances: [{name: x, namespace: ns}]", rf, collectPod("rfr-x-0", "redis", "127.0.0.1"))
	if s, _, err = o.collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := s.redis[0]; p.err != nil || p.info == nil || p.configErr == nil || !strings.HasSuffix(p.configErr.Error(), "`get`, `replica-priority`, ") {
		t.Errorf("pod without keys: %+v", p)
	}
}

// A bootstrapping instance reads where the master of its source is.
func TestCollectSource(t *testing.T) {
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "src"}
	o, _, _ := collectObserver(t, "instances: [{name: x, namespace: ns}]", rf)
	s, _, err := o.collect(context.Background())
	if err != nil || s.bootstrap == nil || s.sourceErr != nil {
		t.Errorf("without a source: %+v, %v", s, err)
	}
	src, _, _ := collectObserver(t, "instances: [{name: x, namespace: ns}]", rf)
	o.SetSource(src)
	if s, _, err = o.collect(context.Background()); err != nil || !errors.Is(s.sourceErr, errNoMaster) {
		t.Errorf("with a source: %+v, %v", s.sourceErr, err)
	}
}

func TestCollectErrors(t *testing.T) {
	denied := errors.New("denied")
	fail := func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, denied }
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	for _, c := range []struct {
		name  string
		setup func(kube *fake.Clientset, rfs *rffake.Clientset)
		calls string
	}{
		{"RedisFailover", func(_ *fake.Clientset, rfs *rffake.Clientset) { rfs.PrependReactor("get", "redisfailovers", fail) }, ""},
		{"pods", func(kube *fake.Clientset, _ *rffake.Clientset) { kube.PrependReactor("list", "pods", fail) }, "list pods"},
		{"EndpointSlices", func(kube *fake.Clientset, _ *rffake.Clientset) { kube.PrependReactor("list", "endpointslices", fail) },
			"list pods, list endpointslices"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, kube, rfs := collectObserver(t, "instances: [{name: x, namespace: ns}]", rf, collectPod("rfr-x-0", "redis", ""))
			c.setup(kube, rfs)
			if _, generation, err := o.collect(context.Background()); !errors.Is(err, denied) || generation != 0 {
				t.Errorf("error %v, generation %d", err, generation)
			}
			actions := kube.Actions()
			calls := make([]string, 0, len(actions))
			for _, a := range actions {
				calls = append(calls, a.GetVerb()+" "+a.GetResource().Resource)
			}
			if got := strings.Join(calls, ", "); got != c.calls {
				t.Errorf("calls %q, want %q", got, c.calls)
			}
		})
	}
}
