package chaos

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/saremox/redis-operator/test/soak/internal/config"
)

func operatorPod(name, uid, image string, isReady bool) corev1.Pod {
	status := corev1.ConditionFalse
	if isReady {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "redis-operator", UID: types.UID(uid),
			Labels: map[string]string{"app": "redis-operator"}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func lease(holder string, acquired time.Time) *coordinationv1.Lease {
	t := metav1.NewMicroTime(acquired)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "redis-failover-lease", Namespace: "redis-operator"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &t},
	}
}

func deployment(image string, replicas, updated, ready int32, observed bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "redis-operator", Namespace: "redis-operator", Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "redis-operator"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: replicas, UpdatedReplicas: updated,
			ReadyReplicas: ready, AvailableReplicas: ready},
	}
	if !observed {
		d.Status.ObservedGeneration = 1
	}
	return d
}

func TestRestarted(t *testing.T) {
	const image = "redis-operator:a"
	old := []types.UID{"old"}
	now := time.Now()
	cases := []struct {
		name string
		s    operatorState
		want string
	}{
		{"new leader", operatorState{
			pods:  []corev1.Pod{operatorPod("op-new", "new", image, true)},
			lease: lease("op-new_1234", now),
		}, ""},
		{"old leader still", operatorState{
			pods:  []corev1.Pod{operatorPod("op-old", "old", image, true), operatorPod("op-new", "new", image, true)},
			lease: lease("op-old_1234", now),
		}, "was there before the restart"},
		{"released, none leads", operatorState{
			pods:  []corev1.Pod{operatorPod("op-new", "new", image, true)},
			lease: lease("", now),
		}, "no operator holds the lease"},
		{"no lease", operatorState{pods: []corev1.Pod{operatorPod("op-new", "new", image, true)}}, "no operator holds the lease"},
		{"leader not ready", operatorState{
			pods:  []corev1.Pod{operatorPod("op-new", "new", image, false)},
			lease: lease("op-new_1234", now),
		}, "not ready"},
		{"leader gone", operatorState{
			pods:  []corev1.Pod{operatorPod("op-new", "new", image, true)},
			lease: lease("op-other_1234", now),
		}, "no operator pod"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := restarted(c.s, old)
			switch {
			case c.want == "" && (err != nil || p.Name != "op-new"):
				t.Errorf("leader %v, %v", p, err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("error %v, want %q", err, c.want)
			}
		})
	}
}

func TestUpgraded(t *testing.T) {
	const from, to = "redis-operator:a", "redis-operator:b"
	now := time.Now()
	done := operatorState{
		deployment: deployment(to, 1, 1, 1, true),
		pods:       []corev1.Pod{operatorPod("op-b", "b", to, true)},
		lease:      lease("op-b_1", now),
	}
	if p, err := upgraded(done, to); err != nil || p.Name != "op-b" {
		t.Fatalf("upgraded: %v, %v", p, err)
	}
	cases := map[string]struct {
		change func(*operatorState)
		want   string
	}{
		"not applied":  {func(s *operatorState) { s.deployment = deployment(from, 1, 1, 1, true) }, "the Deployment runs redis-operator:a"},
		"not observed": {func(s *operatorState) { s.deployment = deployment(to, 1, 1, 1, false) }, "isn't observed"},
		"rolling": {func(s *operatorState) {
			s.deployment = deployment(to, 2, 1, 2, true)
			s.pods = append(s.pods, operatorPod("op-a", "a", from, true))
		}, "2 replicas, 1 updated"},
		"old pod terminating": {func(s *operatorState) {
			s.pods = append(s.pods, operatorPod("op-a", "a", from, false))
		}, "op-a still runs redis-operator:a"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := done
			s.pods = append([]corev1.Pod{}, done.pods...)
			c.change(&s)
			_, err := upgraded(s, to)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("error %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("error %v, want %q", err, c.want)
			}
		})
	}
}

func TestDownSince(t *testing.T) {
	stopped := time.Now()
	if d := downSince(operatorState{lease: lease("op_1", stopped.Add(7*time.Second))}, stopped); d != 7*time.Second {
		t.Errorf("down %s", d)
	}
	// A new leader that took over before the old one was deleted.
	if d := downSince(operatorState{lease: lease("op_1", stopped.Add(-time.Second))}, stopped); d != 0 {
		t.Errorf("down %s", d)
	}
	if d := downSince(operatorState{}, stopped); d != 0 {
		t.Errorf("down %s without a lease", d)
	}
}

func TestHelmArgs(t *testing.T) {
	ch := config.Chaos{Timeout: metav1.Duration{Duration: 5 * time.Minute}, Upgrade: config.Upgrade{Set: []string{"crds.upgradeHook.enabled=true"}}}
	op := config.Operator{Namespace: "ops", Deployment: "redis-operator"}
	got := strings.Join(helmArgs(ch, op, config.OperatorVersion{Name: "rc2", Chart: "/etc/soak/redis-operator-rc2.tgz",
		Image: "localhost:5001/redis-operator:4.2.0-rc2"}), " ")
	want := "upgrade redis-operator /etc/soak/redis-operator-rc2.tgz --namespace ops --reset-then-reuse-values " +
		"--set image.repository=localhost:5001/redis-operator --set image.tag=4.2.0-rc2 --wait --timeout 5m0s " +
		"--set crds.upgradeHook.enabled=true"
	if got != want {
		t.Errorf("args\n%s\nwant\n%s", got, want)
	}
	got = strings.Join(helmArgs(ch, op, config.OperatorVersion{Name: "rc1", Chart: "oci://ghcr.io/saremox/redis-operator/charts/redis-operator",
		Version: "4.2.0-rc1", Image: "ghcr.io/saremox/redis-operator:4.2.0-rc1"}), " ")
	if !strings.Contains(got, " oci://ghcr.io/saremox/redis-operator/charts/redis-operator ") || !strings.Contains(got, "--version 4.2.0-rc1") ||
		!strings.Contains(got, "--set image.repository=ghcr.io/saremox/redis-operator --set image.tag=4.2.0-rc1") {
		t.Errorf("OCI args %s", got)
	}
}

func nodePod(namespace, name, node string, change func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name)},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if change != nil {
		change(&p)
	}
	return p
}

func TestEvictable(t *testing.T) {
	daemon := func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "kindnet"}}
	}
	pods := []corev1.Pod{
		nodePod("op", "rfr-op-0", "w1", nil),
		nodePod("kube-system", "kindnet-x", "w1", daemon),
		nodePod("kube-system", "kube-proxy-w1", "w1", func(p *corev1.Pod) {
			p.Annotations = map[string]string{mirrorAnnotation: "x"}
		}),
		nodePod("op", "job-done", "w1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		nodePod("op", "rfr-op-1", "w1", func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} }),
	}
	got := evictable(pods)
	if len(got) != 1 || got[0].Name != "rfr-op-0" {
		t.Errorf("evictable %v", got)
	}
	// A terminating pod is still on the node.
	err := drained("w1", pods)
	if err == nil || !strings.Contains(err.Error(), "2 pods left on w1: op/rfr-op-0, op/rfr-op-1") {
		t.Errorf("drained: %v", err)
	}
	if err := drained("w1", pods[1:4]); err != nil {
		t.Errorf("drained with only DaemonSet, static and finished pods: %v", err)
	}
	if err := drained("w2", pods); err != nil {
		t.Errorf("another node: %v", err)
	}
}

func TestDrainable(t *testing.T) {
	node := func(name string, ready, unschedulable bool) corev1.Node {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
		}
	}
	nodes := []corev1.Node{node("w3", true, false), node("w1", true, false), node("w2", true, true), node("w4", false, false), node("self", true, false)}
	if got := drainable(nodes, "self"); strings.Join(got, ",") != "w1,w3" {
		t.Errorf("drainable %v", got)
	}
}
