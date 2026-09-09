package service_test

import (
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

// lastValueOf returns the value of the last env var with the given name, which
// is the one that takes effect under Kubernetes' last-wins semantics.
func lastValueOf(env []corev1.EnvVar, name string) (string, bool) {
	value, found := "", false
	for _, e := range env {
		if e.Name == name {
			value, found = e.Value, true
		}
	}
	return value, found
}

func hasEnv(env []corev1.EnvVar, name, value string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == value {
			return true
		}
	}
	return false
}

func TestRedisMainContainerEnv(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Redis.Port = 6379
	rf.Spec.Redis.Env = []corev1.EnvVar{
		{Name: "MY_CUSTOM", Value: "hello"},
		// Duplicates an operator-injected var; the operator value must still win.
		{Name: "REDIS_PORT", Value: "1"},
	}

	var got []corev1.EnvVar
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		got = args.Get(1).(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0].Env
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	assert.True(hasEnv(got, "MY_CUSTOM", "hello"), "custom user env must be injected")
	// Operator REDIS_PORT (6379) is appended after the user's, so it wins.
	port, found := lastValueOf(got, "REDIS_PORT")
	assert.True(found)
	assert.Equal("6379", port, "operator-injected env must take precedence over a user duplicate")
}

func TestSentinelMainContainerEnv(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.Env = []corev1.EnvVar{
		{Name: "MY_CUSTOM", Value: "world"},
	}

	var got []corev1.EnvVar
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
	ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		got = args.Get(1).(*appsv1.Deployment).Spec.Template.Spec.Containers[0].Env
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	assert.True(hasEnv(got, "MY_CUSTOM", "world"), "custom user env must be injected into sentinel")
}
