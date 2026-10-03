package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalizePodSpecForComparison(t *testing.T) {
	assert := assert.New(t)

	spec := &corev1.PodSpec{
		RestartPolicy:            corev1.RestartPolicyAlways,
		SchedulerName:            "default-scheduler",
		DeprecatedServiceAccount: "sa",
		Containers: []corev1.Container{
			{Name: "a", TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile},
		},
		InitContainers: []corev1.Container{
			{Name: "init", TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile},
		},
	}

	normalizePodSpecForComparison(spec)

	assert.Equal(corev1.RestartPolicy(""), spec.RestartPolicy)
	assert.Empty(spec.SchedulerName)
	assert.Empty(spec.DeprecatedServiceAccount)
	assert.Empty(spec.Containers[0].TerminationMessagePath)
	assert.Empty(spec.Containers[0].TerminationMessagePolicy)
	assert.Empty(spec.InitContainers[0].TerminationMessagePath)
	assert.Empty(spec.InitContainers[0].TerminationMessagePolicy)
	// The container's identity must be untouched.
	assert.Equal("a", spec.Containers[0].Name)
}

// A child orphaned by `kubectl delete --cascade=orphan` has no owner. The
// RedisFailover created again must adopt it, so a missing or different
// owner reference makes the object out of date.
func TestUpToDateComparesOwnerReferences(t *testing.T) {
	owner := []metav1.OwnerReference{{APIVersion: "databases.spotahome.com/v1", Kind: "RedisFailover", Name: "test", UID: "new-uid"}}
	meta := func(refs []metav1.OwnerReference) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: "rfr-test", Namespace: "testns", OwnerReferences: refs}
	}

	tests := []struct {
		name     string
		upToDate func(stored, desired []metav1.OwnerReference) bool
	}{
		{"StatefulSet", func(stored, desired []metav1.OwnerReference) bool {
			return statefulSetUpToDate(&appsv1.StatefulSet{ObjectMeta: meta(stored)}, &appsv1.StatefulSet{ObjectMeta: meta(desired)})
		}},
		{"Deployment", func(stored, desired []metav1.OwnerReference) bool {
			return deploymentUpToDate(&appsv1.Deployment{ObjectMeta: meta(stored)}, &appsv1.Deployment{ObjectMeta: meta(desired)})
		}},
		{"Service", func(stored, desired []metav1.OwnerReference) bool {
			return serviceUpToDate(&corev1.Service{ObjectMeta: meta(stored)}, &corev1.Service{ObjectMeta: meta(desired)})
		}},
		{"ConfigMap", func(stored, desired []metav1.OwnerReference) bool {
			return configMapUpToDate(&corev1.ConfigMap{ObjectMeta: meta(stored)}, &corev1.ConfigMap{ObjectMeta: meta(desired)})
		}},
		{"PodDisruptionBudget", func(stored, desired []metav1.OwnerReference) bool {
			return podDisruptionBudgetUpToDate(&policyv1.PodDisruptionBudget{ObjectMeta: meta(stored)}, &policyv1.PodDisruptionBudget{ObjectMeta: meta(desired)})
		}},
		{"ServiceAccount", func(stored, desired []metav1.OwnerReference) bool {
			return serviceAccountUpToDate(&corev1.ServiceAccount{ObjectMeta: meta(stored)}, &corev1.ServiceAccount{ObjectMeta: meta(desired)})
		}},
	}

	oldOwner := []metav1.OwnerReference{{APIVersion: "databases.spotahome.com/v1", Kind: "RedisFailover", Name: "test", UID: "old-uid"}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			assert.True(test.upToDate(owner, owner), "same owner")
			assert.False(test.upToDate(nil, owner), "orphaned child")
			assert.False(test.upToDate(oldOwner, owner), "owner with a different UID")
		})
	}
}
