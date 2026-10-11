package operator

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/saremox/redis-operator:4.2.0-rc1":         "4.2.0-rc1",
		"redis-operator:dev":                               "dev",
		"localhost:5001/redis-operator":                    "latest",
		"localhost:5001/redis-operator:pr":                 "pr",
		"ghcr.io/saremox/redis-operator:4.2.0@sha256:abcd": "4.2.0",
	}
	for image, want := range cases {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestVersion(t *testing.T) {
	kube := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "redis-operator", Namespace: "ops"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "ghcr.io/saremox/redis-operator:4.2.0-rc1"}},
		}}},
	})
	v, err := Version(context.Background(), kube, "ops", "redis-operator")
	if err != nil {
		t.Fatal(err)
	}
	if v != "4.2.0-rc1" {
		t.Errorf("got %q", v)
	}
}
