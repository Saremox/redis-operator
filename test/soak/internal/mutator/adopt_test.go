package mutator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
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

// past is a start of the tester so long ago that the adoption looks once.
var past = time.Now().Add(-time.Hour)

const (
	imageRedis74 = "redis:7.4.11-alpine"
	imageValkey8 = "valkey/valkey:8.1.10-alpine"
)

// stuckState is the edge instance in the middle of a failed-safe change from
// redis-7.4 to valkey-8: the spec has the new image, the master rfr-x-0 and
// the replica rfr-x-1 run the old one, and the replica rfr-x-2 runs the new one,
// is not Ready and does not replicate.
func stuckState() state {
	s := onImages(imageValkey8, "")
	for i := range 2 {
		s.redis[i].Spec.Containers[0].Image = imageRedis74
	}
	s.redis[2].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	s.servers = map[string]server{"rfr-x-2": {fields: map[string]string{"master_link_status": "down"}}}
	return s
}

func TestStuckChange(t *testing.T) {
	notReady := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}
	tests := []struct {
		name     string
		instance string
		change   func(s *state)
		master   string
		// tweak changes the mutator before the check.
		tweak func(m *Mutator)
		want  bool
	}{
		{name: "one replica stuck on the new image", want: true},
		{name: "the link is unreadable", change: func(s *state) { s.servers["rfr-x-2"] = server{err: errors.New("refused")} }, want: true},
		{name: "the link is up", change: func(s *state) { s.servers["rfr-x-2"] = server{fields: map[string]string{"master_link_status": "up"}} }},
		{name: "a fail edge", tweak: func(m *Mutator) {
			m.in.Chain.Expect = nil
			m.versions.Edges[slices.IndexFunc(m.versions.Edges, func(e config.Edge) bool { return e.String() == "redis-7.4 -> valkey-8" })].Expect = config.ExpectFail
		}, want: true},
		{name: "an ok edge", instance: "chain", change: func(s *state) {
			for i := range s.redis {
				s.redis[i].Spec.Containers[0].Image = "redis:7.2.16-alpine"
			}
			s.redis[2].Spec.Containers[0].Image = "redis:7.4.11-alpine"
			s.rf.Spec.Redis.Image = "redis:7.4.11-alpine"
		}},
		{name: "an edge that is not configured", change: func(s *state) {
			s.rf.Spec.Redis.Image = imageRedis74
			for i := range 2 {
				s.redis[i].Spec.Containers[0].Image = imageValkey8
			}
			s.redis[2].Spec.Containers[0].Image = imageRedis74
		}},
		{name: "an instance without a chain", instance: "mixed"},
		{name: "an image that is no version", change: func(s *state) { s.rf.Spec.Redis.Image = "valkey/valkey:8-alpine" }},
		{name: "another pod that is no version", change: func(s *state) { s.redis[1].Spec.Containers[0].Image = "redis:6-alpine" }},
		{name: "other pods on two versions", change: func(s *state) { s.redis[1].Spec.Containers[0].Image = "redis:8.10.2-alpine" }},
		{name: "the pod that is not Ready runs the old image", change: func(s *state) {
			s.redis[1].Status.Conditions = []corev1.PodCondition{notReady}
			s.redis[2].Spec.Containers[0].Image = imageRedis74
			s.redis[2].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}},
		{name: "the pod on the new image is Ready", change: func(s *state) {
			s.redis[2].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}},
		{name: "two pods are not Ready", change: func(s *state) { s.redis[1].Status.Conditions = []corev1.PodCondition{notReady} }},
		{name: "two pods on the new image are not Ready", change: func(s *state) {
			s.redis[1].Spec.Containers[0].Image = imageValkey8
			s.redis[1].Status.Conditions = []corev1.PodCondition{notReady}
		}},
		{name: "all pods on the new image", change: func(s *state) {
			for i := range s.redis {
				s.redis[i].Spec.Containers[0].Image = imageValkey8
			}
		}},
		{name: "no pod on the new image", change: func(s *state) { s.redis[2].Spec.Containers[0].Image = imageRedis74 }},
		{name: "the master is unknown", master: "rfr-x-9"},
		{name: "the master is the pod on the new image", master: "rfr-x-2"},
		{name: "the last pod is missing", change: func(s *state) { s.redis = s.redis[:2] }},
		{name: "a middle pod is missing", change: func(s *state) { s.redis = slices.Delete(s.redis, 1, 2) }},
		{name: "one pod", change: func(s *state) {
			s.rf.Spec.Redis.Replicas = 1
			s.redis = s.redis[2:]
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := cmpOr(tc.instance, "edge")
			m := versionMutator(t, name)
			s := stuckState()
			if name == "chain" {
				s.rf.Spec.Redis.Replicas = 3
			}
			if tc.change != nil {
				tc.change(&s)
			}
			if tc.tweak != nil {
				tc.tweak(m)
			}
			master := cmpOr(tc.master, "rfr-x-0")
			tr, pod := m.stuckChange(s, master)
			if (tr != nil) != tc.want {
				t.Fatalf("stuckChange = %v, want a change: %t", tr, tc.want)
			}
			if tc.want && (pod.Name != "rfr-x-2" || tr.edge.String() != "redis-7.4 -> valkey-8" || tr.edge.Expect == config.ExpectOK || tr.from.Name != "redis-7.4" || tr.to.Name != "valkey-8") {
				t.Errorf("stuckChange = %+v on %s", tr, pod.Name)
			}
		})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// loopbackServers starts one fake server for each loopback address, all on
// the same port, and returns the port. handle gives the pre-hook of an
// address.
func loopbackServers(t *testing.T, addrs []string, handle func(addr string) func(*miniserver.Peer, string, ...string) bool) int {
	t.Helper()
	for range 10 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		var started []*miniredis.Miniredis
		ok := true
		for _, addr := range addrs {
			m := miniredis.NewMiniRedis()
			if err := m.StartAddr(net.JoinHostPort(addr, fmt.Sprint(port))); err != nil {
				ok = false
				break
			}
			started = append(started, m)
			m.Server().SetPreHook(handle(addr))
		}
		if ok {
			t.Cleanup(func() {
				for _, m := range started {
					m.Close()
				}
			})
			return port
		}
		for _, m := range started {
			m.Close()
		}
	}
	t.Skip("no free port on the loopback addresses")
	return 0
}

// stubObserver is the observer of an instance whose master and window are
// set by the test.
type stubObserver struct {
	master, addr string
	quiet        atomic.Bool
	seen         atomic.Bool
}

func (o *stubObserver) Quiet() bool        { return o.quiet.Load() }
func (o *stubObserver) Master() string     { return o.master }
func (o *stubObserver) MasterAddr() string { return o.addr }
func (o *stubObserver) Report() observer.Report {
	if !o.seen.Load() {
		return observer.Report{}
	}
	return observer.Report{At: time.Now()}
}

// adoptEnv is a mutator of the edge instance, and the cluster that a
// restarted tester finds: three redis pods on loopback addresses, the
// master edge-0 and the replica edge-1 on redis-7.4, the replica edge-2 on
// valkey-8 and stuck.
type adoptEnv struct {
	m        *Mutator
	kube     *fake.Clientset
	reg      *prometheus.Registry
	logs     *bytes.Buffer
	deletes  atomic.Int32
	creates  atomic.Int32
	logCalls atomic.Int32
	// readOnly makes the master refuse writes.
	readOnly atomic.Bool
	// logLine is what the log of the pod on the new image says.
	logLine atomic.Pointer[string]
}

func newAdoptEnv(t *testing.T) *adoptEnv {
	t.Helper()
	e := &adoptEnv{reg: prometheus.NewRegistry(), logs: &bytes.Buffer{}}
	line := "7:S 01 Oct 2026 21:49:55.532 # Can't handle RDB format version 12"
	e.logLine.Store(&line)
	ips := []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}
	info := map[string]string{
		ips[0]: "# Server\r\nredis_version:7.4.11\r\n# Replication\r\nrole:master\r\n",
		ips[1]: "# Server\r\nredis_version:7.4.11\r\n# Replication\r\nrole:slave\r\nmaster_link_status:up\r\n",
		ips[2]: "# Server\r\nredis_version:7.2.4\r\nserver_name:valkey\r\nvalkey_version:8.1.10\r\n# Replication\r\nrole:slave\r\nmaster_link_status:down\r\n",
	}
	port := loopbackServers(t, ips, func(addr string) func(*miniserver.Peer, string, ...string) bool {
		return func(c *miniserver.Peer, cmd string, _ ...string) bool {
			switch {
			case strings.EqualFold(cmd, "INFO"):
				c.WriteBulk(info[addr])
				return true
			case strings.EqualFold(cmd, "SET") && e.readOnly.Load():
				c.WriteError("READONLY You can't write against a read only replica.")
				return true
			}
			return false
		}
	})

	cfg, err := config.Parse([]byte(versionsConfig))
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Instances[slices.IndexFunc(cfg.Instances, func(in config.Instance) bool { return in.Name == "edge" })]
	var objects []runtime.Object
	objects = append(objects, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "rfr-edge", Namespace: in.Namespace}})
	for i, ip := range ips {
		image, ready := imageRedis74, corev1.ConditionTrue
		if i == 2 {
			image, ready = imageValkey8, corev1.ConditionFalse
		}
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("rfr-edge-%d", i), Namespace: in.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": "edge", "app.kubernetes.io/component": "redis",
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: redisName, Image: image}}},
			Status: corev1.PodStatus{
				PodIP:      ip,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
			},
		})
	}
	e.kube = fake.NewClientset(objects...)
	logger := slog.New(slog.DiscardHandler)
	template, err := instances.New(in, e.kube, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	rf := template.Build(imageValkey8, "")
	rf.UID = "old"
	rf.Spec.Auth.SecretPath = ""
	rf.Spec.Redis.Replicas = 3
	rf.Spec.Redis.Port = int32(port)
	rfs := rffake.NewSimpleClientset(rf)
	// The operator deletes what it made with the RedisFailover.
	rfs.PrependReactor("delete", "redisfailovers", func(k8stesting.Action) (bool, runtime.Object, error) {
		e.deletes.Add(1)
		ctx := context.Background()
		if err := e.kube.AppsV1().StatefulSets(in.Namespace).Delete(ctx, "rfr-edge", metav1.DeleteOptions{}); err != nil {
			return true, nil, err
		}
		for i := range ips {
			if err := e.kube.CoreV1().Pods(in.Namespace).Delete(ctx, fmt.Sprintf("rfr-edge-%d", i), metav1.DeleteOptions{}); err != nil {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
	rfs.PrependReactor("create", "redisfailovers", func(k8stesting.Action) (bool, runtime.Object, error) {
		e.creates.Add(1)
		return false, nil, nil
	})
	inst, err := instances.New(in, e.kube, rfs, logger)
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(func(string) (*corev1.Secret, error) { return nil, errors.New("no Secret") })
	mt := metrics.New(e.reg, time.Minute)
	m := New(in, cfg, e.kube, rfs, nil, nil, a, &global.Lock{}, inst, mt, slog.New(slog.NewJSONHandler(e.logs, nil)))
	m.timeout = time.Second
	m.grace = 10 * time.Millisecond
	m.observerCfg.Interval.Duration = 10 * time.Millisecond
	m.observer = &stubObserver{master: "rfr-edge-0", addr: net.JoinHostPort(ips[0], fmt.Sprint(port))}
	m.observer.(*stubObserver).seen.Store(true)
	m.hold = func(time.Duration, func(context.Context) error) holdHandle {
		return &fakeHold{done: make(chan bool, 1), applied: func() {}}
	}
	m.logs = func(_ context.Context, pod string, _ *corev1.PodLogOptions) ([]byte, error) {
		e.logCalls.Add(1)
		if pod != "rfr-edge-2" {
			return []byte("1:M * Ready to accept connections tcp\n"), nil
		}
		return []byte(*e.logLine.Load() + "\n"), nil
	}
	e.m = m
	return e
}

func (e *adoptEnv) transitions(result string) float64 {
	return testutil.ToFloat64(e.m.transitions.WithLabelValues("redis-7.4", "valkey-8", config.ExpectUnknown, result))
}

func (e *adoptEnv) findings() float64 {
	return testutil.ToFloat64(e.m.findings.WithLabelValues(invVersionTransition)) +
		testutil.ToFloat64(e.m.findings.WithLabelValues(invResetIncomplete))
}

// A tester that restarts in the middle of a failed-safe change judges it as
// the former tester would have, and resets the instance once.
func TestAdopt(t *testing.T) {
	e := newAdoptEnv(t)
	e.m.adopt(context.Background(), time.Now())
	if got := e.transitions(transitionFailedSafe); got != 1 {
		t.Errorf("failed_safe changes = %v, want 1", got)
	}
	if e.deletes.Load() != 1 || e.creates.Load() != 1 {
		t.Errorf("deletes %d, creates %d, want 1 each", e.deletes.Load(), e.creates.Load())
	}
	if got := e.findings(); got != 0 {
		t.Errorf("findings = %v, want 0", got)
	}
	if got := testutil.ToFloat64(e.m.total.WithLabelValues(string(config.Reset), resultConverged)); got != 1 {
		t.Errorf("converged resets = %v, want 1", got)
	}
	var judged string
	for line := range strings.Lines(e.logs.String()) {
		if strings.Contains(line, `"version transition"`) {
			judged = line
		}
	}
	for _, want := range []string{`"adopted":true`, `"result":"failed_safe"`, `"expect":"unknown"`, `"master":"rfr-edge-0"`, `"level":"WARN"`} {
		if !strings.Contains(judged, want) {
			t.Errorf("the log line %q has no %s", judged, want)
		}
	}
	if !strings.Contains(e.logs.String(), "after redis-7.4 -\u003e valkey-8 didn't converge, found after a restart") {
		t.Errorf("the reset does not say why:\n%s", e.logs)
	}
}

// A change that the tester does not adopt is left to the observer: no
// judgement, no reset.
func TestAdoptRefuses(t *testing.T) {
	tests := map[string]func(e *adoptEnv){
		"the master refuses writes": func(e *adoptEnv) { e.readOnly.Store(true) },
		"the log has no load error": func(e *adoptEnv) {
			line := "1:S * Ready to accept connections tcp"
			e.logLine.Store(&line)
		},
		"the master is unknown": func(e *adoptEnv) { e.m.observer.(*stubObserver).master = "" },
		"the pod is Ready": func(e *adoptEnv) {
			p, _ := e.kube.CoreV1().Pods("ns").Get(context.Background(), "rfr-edge-2", metav1.GetOptions{})
			p.Status.Conditions[0].Status = corev1.ConditionTrue
			_, _ = e.kube.CoreV1().Pods("ns").Update(context.Background(), p, metav1.UpdateOptions{})
		},
		"the instance has no template": func(e *adoptEnv) { e.m.instance = nil },
		"the instance has no chain":    func(e *adoptEnv) { e.m.in.Chain = nil },
		"the instance cannot be read": func(e *adoptEnv) {
			e.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("denied")
			})
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			e := newAdoptEnv(t)
			setup(e)
			e.m.adopt(context.Background(), past)
			for _, result := range []string{transitionOK, transitionFailedSafe, transitionFailedUnsafe} {
				if got := e.transitions(result); got != 0 {
					t.Errorf("%s changes = %v, want 0", result, got)
				}
			}
			if e.deletes.Load() != 0 || e.creates.Load() != 0 {
				t.Errorf("deletes %d, creates %d, want none", e.deletes.Load(), e.creates.Load())
			}
			if got := e.findings(); got != 0 {
				t.Errorf("findings = %v, want 0", got)
			}
		})
	}
}

// A pod that recovers during the grace period is no stuck change.
func TestAdoptRechecks(t *testing.T) {
	e := newAdoptEnv(t)
	var reads atomic.Int32
	e.m.logs = func(_ context.Context, pod string, _ *corev1.PodLogOptions) ([]byte, error) {
		// The second read happens after the grace period.
		if pod == "rfr-edge-2" && reads.Add(1) == 1 {
			return []byte("# Can't handle RDB format version 12\n"), nil
		}
		return nil, nil
	}
	e.m.adopt(context.Background(), past)
	if e.deletes.Load() != 0 || e.transitions(transitionFailedSafe) != 0 {
		t.Errorf("deletes %d, failed_safe %v", e.deletes.Load(), e.transitions(transitionFailedSafe))
	}
}

// Ending the tester during the grace period ends the adoption.
func TestAdoptStops(t *testing.T) {
	e := newAdoptEnv(t)
	e.m.grace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.adopt(ctx, time.Now())
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopt did not return")
	}
	if e.deletes.Load() != 0 {
		t.Errorf("deletes = %d, want 0", e.deletes.Load())
	}
}

// The adoption waits for the first round of the observer, and ends with the
// tester.
func TestAdoptWaitsForTheObserver(t *testing.T) {
	e := newAdoptEnv(t)
	e.m.observer.(*stubObserver).seen.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.adopt(ctx, time.Now())
	}()
	time.Sleep(100 * time.Millisecond)
	if e.logCalls.Load() != 0 {
		t.Error("read a log before the observer saw the instance")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopt did not return")
	}
}

// Run adopts before it waits for a quiet instance.
func TestRunAdopts(t *testing.T) {
	e := newAdoptEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.deletes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if e.deletes.Load() != 1 || e.transitions(transitionFailedSafe) != 1 {
		t.Errorf("deletes %d, failed_safe %v, want 1 each", e.deletes.Load(), e.transitions(transitionFailedSafe))
	}
}

// The default log reader reads the log of a pod from the cluster.
func TestDefaultLogs(t *testing.T) {
	e := newAdoptEnv(t)
	m := New(e.m.in, e.m.versions, e.kube, nil, nil, nil, nil, &global.Lock{}, nil, metrics.New(prometheus.NewRegistry(), time.Minute), slog.New(slog.DiscardHandler))
	b, err := m.logs(context.Background(), "rfr-edge-2", &corev1.PodLogOptions{})
	if err != nil || len(b) == 0 {
		t.Errorf("logs = %q, %v", b, err)
	}
}

// The gauge shows how long the mutator waits for a quiet instance, and is 0
// when the instance is quiet. New registers it, at 0.
func TestStalledGauge(t *testing.T) {
	e := newAdoptEnv(t)
	families, err := e.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() != "redis_soak_mutation_stalled_seconds" {
			continue
		}
		found = true
		labels := map[string]string{}
		for _, l := range f.GetMetric()[0].GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if _, ok := labels["mode"]; !ok || labels["rf"] != "edge" || labels["namespace"] != "ns" || f.GetMetric()[0].GetGauge().GetValue() != 0 {
			t.Errorf("series %v = %v", labels, f.GetMetric()[0].GetGauge().GetValue())
		}
	}
	if !found {
		t.Fatal("redis_soak_mutation_stalled_seconds is not registered at its start")
	}

	o := e.m.observer.(*stubObserver)
	done := make(chan bool)
	go func() { done <- e.m.waitQuiet(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for testutil.ToFloat64(e.m.stalled) < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := testutil.ToFloat64(e.m.stalled); got < 1 {
		t.Errorf("stalled = %v after waiting, want at least 1", got)
	}
	o.quiet.Store(true)
	select {
	case ok := <-done:
		if !ok {
			t.Error("waitQuiet returned false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitQuiet did not return")
	}
	if got := testutil.ToFloat64(e.m.stalled); got != 0 {
		t.Errorf("stalled = %v when quiet, want 0", got)
	}
}

// judge counts a version change by its result, and a finding for a change
// that failed unsafely.
func TestJudge(t *testing.T) {
	e := newAdoptEnv(t)
	tr, _ := e.m.stuckChange(stuckState(), "rfr-x-0")
	if tr == nil {
		t.Fatal("no change")
	}
	log := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		converged bool
		lost      int
		want      string
		findings  float64
	}{
		{true, 0, transitionOK, 0},
		{false, 0, transitionFailedSafe, 0},
		{false, 2, transitionFailedUnsafe, 1},
	} {
		if got := e.m.judge(context.Background(), tr, c.converged, c.lost, nil, log); got != c.want {
			t.Errorf("judge = %s, want %s", got, c.want)
		}
		if got := e.transitions(c.want); got != 1 {
			t.Errorf("%s changes = %v, want 1", c.want, got)
		}
		if got := e.findings(); got != c.findings {
			t.Errorf("findings = %v, want %v", got, c.findings)
		}
	}
}

// setPod changes the pod rfr-edge-2 of the environment.
func (e *adoptEnv) setPod(t *testing.T, image string, ready corev1.ConditionStatus) {
	t.Helper()
	pods := e.kube.CoreV1().Pods("ns")
	p, err := pods.Get(context.Background(), "rfr-edge-2", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Spec.Containers[0].Image = image
	p.Status.Conditions[0].Status = ready
	if _, err := pods.Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// listCalls counts the lists of pods.
func (e *adoptEnv) listCalls() *atomic.Int32 {
	var n atomic.Int32
	e.kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		n.Add(1)
		return false, nil, nil
	})
	return &n
}

// A restart can come before the operator replaced a pod, and before the new
// pod logged its load error. The adoption waits for both, and adopts once.
func TestAdoptPolls(t *testing.T) {
	e := newAdoptEnv(t)
	e.setPod(t, imageRedis74, corev1.ConditionTrue)
	lists := e.listCalls()
	var reads atomic.Int32
	e.m.logs = func(_ context.Context, pod string, _ *corev1.PodLogOptions) ([]byte, error) {
		if pod == "rfr-edge-2" && reads.Add(1) >= 3 {
			return []byte("# Can't handle RDB format version 12\n"), nil
		}
		return nil, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.adopt(context.Background(), time.Now())
	}()
	time.Sleep(100 * time.Millisecond)
	if e.deletes.Load() != 0 || lists.Load() < 3 {
		t.Fatalf("deletes %d, lists %d: the adoption did not wait", e.deletes.Load(), lists.Load())
	}
	e.setPod(t, imageValkey8, corev1.ConditionFalse)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("adopt did not return")
	}
	if reads.Load() < 3 {
		t.Errorf("log reads = %d, want at least 3", reads.Load())
	}
	if e.deletes.Load() != 1 || e.creates.Load() != 1 || e.transitions(transitionFailedSafe) != 1 {
		t.Errorf("deletes %d, creates %d, failed_safe %v, want 1 each", e.deletes.Load(), e.creates.Load(), e.transitions(transitionFailedSafe))
	}
}

// An instance that becomes quiet while the adoption waits is not adopted.
func TestAdoptStopsWhenQuiet(t *testing.T) {
	e := newAdoptEnv(t)
	e.setPod(t, imageRedis74, corev1.ConditionTrue)
	lists := e.listCalls()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.adopt(context.Background(), time.Now())
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("adopt returned before the instance was quiet")
	default:
	}
	e.m.observer.(*stubObserver).quiet.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopt did not return")
	}
	if lists.Load() < 3 || e.deletes.Load() != 0 || e.transitions(transitionFailedSafe) != 0 {
		t.Errorf("lists %d, deletes %d, failed_safe %v", lists.Load(), e.deletes.Load(), e.transitions(transitionFailedSafe))
	}
}

// The deadline ends the wait for a stuck change: the convergence timeout
// minus the grace, the verification bound and adoptMargin, after the start.
func TestAdoptDeadline(t *testing.T) {
	e := newAdoptEnv(t)
	e.setPod(t, imageRedis74, corev1.ConditionTrue)
	e.m.convergeTimeout = 10 * time.Minute
	started := time.Now()
	if got, want := e.m.adoptDeadline(started), started.Add(10*time.Minute-e.m.grace-verifyBound-adoptMargin); !got.Equal(want) {
		t.Fatalf("deadline %s, want %s", got, want)
	}
	// 150 ms remain.
	started = time.Now().Add(-(10*time.Minute - e.m.grace - verifyBound - adoptMargin) + 150*time.Millisecond)
	lists := e.listCalls()
	begin := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.adopt(context.Background(), started)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopt did not return at its deadline")
	}
	if d := time.Since(begin); d < 100*time.Millisecond {
		t.Errorf("returned after %s, before the deadline", d)
	}
	if lists.Load() < 3 || e.deletes.Load() != 0 {
		t.Errorf("lists %d, deletes %d", lists.Load(), e.deletes.Load())
	}
}

// Another mutation or a chaos action can run while the adoption waits for the
// lock. The adoption reads the instance again after it.
func TestAdoptRechecksAfterTheLock(t *testing.T) {
	e := newAdoptEnv(t)
	unlock := e.m.lock.Exclusive()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.adopt(context.Background(), time.Now())
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.logCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	e.setPod(t, imageRedis74, corev1.ConditionTrue)
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopt did not return")
	}
	if e.deletes.Load() != 0 || e.transitions(transitionFailedSafe) != 0 {
		t.Errorf("deletes %d, failed_safe %v, want none", e.deletes.Load(), e.transitions(transitionFailedSafe))
	}
}

// Ending the tester ends the wait for a stuck change.
func TestAdoptStopsWhilePolling(t *testing.T) {
	e := newAdoptEnv(t)
	e.setPod(t, imageRedis74, corev1.ConditionTrue)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.m.adopt(ctx, time.Now())
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("adopt did not return")
	}
}
