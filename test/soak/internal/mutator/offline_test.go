package mutator

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
)

// Scenario C must start the operator again with the replicas it had, for
// example two with leader election.
func TestScenarioCRestoresReplicas(t *testing.T) {
	replicas := int32(2)
	op := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "redis-operator", Namespace: "redis-operator"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "redis-operator"}},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 2, UpdatedReplicas: 2},
	}
	in := config.Instance{Name: "a", Namespace: "ns"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "a-auth", Namespace: "ns"}}
	kube := fake.NewClientset(op, secret)
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}
	rf.Status.State, rf.Status.Message = redisfailoverv1.NotHealthyState, passwordNotApplied
	m := &Mutator{
		in:              in,
		operator:        config.Operator{Namespace: "redis-operator", Deployment: "redis-operator"},
		convergeTimeout: time.Second,
		kube:            kube,
		rfs:             rffake.NewSimpleClientset(rf),
		auth: auth.New(func(name string) (*corev1.Secret, error) {
			return kube.CoreV1().Secrets("ns").Get(context.Background(), name, metav1.GetOptions{})
		}),
		lock: &global.Lock{},
	}
	_ = m.rotateOffline(context.Background(), &offline{secret: "a-auth", previous: "p0", first: "p1", second: "p2"}, slog.New(slog.DiscardHandler))
	d, err := kube.AppsV1().Deployments("redis-operator").Get(context.Background(), "redis-operator", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *d.Spec.Replicas != replicas {
		t.Errorf("operator replicas = %d, want %d", *d.Spec.Replicas, replicas)
	}
}

// operatorDeployment adds a running operator Deployment with one replica to
// the rotation.
func (r *rotation) operatorDeployment(t *testing.T) {
	t.Helper()
	replicas := int32(1)
	op := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "redis-operator", Namespace: "redis-operator"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "redis-operator"}},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1, UpdatedReplicas: 1},
	}
	if _, err := r.kube.AppsV1().Deployments("redis-operator").Create(context.Background(), op, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.m.operator = config.Operator{Namespace: "redis-operator", Deployment: "redis-operator"}
}

// appliedPasswords adds the Secret in which the operator keeps the applied
// passwords of instance x.
func (r *rotation) appliedPasswords(t *testing.T) {
	t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rfr-auth-applied-x", Namespace: "ns"}}
	if _, err := r.kube.CoreV1().Secrets("ns").Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// The restarted operator applies the password with the passwords that it
// kept, so password_rotate_offline converges on the first rotation. The
// tester neither waits for passwordNotApplied nor deletes the kept passwords.
func TestScenarioCConvergesWithoutRestore(t *testing.T) {
	r, ctx := newRotation(t, redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState}, true)
	r.operatorDeployment(t)
	r.appliedPasswords(t)
	p := planAuth(config.PasswordRotateOffline, authState())
	if next := r.m.execute(ctx, 1, authState(), p, r.m.log); next != "" {
		t.Fatalf("next kind %q", next)
	}
	if len(r.windows) != 1 {
		t.Fatalf("%d windows, want 1", len(r.windows))
	}
	if got := r.password(); got != p.offline.first || got == "old" {
		t.Errorf("the Secret has %q, want the first rotation", got)
	}
	if _, err := r.kube.CoreV1().Secrets("ns").Get(context.Background(), "rfr-auth-applied-x", metav1.GetOptions{}); err != nil {
		t.Errorf("the applied passwords: %v", err)
	}
	if got := r.results(t, config.PasswordRotateOffline); got[resultConverged] != 1 {
		t.Errorf("results %v, want 1 converged", got)
	}
	var last string
	for line := range strings.SplitSeq(r.out.String(), "\n") {
		if strings.Contains(line, `"msg":"scenario phase done"`) {
			last = line
		}
	}
	if !strings.Contains(last, `"phase":4,`) || !strings.Contains(last, "expect the rotation to converge") {
		t.Errorf("the last phase log line is %q", last)
	}
}

// An operator that cannot apply the password after all gets the previous
// password back, as after a stuck password_rotate.
func TestScenarioCStuckRestored(t *testing.T) {
	r, ctx := newRotation(t, stuck, false, true)
	r.operatorDeployment(t)
	p := planAuth(config.PasswordRotateOffline, authState())
	if next := r.m.execute(ctx, 1, authState(), p, r.m.log); next != "" {
		t.Fatalf("next kind %q", next)
	}
	if len(r.windows) != 2 {
		t.Fatalf("%d windows, want 2", len(r.windows))
	}
	if got := r.password(); got != "old" {
		t.Errorf("the Secret has %q, want old", got)
	}
	if got := r.results(t, config.PasswordRotateOffline); got[resultStuckRestored] != 1 {
		t.Errorf("results %v, want 1 stuck_restored", got)
	}
}

// password_rotate_offline_forget deletes the kept passwords while the
// operator is stopped. The operator then cannot apply the password, and the
// tester puts the previous password back and rotates again.
func TestScenarioCForgetDeletesAppliedPasswords(t *testing.T) {
	for name, kept := range map[string]bool{"kept passwords": true, "no kept passwords": false} {
		t.Run(name, func(t *testing.T) {
			r, ctx := newRotation(t, stuck)
			r.operatorDeployment(t)
			r.m.convergeTimeout = time.Second
			if kept {
				r.appliedPasswords(t)
			}
			// The fake holds its lock in a reactor, so the reactors only
			// record the order of the scales and the delete.
			var order []string
			r.kube.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				order = append(order, string(a.(k8stesting.PatchAction).GetPatch()))
				return false, nil, nil
			})
			r.kube.PrependReactor("delete", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
				order = append(order, "delete "+a.(k8stesting.DeleteAction).GetName())
				return false, nil, nil
			})
			o := &offline{secret: "x-auth", previous: "old", first: "p1", second: "p2", forget: true}
			err := r.m.rotateOffline(ctx, o, r.m.log)
			// The fake has no pods, so the instance never reads Healthy.
			if _, ok := errors.AsType[expectation](err); !ok {
				t.Errorf("error %v, want an expectation", err)
			}
			want := []string{`{"spec":{"replicas":0}}`, "delete rfr-auth-applied-x", `{"spec":{"replicas":1}}`}
			if !slices.Equal(order, want) {
				t.Errorf("order %q, want %q", order, want)
			}
			if _, err := r.kube.CoreV1().Secrets("ns").Get(context.Background(), "rfr-auth-applied-x", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Errorf("the applied passwords: %v", err)
			}
			if got := r.password(); got != "p2" {
				t.Errorf("the Secret has %q, want p2", got)
			}
			if o.phases != 8 {
				t.Errorf("%d phases, want 8", o.phases)
			}
			if line := r.logged("scenario phase failed"); !strings.Contains(line, `"phase_name":"expect Healthy"`) {
				t.Errorf("the failed phase log line is %q", line)
			}
		})
	}
}
