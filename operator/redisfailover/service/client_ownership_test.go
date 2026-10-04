package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// generateOwnedRF returns a RedisFailover with a UID. ownedMeta returns the
// metadata of an object that this RedisFailover controls.
func generateOwnedRF() *redisfailoverv1.RedisFailover {
	rf := generateRF()
	rf.UID = "uid-rf"
	return rf
}

func ownedMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{OwnerReferences: ownerRefsOf(generateOwnedRF())}
}

func ownerRefsOf(rf *redisfailoverv1.RedisFailover) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(rf, redisfailoverv1.VersionKind(redisfailoverv1.RFKind))}
}

// ownershipVariants are the owner references of a stored object: the
// RedisFailover that runs Ensure, another RedisFailover, and no owner.
func ownershipVariants() (own, foreign, none []metav1.OwnerReference, rf *redisfailoverv1.RedisFailover) {
	rf = generateOwnedRF()
	other := generateRF()
	other.Name = "other"
	other.UID = "uid-other"
	return ownerRefsOf(rf), ownerRefsOf(other), nil, rf
}

func TestEnsureNotPresentRedisServiceDeletesOnlyAnOwnedService(t *testing.T) {
	own, foreign, none, rf := ownershipVariants()
	tests := []struct {
		name       string
		refs       []metav1.OwnerReference
		wantDelete bool
	}{
		{"owned", own, true},
		{"controlled by another RedisFailover", foreign, false},
		{"without owner", none, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svcName := rfservice.GetRedisName(rf)
			ms := &mK8SService.Services{}
			ms.On("GetService", namespace, svcName).Once().Return(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, OwnerReferences: test.refs}}, nil)
			if test.wantDelete {
				ms.On("DeleteService", namespace, svcName).Once().Return(nil)
			}

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)

			assert.NoError(t, client.EnsureNotPresentRedisService(rf))
			ms.AssertExpectations(t)
			if !test.wantDelete {
				ms.AssertNotCalled(t, "DeleteService", namespace, svcName)
			}
		})
	}
}

func TestEnsureNotPresentSentinelResourcesDeletesOnlyOwnedObjects(t *testing.T) {
	own, foreign, none, rf := ownershipVariants()
	name := rfservice.GetSentinelName(rf)
	saName := rfservice.GetSentinelServiceAccountName(rf)
	pdbName := name

	// Each object kind has one delete that is expected when the object is owned.
	kinds := []string{"Deployment", "Service", "ConfigMap", "PodDisruptionBudget", "ServiceAccount"}

	run := func(t *testing.T, refsOf func(kind string) []metav1.OwnerReference, wantDelete map[string]bool) {
		ms := &mK8SService.Services{}
		meta := func(n, kind string) metav1.ObjectMeta {
			return metav1.ObjectMeta{Name: n, OwnerReferences: refsOf(kind)}
		}
		ms.On("GetDeployment", namespace, name).Once().Return(&appsv1.Deployment{ObjectMeta: meta(name, "Deployment")}, nil)
		ms.On("GetService", namespace, name).Once().Return(&corev1.Service{ObjectMeta: meta(name, "Service")}, nil)
		ms.On("GetConfigMap", namespace, name).Once().Return(&corev1.ConfigMap{ObjectMeta: meta(name, "ConfigMap")}, nil)
		ms.On("GetPodDisruptionBudget", namespace, pdbName).Once().Return(&policyv1.PodDisruptionBudget{ObjectMeta: meta(pdbName, "PodDisruptionBudget")}, nil)
		ms.On("GetServiceAccount", namespace, saName).Once().Return(&corev1.ServiceAccount{ObjectMeta: meta(saName, "ServiceAccount")}, nil)
		// A delete without an expectation makes the mock panic and fails the test.
		if wantDelete["Deployment"] {
			ms.On("DeleteDeployment", namespace, name).Once().Return(nil)
		}
		if wantDelete["Service"] {
			ms.On("DeleteService", namespace, name).Once().Return(nil)
		}
		if wantDelete["ConfigMap"] {
			ms.On("DeleteConfigMap", namespace, name).Once().Return(nil)
		}
		if wantDelete["PodDisruptionBudget"] {
			ms.On("DeletePodDisruptionBudget", namespace, pdbName).Once().Return(nil)
		}
		if wantDelete["ServiceAccount"] {
			ms.On("DeleteServiceAccount", namespace, saName).Once().Return(nil)
		}

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)

		assert.NoError(t, client.EnsureNotPresentSentinelResources(rf))
		ms.AssertExpectations(t)
	}

	for _, kind := range kinds {
		t.Run("only the "+kind+" is owned", func(t *testing.T) {
			run(t, func(k string) []metav1.OwnerReference {
				if k == kind {
					return own
				}
				return foreign
			}, map[string]bool{kind: true})
		})
	}
	t.Run("all objects are owned", func(t *testing.T) {
		run(t, func(string) []metav1.OwnerReference { return own }, map[string]bool{
			"Deployment": true, "Service": true, "ConfigMap": true, "PodDisruptionBudget": true, "ServiceAccount": true,
		})
	})
	t.Run("all objects belong to another RedisFailover", func(t *testing.T) {
		run(t, func(string) []metav1.OwnerReference { return foreign }, nil)
	})
	t.Run("no object has an owner", func(t *testing.T) {
		run(t, func(string) []metav1.OwnerReference { return none }, nil)
	})
}
