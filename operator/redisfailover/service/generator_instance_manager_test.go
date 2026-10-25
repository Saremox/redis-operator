package service_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

const (
	operatorImageVar   = "OPERATOR_IMAGE_NAME"
	operatorImage      = "ghcr.io/saremox/redis-operator:4.2.0"
	instanceManagerBin = "/controller/redis-instance"
)

// withInstanceManager turns the manager on and gives the operator its own
// image, as the operator Deployment does. The field is false by default, so
// the RedisFailover that redisStatefulSetFor builds runs redis-server
// straight from the Redis image.
func withInstanceManager(t *testing.T) func(*redisfailoverv1.RedisFailover) {
	t.Helper()
	t.Setenv(operatorImageVar, operatorImage)
	return func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.InstanceManager.Enabled = true
		rf.Spec.Redis.Port = 6379
	}
}

// managedStatefulSetFor builds the StatefulSet of a RedisFailover with the
// manager on, after mutate.
func managedStatefulSetFor(t *testing.T, mutate func(*redisfailoverv1.RedisFailover)) *appsv1.StatefulSet {
	t.Helper()
	on := withInstanceManager(t)
	return redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		on(rf)
		if mutate != nil {
			mutate(rf)
		}
	})
}

// The field is the switch. Without it nothing below changes.
func TestTheInstanceManagerIsOffByDefault(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	assert.Nil(t, findInitContainer(ss, "instance-manager-init"),
		"the manager must be opt-in")
	assert.NotNil(t, findInitContainer(ss, "rdb-tempfile-cleanup"),
		"without the manager the cleanup container must stay")
}

// The init container copies the binary out of the operator image. The
// operator writes the flags of the binary, so the operator also chooses the
// build that reads them.
func TestTheInitContainerCopiesTheBinaryOutOfTheOperatorImage(t *testing.T) {
	ss := managedStatefulSetFor(t, nil)

	c := findInitContainer(ss, "instance-manager-init")
	require.NotNil(t, c, "expected an instance-manager-init container")
	assert.Equal(t, operatorImage, c.Image)
	assert.Equal(t,
		[]string{"/usr/local/bin/redis-instance", "bootstrap", instanceManagerBin},
		c.Command)

	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/controller" {
			mounted = true
		}
	}
	assert.True(t, mounted, "the binary needs the shared volume to copy itself onto")
}

// The operator cannot guess its own image, and a pod without the binary
// stays in Init:CrashLoopBackOff. The reconcile must fail instead.
func TestTheManagerNeedsTheOperatorImageVariable(t *testing.T) {
	t.Setenv(operatorImageVar, "")
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{Replicas: 3, Port: 6379},
		},
	}
	rf.Spec.Redis.InstanceManager.Enabled = true

	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", "testns", mock.Anything).Maybe().Return(nil, nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)

	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	require.ErrorContains(t, err, operatorImageVar)
	ms.AssertNotCalled(t, "CreateOrUpdateStatefulSet", mock.Anything, mock.Anything)
}

// The cleanup container keeps its own job: it removes temp-*.rdb before the
// user init containers run, as it does without the manager.
func TestTheCleanupContainerStaysWithTheManager(t *testing.T) {
	ss := managedStatefulSetFor(t, nil)

	assert.NotNil(t, findInitContainer(ss, "rdb-tempfile-cleanup"),
		"the cleanup must keep its place before the user init containers")
}

// A user init container still runs after ours, as it does without the
// manager.
func TestTheInitContainersRunBeforeUserInitContainers(t *testing.T) {
	ss := managedStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.InitContainers = []corev1.Container{{Name: "user-init", Image: "busybox"}}
	})

	names := make([]string, 0, len(ss.Spec.Template.Spec.InitContainers))
	for _, c := range ss.Spec.Template.Spec.InitContainers {
		names = append(names, c.Name)
	}

	assert.Equal(t, []string{"rdb-tempfile-cleanup", "instance-manager-init", "user-init"}, names)
}

// The init container honours spec.redis.containerSecurityContext, as the
// cleanup container beside it does. The copy goes to the mounted volume and
// not to the root filesystem, so readOnlyRootFilesystem holds.
func TestTheInitContainerTakesTheContainerSecurityContext(t *testing.T) {
	ss := managedStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
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

// The Redis container runs the copy, not redis-server. The flags carry what
// ready.sh and the probes of the pod hold without the manager.
func TestTheRedisContainerRunsTheManager(t *testing.T) {
	ss := managedStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.Port = 7000
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	c := ss.Spec.Template.Spec.Containers[0]
	assert.Equal(t, []string{
		instanceManagerBin, "run",
		"--redis-conf", "/redis/redis.conf",
		"--redis-port", "7000",
		"--health-port", "8080",
		"--max-link-down", "70s",
	}, c.Command)
	assert.NotContains(t, strings.Join(c.Command, " "), "redis-server",
		"the manager starts redis-server itself")
}

// Sentinel gives the replicas a longer window, because the failover takes
// down-after-milliseconds plus failover-timeout.
func TestTheLinkDownWindowFollowsTheFailoverMode(t *testing.T) {
	ss := managedStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Sentinel.Enabled = boolPtr(true)
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	assert.Contains(t, ss.Spec.Template.Spec.Containers[0].Command, "75s")
}

// It can only run the binary if it mounts the volume the init container
// wrote.
func TestTheRedisContainerMountsTheSharedVolume(t *testing.T) {
	ss := managedStatefulSetFor(t, nil)

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
// An exec probe can fail to fork on a node that is short of memory while
// Redis is healthy; an HTTP probe cannot.
func TestTheProbesBecomeHTTPProbes(t *testing.T) {
	ss := managedStatefulSetFor(t, nil)

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

// With hostNetwork the pod shares the ports of the node, so 8080 can be
// taken. The port follows the field, as the exporter port does.
func TestTheHealthPortFollowsTheField(t *testing.T) {
	ss := managedStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.InstanceManager.Port = 9999
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	c := ss.Spec.Template.Spec.Containers[0]
	assert.Equal(t, int32(9999), c.LivenessProbe.HTTPGet.Port.IntVal)
	assert.Equal(t, int32(9999), c.ReadinessProbe.HTTPGet.Port.IntVal)
	assert.Contains(t, c.Command, "9999")

	var port bool
	for _, p := range c.Ports {
		if p.ContainerPort == 9999 {
			port = true
		}
	}
	assert.True(t, port)
}

// A custom probe is the user's own choice and still wins.
func TestACustomProbeBeatsTheManagerProbe(t *testing.T) {
	custom := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"mine"}},
		},
	}
	ss := managedStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
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
	ss := managedStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.ImagePullPolicy = corev1.PullIfNotPresent
	})

	c := findInitContainer(ss, "instance-manager-init")
	require.NotNil(t, c)
	assert.Equal(t, corev1.PullIfNotPresent, c.ImagePullPolicy)
}

func boolPtr(b bool) *bool { return &b }
