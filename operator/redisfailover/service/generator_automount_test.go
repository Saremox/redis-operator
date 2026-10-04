package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// The generated containers do not use the Kubernetes API, so the pod gets no
// token unless the user chooses a service account.
func TestRedisStatefulSetAutomountServiceAccountToken(t *testing.T) {
	for name, serviceAccountName := range map[string]string{"no service account": "", "user service account": "custom"} {
		t.Run(name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Redis.ServiceAccountName = serviceAccountName

			var gotSS *appsv1.StatefulSet
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotSS = args.Get(1).(*appsv1.StatefulSet)
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			assert.NoError(t, client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{}))

			if assert.NotNil(t, gotSS) {
				assertAutomount(t, serviceAccountName, gotSS.Spec.Template.Spec.AutomountServiceAccountToken)
			}
		})
	}
}

func TestSentinelDeploymentAutomountServiceAccountToken(t *testing.T) {
	for name, serviceAccountName := range map[string]string{"no service account": "", "user service account": "custom"} {
		t.Run(name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Sentinel.Enabled = ptr.To(true)
			rf.Spec.Sentinel.ServiceAccountName = serviceAccountName

			var gotD *appsv1.Deployment
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Maybe().Return(nil)
			ms.On("GetServiceAccount", namespace, mock.Anything).Maybe().Return(nil, apierrors.NewNotFound(corev1.Resource("serviceaccounts"), "sa"))
			ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotD = args.Get(1).(*appsv1.Deployment)
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			assert.NoError(t, client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{}))

			if assert.NotNil(t, gotD) {
				assertAutomount(t, serviceAccountName, gotD.Spec.Template.Spec.AutomountServiceAccountToken)
			}
		})
	}
}

func assertAutomount(t *testing.T, serviceAccountName string, got *bool) {
	t.Helper()
	if serviceAccountName != "" {
		assert.Nil(t, got, "the user service account keeps the cluster default")
		return
	}
	if assert.NotNil(t, got) {
		assert.False(t, *got)
	}
}
