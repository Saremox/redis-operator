package mutator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
