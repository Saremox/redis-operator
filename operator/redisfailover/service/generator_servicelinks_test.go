package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// Kubernetes injects one pair of environment variables per Service in the
// namespace when enableServiceLinks is left at its default of true. In a
// namespace running many RedisFailovers that is thousands of variables in
// every pod, which slows container start and has hit the environment size
// limit. Redis reaches its peers through DNS, so the links are never used.
func TestRedisStatefulSetDisablesServiceLinks(t *testing.T) {
	assert := assert.New(t)

	var gotSS *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotSS = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(generateRF(), nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotSS) {
		links := gotSS.Spec.Template.Spec.EnableServiceLinks
		if assert.NotNil(links, "enableServiceLinks should be set explicitly, not left to the default") {
			assert.False(*links)
		}
	}
}

func TestSentinelDeploymentDisablesServiceLinks(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	sentinelOn := true
	rf.Spec.Sentinel.Enabled = &sentinelOn

	var gotD *appsv1.Deployment
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Maybe().Return(nil)
	ms.On("CreateOrUpdateRole", namespace, mock.Anything).Maybe().Return(nil)
	ms.On("CreateOrUpdateRoleBinding", namespace, mock.Anything).Maybe().Return(nil)
	ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotD = args.Get(1).(*appsv1.Deployment)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotD) {
		links := gotD.Spec.Template.Spec.EnableServiceLinks
		if assert.NotNil(links, "enableServiceLinks should be set explicitly, not left to the default") {
			assert.False(*links)
		}
	}
}
