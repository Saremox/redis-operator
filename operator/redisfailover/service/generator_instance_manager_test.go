package service_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

const instanceManagerImage = "ghcr.io/saremox/redis-operator:4.2.0"

// withInstanceManager turns the manager on. spec.redis.instanceManagerImage is
// empty by default, so the RedisFailover that redisStatefulSetFor builds runs
// redis-server straight from the Redis image.
func withInstanceManager(rf *redisfailoverv1.RedisFailover) {
	rf.Spec.Redis.InstanceManagerImage = instanceManagerImage
}

// The field is the switch. Without it nothing below changes.
func TestTheInstanceManagerIsOffByDefault(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	assert.Nil(t, findInitContainer(ss, "instance-manager-init"),
		"the manager must be opt-in")
	assert.NotNil(t, findInitContainer(ss, "rdb-tempfile-cleanup"),
		"without the manager the cleanup container must stay")
}

// The init container copies the binary out of the image that the field names.
func TestTheInitContainerCopiesTheBinary(t *testing.T) {
	ss := redisStatefulSetFor(t, withInstanceManager)

	c := findInitContainer(ss, "instance-manager-init")
	require.NotNil(t, c, "expected an instance-manager-init container")
	assert.Equal(t, instanceManagerImage, c.Image)
	assert.Equal(t,
		[]string{"/usr/local/bin/redis-instance", "bootstrap", "/controller/redis-instance"},
		c.Command)

	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/controller" {
			mounted = true
		}
	}
	assert.True(t, mounted, "the binary needs the shared volume to copy itself onto")
}

// The manager removes the RDB tempfiles before Redis starts, so the container
// that does only that has no work left.
func TestTheManagerReplacesTheCleanupContainer(t *testing.T) {
	ss := redisStatefulSetFor(t, withInstanceManager)

	assert.Nil(t, findInitContainer(ss, "rdb-tempfile-cleanup"),
		"the manager already removes the tempfiles")
}

// A user init container still runs after ours, as it does without the manager.
func TestTheInitContainerRunsBeforeUserInitContainers(t *testing.T) {
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		withInstanceManager(rf)
		rf.Spec.Redis.InitContainers = []corev1.Container{{Name: "user-init", Image: "busybox"}}
	})

	names := make([]string, 0, len(ss.Spec.Template.Spec.InitContainers))
	for _, c := range ss.Spec.Template.Spec.InitContainers {
		names = append(names, c.Name)
	}

	require.Len(t, names, 2)
	assert.Equal(t, "instance-manager-init", names[0])
	assert.Equal(t, "user-init", names[1])
}

// The init container honours spec.redis.containerSecurityContext in the same
// way as the container it replaces. The copy goes to the mounted volume and
// not to the root filesystem, so readOnlyRootFilesystem holds.
func TestTheInitContainerTakesTheContainerSecurityContext(t *testing.T) {
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		withInstanceManager(rf)
		rf.Spec.Redis.ContainerSecurityContext = &corev1.SecurityContext{
			ReadOnlyRootFilesystem: boolPtr(true),
		}
	})

	c := findInitContainer(ss, "instance-manager-init")
	require.NotNil(t, c)
	require.NotNil(t, c.SecurityContext)
	require.NotNil(t, c.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
}

// The Redis container runs the copy, not redis-server.
func TestTheRedisContainerRunsTheManager(t *testing.T) {
	ss := redisStatefulSetFor(t, withInstanceManager)

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	c := ss.Spec.Template.Spec.Containers[0]
	assert.Equal(t,
		[]string{"/controller/redis-instance", "run", "--redis-conf", "/redis/redis.conf"},
		c.Command)
	assert.NotContains(t, strings.Join(c.Command, " "), "redis-server",
		"the manager starts redis-server itself")
}

// It can only run the binary if it mounts the volume the init container wrote.
func TestTheRedisContainerMountsTheSharedVolume(t *testing.T) {
	ss := redisStatefulSetFor(t, withInstanceManager)

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	var mounted bool
	for _, m := range ss.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/controller" {
			mounted = true
		}
	}
	assert.True(t, mounted)

	var declared bool
	for _, v := range ss.Spec.Template.Spec.Volumes {
		if v.Name == "instance-manager" {
			declared = true
			assert.NotNil(t, v.EmptyDir, "the binary needs no storage that outlives the pod")
		}
	}
	assert.True(t, declared)
}

// The manager serves the health endpoints, so the probes become HTTP probes.
// An exec probe can fail to fork on a node that is short of memory while Redis
// is healthy; an HTTP probe cannot.
func TestTheProbesBecomeHTTPProbes(t *testing.T) {
	ss := redisStatefulSetFor(t, withInstanceManager)

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	c := ss.Spec.Template.Spec.Containers[0]

	require.NotNil(t, c.LivenessProbe)
	require.NotNil(t, c.LivenessProbe.HTTPGet, "liveness must not start a process")
	assert.Equal(t, "/healthz", c.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, int32(8080), c.LivenessProbe.HTTPGet.Port.IntVal)

	require.NotNil(t, c.ReadinessProbe)
	require.NotNil(t, c.ReadinessProbe.HTTPGet, "readiness must not start a process")
	assert.Equal(t, "/readyz", c.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, int32(8080), c.ReadinessProbe.HTTPGet.Port.IntVal)

	var port bool
	for _, p := range c.Ports {
		if p.ContainerPort == 8080 {
			port = true
		}
	}
	assert.True(t, port, "the health port must be declared")
}

// A custom probe is the user's own choice and still wins.
func TestACustomProbeBeatsTheManagerProbe(t *testing.T) {
	custom := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"mine"}},
		},
	}
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		withInstanceManager(rf)
		rf.Spec.Redis.CustomLivenessProbe = custom
		rf.Spec.Redis.CustomReadinessProbe = custom
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	c := ss.Spec.Template.Spec.Containers[0]
	require.NotNil(t, c.LivenessProbe)
	assert.Equal(t, custom, c.LivenessProbe)
	require.NotNil(t, c.ReadinessProbe)
	assert.Equal(t, custom, c.ReadinessProbe)
}

// The init container pulls the operator image, and spec.redis.imagePullPolicy
// is the only policy the RedisFailover has.
func TestTheInitContainerTakesThePullPolicy(t *testing.T) {
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		withInstanceManager(rf)
		rf.Spec.Redis.ImagePullPolicy = corev1.PullIfNotPresent
	})

	c := findInitContainer(ss, "instance-manager-init")
	require.NotNil(t, c)
	assert.Equal(t, corev1.PullIfNotPresent, c.ImagePullPolicy)
}

func boolPtr(b bool) *bool { return &b }
