package service_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// ---------------------------------------------------------------------------
// EnsureSentinelDeployment / auto-provisioned ServiceAccount
//
// PR #43 originally built the ServiceAccount object in generateSentinelDeployment
// but never persisted it against the k8s API -- the Sentinel Deployment would
// reference a ServiceAccount name that was never actually created. These tests
// pin the fixed behavior: EnsureSentinelDeployment must create the ServiceAccount
// itself before/alongside the Deployment when the user hasn't set one, and must
// never touch it when the user already provided their own.
// ---------------------------------------------------------------------------

func TestEnsureSentinelDeploymentCreatesServiceAccountWhenUnset(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Sentinel.ServiceAccountName = ""

	ownerRefs := []metav1.OwnerReference{{Name: "owner"}}
	labels := map[string]string{"some": "label"}

	var gotSA *corev1.ServiceAccount
	var gotDeployment *appsv1.Deployment

	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil)
	ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotSA = args.Get(1).(*corev1.ServiceAccount)
	}).Return(nil)
	ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotDeployment = args.Get(1).(*appsv1.Deployment)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureSentinelDeployment(rf, labels, ownerRefs)

	assert.NoError(err)
	ms.AssertExpectations(t)

	if assert.NotNil(gotSA) {
		expectedName := rfservice.GetSentinelServiceAccountName(rf)
		assert.Equal(expectedName, gotSA.Name)
		assert.Equal(namespace, gotSA.Namespace)
		assert.Equal(ownerRefs, gotSA.OwnerReferences)

		if assert.NotNil(gotDeployment) {
			assert.Equal(expectedName, gotDeployment.Spec.Template.Spec.ServiceAccountName,
				"the Deployment must reference the exact ServiceAccount that was actually created")
		}
	}
}

func TestEnsureSentinelDeploymentDoesNotTouchExistingServiceAccount(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Sentinel.ServiceAccountName = "user-provided-sa"

	var gotDeployment *appsv1.Deployment

	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil)
	ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotDeployment = args.Get(1).(*appsv1.Deployment)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	ms.AssertNotCalled(t, "CreateOrUpdateServiceAccount", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "GetServiceAccount", mock.Anything, mock.Anything)

	if assert.NotNil(gotDeployment) {
		assert.Equal("user-provided-sa", gotDeployment.Spec.Template.Spec.ServiceAccountName)
	}
}

func TestEnsureSentinelDeploymentPropagatesServiceAccountCreationError(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Sentinel.ServiceAccountName = ""

	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil)
	ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(errors.New("service account creation failed"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

	assert.Error(err)
	// The Deployment must never be written once provisioning its own
	// ServiceAccount failed -- it would otherwise reference a ServiceAccount
	// that doesn't exist.
	ms.AssertNotCalled(t, "CreateOrUpdateDeployment", mock.Anything, mock.Anything)
}

// ---------------------------------------------------------------------------
// GetSentinelServiceAccountName naming convention
//
// PR #43 named the auto-provisioned ServiceAccount directly after the
// RedisFailover CR (rf.Name), which both collides with the CR's own name and
// doesn't match how every sibling resource is namespaced via a Get*Name
// helper built on generateName(typeName, rf.Name). This pins the fixed
// convention.
// ---------------------------------------------------------------------------

func TestGetSentinelServiceAccountNameFollowsNamingConvention(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	got := rfservice.GetSentinelServiceAccountName(rf)

	assert.Equal("rfs-sa-test", got)
	assert.NotEqual(rf.Name, got, "must not collide with the RedisFailover CR's own name")
	assert.NotEqual(rfservice.GetSentinelName(rf), got, "must not collide with the Sentinel Deployment/Service/ConfigMap name")
}
