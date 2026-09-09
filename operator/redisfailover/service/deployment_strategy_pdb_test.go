package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestSentinelDeploymentStrategy(t *testing.T) {
	assert := assert.New(t)

	maxSurge := intstr.FromInt(1)
	maxUnavailable := intstr.FromInt(0)
	strategy := appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
	}

	rf := generateRF()
	rf.Spec.Sentinel.Strategy = strategy

	var gotStrategy appsv1.DeploymentStrategy
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
	ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotStrategy = args.Get(1).(*appsv1.Deployment).Spec.Strategy
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	assert.Equal(strategy, gotStrategy)
}

func TestPodDisruptionBudgetMinAvailableOverride(t *testing.T) {
	tests := []struct {
		name        string
		override    *intstr.IntOrString
		redisReplic int32
		expected    intstr.IntOrString
	}{
		{
			name:        "default with >2 replicas is 2",
			override:    nil,
			redisReplic: 3,
			expected:    intstr.FromInt(2),
		},
		{
			name:        "default with <=2 replicas is 1",
			override:    nil,
			redisReplic: 2,
			expected:    intstr.FromInt(1),
		},
		{
			name:        "explicit override wins",
			override:    ptrIOS(intstr.FromString("60%")),
			redisReplic: 3,
			expected:    intstr.FromString("60%"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF()
			rf.Spec.Redis.Replicas = test.redisReplic
			rf.Spec.Redis.PodDisruptionBudgetMinAvailable = test.override

			var gotMinAvailable *intstr.IntOrString
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotMinAvailable = args.Get(1).(*policyv1.PodDisruptionBudget).Spec.MinAvailable
			}).Return(nil)
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

			assert.NoError(err)
			assert.NotNil(gotMinAvailable)
			assert.Equal(test.expected, *gotMinAvailable)
		})
	}
}

func ptrIOS(v intstr.IntOrString) *intstr.IntOrString { return &v }
