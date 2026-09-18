package service_test

import (
	"strings"
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

func TestRedisStatefulSetHasRDBTempfileCleanupInitContainer(t *testing.T) {
	assert := assert.New(t)

	ss := redisStatefulSetFor(t, nil)
	if !assert.NotNil(ss) {
		return
	}

	c := findInitContainer(ss, "rdb-tempfile-cleanup")
	if !assert.NotNil(c, "expected an rdb-tempfile-cleanup init container") {
		return
	}

	// Only temp-<pid>.rdb is removed. The live dump must never match, whatever
	// dbfilename is set to.
	joined := strings.Join(c.Command, " ")
	assert.Contains(joined, "temp-*.rdb")
	assert.NotContains(joined, "dump.rdb")
	assert.Contains(joined, "-maxdepth 1", "must not recurse out of the data directory")

	// It can only clean what it can see.
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/data" {
			mounted = true
		}
	}
	assert.True(mounted, "cleanup container must mount the redis data volume at /data")
}

func TestRDBTempfileCleanupRunsBeforeUserInitContainers(t *testing.T) {
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

	if assert.Len(names, 2) {
		assert.Equal("rdb-tempfile-cleanup", names[0], "cleanup must run first")
		assert.Equal("user-init", names[1])
	}
}

func TestRDBTempfileCleanupUsesTheRedisImage(t *testing.T) {
	assert := assert.New(t)

	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.Image = "redis:7.2.12-alpine"
	})
	if !assert.NotNil(ss) {
		return
	}

	c := findInitContainer(ss, "rdb-tempfile-cleanup")
	if assert.NotNil(c) {
		assert.Equal("redis:7.2.12-alpine", c.Image, "reuse the redis image rather than pulling another one")
	}
}
