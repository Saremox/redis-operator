package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// syncBuffer collects the lane's JSON log lines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// line returns the first log line with msg.
func (s *syncBuffer) line(t *testing.T, msg string) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for l := range strings.Lines(s.b.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err == nil && m["msg"] == msg {
			return m
		}
	}
	t.Fatalf("no %q in the log:\n%s", msg, s.b.String())
	return nil
}

func fakeLane(t *testing.T, yaml string, objects ...runtime.Object) (*Lane, *fake.Clientset, *metrics.Metrics, *syncBuffer) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml + "\ninstances: [{name: a, namespace: a}]"))
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset(objects...)
	m := metrics.New(prometheus.NewRegistry(), time.Minute)
	logs := &syncBuffer{}
	l := New(cfg, kube, &global.Lock{}, []Instance{{Name: "a", Observer: &fakeObserver{}}}, "w2", m, slog.New(slog.NewJSONHandler(logs, nil)))
	l.poll = 10 * time.Millisecond
	return l, kube, m, logs
}

func testAction(l *Lane, kind config.ChaosKind) *action {
	return &action{kind: kind, step: 1, r: stepRand(1, 1), log: l.log.With("kind", kind), begin: func() {}}
}

// operatorTakesOver replaces the operator pod on old with one on image,
// which takes the Lease over, as the Deployment and the new operator do.
func operatorTakesOver(ctx context.Context, kube *fake.Clientset, name, image string) error {
	pods := kube.CoreV1().Pods("redis-operator")
	p := operatorPod(name, name, image, true)
	if _, err := pods.Create(ctx, &p, metav1.CreateOptions{}); err != nil {
		return err
	}
	l, err := kube.CoordinationV1().Leases("redis-operator").Get(ctx, "redis-failover-lease", metav1.GetOptions{})
	if err != nil {
		return err
	}
	l.Spec = lease(name+"_1", time.Now()).Spec
	_, err = kube.CoordinationV1().Leases("redis-operator").Update(ctx, l, metav1.UpdateOptions{})
	return err
}

func TestRestartOnFakes(t *testing.T) {
	const image = "redis-operator:a"
	old := operatorPod("op-old", "op-old", image, true)
	l, kube, m, logs := fakeLane(t, "chaos: {kinds: {operator_restart: 1}, timeout: 5s}",
		deployment(image, 1, 1, 1, true), &old, lease("op-old_1", time.Now().Add(-time.Hour)))
	ctx := context.Background()
	disturbed := make(chan string, 1)
	go func() {
		for {
			if _, err := kube.CoreV1().Pods("redis-operator").Get(ctx, "op-old", metav1.GetOptions{}); apierrors.IsNotFound(err) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		disturbed <- l.lock.Disturbance()
		time.Sleep(200 * time.Millisecond)
		if err := operatorTakesOver(ctx, kube, "op-new", image); err != nil {
			t.Error(err)
		}
	}()
	o := l.restart(ctx, testAction(l, config.OperatorRestart))
	if o.result() != resultConverged || len(o.converged) != 1 {
		t.Fatalf("outcome %+v", o)
	}
	if d := <-disturbed; d != "operator restart" {
		t.Errorf("disturbance while the operator was gone: %q", d)
	}
	if d := l.lock.Disturbance(); d != "" {
		t.Errorf("still disturbed: %q", d)
	}
	line := logs.line(t, "operator restarted")
	if line["from"] != "op-old" || line["to"] != "op-new" {
		t.Errorf("logged %v", line)
	}
	if down := line["operator_down_seconds"].(float64); down < 0.2 || down > 2 {
		t.Errorf("operator down %vs", down)
	}
	if n := testutil.CollectAndCount(m.ChaosOperatorDown, "redis_soak_chaos_operator_down_seconds"); n != 1 {
		t.Errorf("%d operator_down series", n)
	}
}

// A restart whose new operator never leads times out, and leaves the
// instances undisturbed.
func TestRestartTimesOut(t *testing.T) {
	const image = "redis-operator:a"
	old := operatorPod("op-old", "op-old", image, true)
	l, _, _, _ := fakeLane(t, "chaos: {kinds: {operator_restart: 1}, timeout: 100ms}",
		deployment(image, 1, 1, 1, true), &old, lease("op-old_1", time.Now()))
	o := l.restart(context.Background(), testAction(l, config.OperatorRestart))
	if o.result() != resultTimeout || !strings.Contains(o.timeout.Error(), "the operator: not within 100ms") {
		t.Errorf("outcome %+v", o)
	}
	if d := l.lock.Disturbance(); d != "" {
		t.Errorf("still disturbed: %q", d)
	}
}

// operator_upgrade upgrades to the other version and back, and runs helm
// with each version's chart and image.
func TestUpgradeOnFakes(t *testing.T) {
	const a, b = "redis-operator:a", "redis-operator:b"
	old := operatorPod("op-a", "op-a", a, true)
	l, kube, m, logs := fakeLane(t, `
chaos:
  kinds: {operator_upgrade: 1}
  timeout: 5s
  upgrade:
    set: [crds.upgradeHook.enabled=true]
    versions:
      - {name: a, chart: /charts/a.tgz, image: "redis-operator:a"}
      - {name: b, chart: /charts/b.tgz, image: "redis-operator:b"}`,
		deployment(a, 1, 1, 1, true), &old, lease("op-a_1", time.Now().Add(-time.Hour)))
	var charts []string
	n := 0
	l.helm = func(ctx context.Context, args ...string) ([]byte, error) {
		charts = append(charts, args[2])
		if !strings.Contains(strings.Join(args, " "), "--set crds.upgradeHook.enabled=true") {
			t.Errorf("args %v", args)
		}
		image := map[string]string{"/charts/a.tgz": a, "/charts/b.tgz": b}[args[2]]
		n++
		ns := "redis-operator"
		pods, _ := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		for _, p := range pods.Items {
			_ = kube.CoreV1().Pods(ns).Delete(ctx, p.Name, metav1.DeleteOptions{})
		}
		if err := operatorTakesOver(ctx, kube, fmt.Sprintf("op-%d", n), image); err != nil {
			return nil, err
		}
		_, err := kube.AppsV1().Deployments(ns).Update(ctx, deployment(image, 1, 1, 1, true), metav1.UpdateOptions{})
		return []byte("Release \"redis-operator\" has been upgraded."), err
	}
	o := l.upgrade(context.Background(), testAction(l, config.OperatorUpgrade))
	if o.result() != resultConverged || len(o.converged) != 2 {
		t.Fatalf("outcome %+v", o)
	}
	if strings.Join(charts, " ") != "/charts/b.tgz /charts/a.tgz" {
		t.Errorf("charts %v", charts)
	}
	line := logs.line(t, "operator upgraded")
	if line["from"] != "a" || line["to"] != "b" || line["leader"] != "op-1" {
		t.Errorf("logged %v", line)
	}
	if n := testutil.CollectAndCount(m.ChaosOperatorDown, "redis_soak_chaos_operator_down_seconds"); n != 1 {
		t.Errorf("%d operator_down series", n)
	}

	// An operator on no configured version isn't upgraded.
	if _, err := kube.AppsV1().Deployments("redis-operator").Update(context.Background(), deployment("redis-operator:c", 1, 1, 1, true), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if o := l.upgrade(context.Background(), testAction(l, config.OperatorUpgrade)); o.result() != resultSkipped {
		t.Errorf("outcome %+v", o)
	}
}

// A failed helm upgrade, e.g. its hook failing, fails the action.
func TestUpgradeFails(t *testing.T) {
	const a = "redis-operator:a"
	old := operatorPod("op-a", "op-a", a, true)
	l, _, _, logs := fakeLane(t, `
chaos:
  kinds: {operator_upgrade: 1}
  timeout: 1s
  upgrade:
    versions:
      - {name: a, chart: /charts/a.tgz, image: "redis-operator:a"}
      - {name: b, chart: /charts/b.tgz, image: "redis-operator:b"}`,
		deployment(a, 1, 1, 1, true), &old, lease("op-a_1", time.Now()))
	l.helm = func(context.Context, ...string) ([]byte, error) {
		return []byte("Error: UPGRADE FAILED: pre-upgrade hooks failed: job redis-operator-crds-upgrade failed: BackoffLimitExceeded"),
			fmt.Errorf("exit status 1")
	}
	o := l.upgrade(context.Background(), testAction(l, config.OperatorUpgrade))
	if o.result() != resultFailed || !strings.Contains(o.err.Error(), "a -> b: helm upgrade: exit status 1") {
		t.Errorf("outcome %+v", o)
	}
	if line := logs.line(t, "helm upgrade failed"); !strings.Contains(line["output"].(string), "BackoffLimitExceeded") {
		t.Errorf("logged %v", line)
	}
}

func TestDrainOnFakes(t *testing.T) {
	node := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		}
	}
	redis0 := nodePod("a", "rfr-a-0", "w1", nil)
	redis1 := nodePod("a", "rfr-a-1", "w2", nil)
	sentinel := nodePod("a", "rfs-a-x", "w1", nil)
	kindnet := nodePod("kube-system", "kindnet-w1", "w1", func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "kindnet"}}
	})
	l, kube, m, logs := fakeLane(t, "chaos: {kinds: {node_drain: 1}, timeout: 5s, drain: {hold: 50ms, timeout: 5s}}",
		node("cp", map[string]string{"node-role.kubernetes.io/control-plane": ""}), node("w1", nil), node("w2", nil),
		&redis0, &redis1, &sentinel, &kindnet)
	ctx := context.Background()
	var mu sync.Mutex
	attempts := map[string]int{}
	var cordoned []bool
	kube.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := action.(k8stesting.CreateAction).GetObject().(interface{ GetName() string })
		w1, _ := kube.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, "", "w1")
		mu.Lock()
		attempts[ev.GetName()]++
		n := attempts[ev.GetName()]
		cordoned = append(cordoned, w1.(*corev1.Node).Spec.Unschedulable)
		mu.Unlock()
		// The Sentinels' budget blocks the first two attempts.
		if ev.GetName() == "rfs-a-x" && n <= 2 {
			return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
		}
		err := kube.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "a", ev.GetName())
		return true, nil, err
	})
	o := l.drain(ctx, testAction(l, config.NodeDrain))
	if o.result() != resultConverged || len(o.converged) != 1 {
		t.Fatalf("outcome %+v", o)
	}
	if attempts["rfr-a-0"] != 1 || attempts["rfs-a-x"] != 3 || len(attempts) != 2 {
		t.Errorf("evictions %v: only w1's, not its DaemonSet pod", attempts)
	}
	for _, c := range cordoned {
		if !c {
			t.Error("evicted from w1 while it was schedulable")
		}
	}
	w1, err := kube.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{})
	if err != nil || w1.Spec.Unschedulable {
		t.Errorf("w1 still cordoned: %v", err)
	}
	if line := logs.line(t, "node drained"); line["node"] != "w1" || line["pdb_blocked"] != 1.0 {
		t.Errorf("logged %v", line)
	}
	if line := logs.line(t, "eviction unblocked"); line["pod"] != "rfs-a-x" {
		t.Errorf("logged %v", line)
	}
	if n := testutil.CollectAndCount(m.ChaosEvictionBlocked, "redis_soak_chaos_eviction_blocked_seconds"); n != 1 {
		t.Errorf("%d eviction_blocked series", n)
	}
	if d := l.lock.Disturbance(); d != "" {
		t.Errorf("still disturbed: %q", d)
	}
}

// A drain whose evictions a budget blocks past the drain timeout times
// out, and still uncordons the node.
func TestDrainBlocked(t *testing.T) {
	w1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := nodePod("a", "rfr-a-0", "w1", nil)
	l, kube, _, _ := fakeLane(t, "chaos: {kinds: {node_drain: 1}, timeout: 2s, drain: {hold: 10ms, timeout: 100ms}}", w1, &pod)
	kube.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
	})
	o := l.drain(context.Background(), testAction(l, config.NodeDrain))
	if o.result() != resultTimeout || !strings.Contains(o.timeout.Error(), "a/rfr-a-0: blocked by a PodDisruptionBudget for") {
		t.Errorf("outcome %v", o.timeout)
	}
	n, err := kube.CoreV1().Nodes().Get(context.Background(), "w1", metav1.GetOptions{})
	if err != nil || n.Spec.Unschedulable {
		t.Errorf("w1 still cordoned: %v", err)
	}
}

// A drain that evicted the only redis pod of an instance without a volume
// reset it: its data is verified as a reset's, every other's as the
// drain's. A drain must lose no write only on volumes, and on a server that
// waits for its replicas on SIGTERM, which Redis 6.2 does not.
func TestDrainResets(t *testing.T) {
	w1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	redis := func(rf string) *corev1.Pod {
		p := nodePod(rf, "rfr-"+rf+"-0", "w1", func(p *corev1.Pod) {
			p.Labels = map[string]string{"app.kubernetes.io/name": rf, "app.kubernetes.io/component": "redis"}
		})
		return &p
	}
	l, kube, _, _ := fakeLane(t, "chaos: {kinds: {node_drain: 1}, timeout: 2s, drain: {hold: 10ms}}", w1, redis("single"), redis("pvc"), redis("pvc62"), redis("empty"))
	single, pvc, pvc62, empty := &fakeData{}, &fakeData{}, &fakeData{}, &fakeData{}
	l.instances = []Instance{
		{Name: "single", Namespace: "single", Observer: &fakeObserver{ephemeral: true}, Data: single},
		{Name: "pvc", Namespace: "pvc", Observer: &fakeObserver{volumes: true, waits: true}, Data: pvc},
		{Name: "pvc62", Namespace: "pvc62", Observer: &fakeObserver{volumes: true}, Data: pvc62},
		{Name: "empty", Namespace: "empty", Observer: &fakeObserver{}, Data: empty},
	}
	kube.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := action.(k8stesting.CreateAction).GetObject().(interface{ GetName() string })
		return true, nil, kube.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, action.GetNamespace(), ev.GetName())
	})
	l.act(context.Background(), 1, stepRand(1, 1), config.NodeDrain)
	if got := strings.Join(single.calls, ","); got != "begin,verify reset" {
		t.Errorf("single: %s", got)
	}
	if got := strings.Join(pvc.calls, ","); got != "begin,verify node_drain lossless" {
		t.Errorf("pvc: %s", got)
	}
	if got := strings.Join(pvc62.calls, ","); got != "begin,verify node_drain" {
		t.Errorf("pvc62: %s", got)
	}
	if got := strings.Join(empty.calls, ","); got != "begin,verify node_drain" {
		t.Errorf("empty: %s", got)
	}
}
