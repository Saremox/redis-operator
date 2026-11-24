package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func redisStatefulSetFor(t *testing.T, mutate func(*redisfailoverv1.RedisFailover)) *appsv1.StatefulSet {
	t.Helper()

	rf := generateRF()
	if mutate != nil {
		mutate(rf)
	}

	var gotSS *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotSS = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	assert.NoError(t, client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{}))

	return gotSS
}

func findInitContainer(ss *appsv1.StatefulSet, name string) *corev1.Container {
	for i, c := range ss.Spec.Template.Spec.InitContainers {
		if c.Name == name {
			return &ss.Spec.Template.Spec.InitContainers[i]
		}
	}
	return nil
}

// This fork's instance manager cleans the data directory before it starts
// redis, so upstream's shell cleanup container is not added alongside it.
func TestInstanceManagerReplacesRDBTempfileCleanupContainer(t *testing.T) {
	assert := assert.New(t)

	ss := redisStatefulSetFor(t, nil)
	if !assert.NotNil(ss) {
		return
	}

	assert.Nil(findInitContainer(ss, "rdb-tempfile-cleanup"),
		"the shell cleanup container must not run alongside the instance manager")
	assert.NotNil(findInitContainer(ss, "instance-manager-init"),
		"the instance manager must be installed, since it does the cleanup")
}

func TestInstanceManagerInitRunsBeforeUserInitContainers(t *testing.T) {
	assert := assert.New(t)

	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.InitContainers = []corev1.Container{{Name: "user-init", Image: "busybox"}}
	})
	if !assert.NotNil(ss) {
		return
	}

	names := make([]string, 0, len(ss.Spec.Template.Spec.InitContainers))
	for _, c := range ss.Spec.Template.Spec.InitContainers {
		names = append(names, c.Name)
	}
	assert.Equal([]string{"instance-manager-init", "user-init"}, names)
}
