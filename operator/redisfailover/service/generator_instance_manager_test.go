package service_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// The init container replaces upstream's rdb-tempfile-cleanup container, so
// it must keep the hardening that the replaced container had.
func TestInstanceManagerInitContainerKeepsTheHardening(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	sc := ss.Spec.Template.Spec.InitContainers[0].SecurityContext
	require.NotNil(t, sc, "the init container must have a security context")

	require.NotNil(t, sc.AllowPrivilegeEscalation)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.Privileged)
	assert.False(t, *sc.Privileged)
	require.NotNil(t, sc.ReadOnlyRootFilesystem)
	assert.True(t, *sc.ReadOnlyRootFilesystem)
	require.NotNil(t, sc.RunAsNonRoot)
	assert.True(t, *sc.RunAsNonRoot)
	require.NotNil(t, sc.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
}

// A user who sets spec.redis.containerSecurityContext must reach the init
// container too, exactly as with the container it replaces.
func TestInstanceManagerInitContainerHonoursTheUserSecurityContext(t *testing.T) {
	want := int64(2000)
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.ContainerSecurityContext = &corev1.SecurityContext{RunAsUser: &want}
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	sc := ss.Spec.Template.Spec.InitContainers[0].SecurityContext
	require.NotNil(t, sc)
	require.NotNil(t, sc.RunAsUser)
	assert.Equal(t, want, *sc.RunAsUser)
}

// The init container must put the redis-instance binary on the shared volume
// without a shell and without cp, because the operator image is distroless.
// The binary copies itself with its own bootstrap subcommand.
func TestInstanceManagerInitContainerCopiesWithTheBinaryItself(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	init := ss.Spec.Template.Spec.InitContainers[0]

	assert.Equal(t, "instance-manager-init", init.Name)
	assert.Equal(t, []string{
		"/usr/local/bin/redis-instance",
		"bootstrap",
		"/controller/redis-instance",
	}, init.Command)
}

// A distroless image has no shell and no coreutils. If the init container ever
// goes back to one of them, the Redis pods stop starting, and this names why.
func TestInstanceManagerInitContainerNeedsNoShellOrCoreutils(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	init := ss.Spec.Template.Spec.InitContainers[0]

	require.NotEmpty(t, init.Command)
	for _, forbidden := range []string{"cp", "sh", "/bin/sh", "bash", "/bin/bash"} {
		assert.NotEqual(t, forbidden, init.Command[0],
			"the operator image is distroless and holds no %s", forbidden)
	}
	assert.True(t, strings.HasSuffix(init.Command[0], "/redis-instance"),
		"the init container must run the redis-instance binary, got %q", init.Command[0])
}

// The destination of the copy must be the path that the Redis container runs,
// on the volume that both containers mount.
func TestInstanceManagerInitContainerWritesWhereRedisReadsIt(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	init := ss.Spec.Template.Spec.InitContainers[0]
	require.Len(t, init.Command, 3)
	dest := init.Command[2]

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	redis := ss.Spec.Template.Spec.Containers[0]
	require.NotEmpty(t, redis.Command)

	assert.Equal(t, dest, redis.Command[0],
		"the Redis container must run the file that the init container wrote")

	var initMount, redisMount string
	for _, m := range init.VolumeMounts {
		if strings.HasPrefix(dest, m.MountPath+"/") {
			initMount = m.Name
		}
	}
	for _, m := range redis.VolumeMounts {
		if strings.HasPrefix(dest, m.MountPath+"/") {
			redisMount = m.Name
		}
	}
	assert.Equal(t, "instance-manager", initMount, "the init container must mount the volume that holds %s", dest)
	assert.Equal(t, initMount, redisMount, "both containers must mount the same volume")
}

// spec.redis.instanceManagerImage chooses the image that holds the binary.
// Without it the operator falls back to the default, so the override is the
// only way to run a build that is not yet released.
func TestInstanceManagerInitContainerHonoursTheImageOverride(t *testing.T) {
	const want = "example.test/redis-operator:under-test"
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.InstanceManagerImage = want
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	assert.Equal(t, want, ss.Spec.Template.Spec.InitContainers[0].Image)
}

func TestInstanceManagerInitContainerFallsBackToTheDefaultImage(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	assert.Equal(t, redisfailoverv1.DefaultInstanceManagerImage,
		ss.Spec.Template.Spec.InitContainers[0].Image)
}

// The init container pulls its image at run time, so a constant pinned to a
// released image can never hold the operator's own new code. The Deployment
// gives the operator its own image in OPERATOR_IMAGE, and the init container
// uses that, so the binary it copies is always the one this operator shipped.
func TestInstanceManagerInitContainerUsesTheOperatorImage(t *testing.T) {
	const want = "example.test/redis-operator:built-by-ci"
	t.Setenv("OPERATOR_IMAGE", want)

	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	assert.Equal(t, want, ss.Spec.Template.Spec.InitContainers[0].Image)
}

// A user who names an image keeps it, whatever the operator runs.
func TestInstanceManagerImageOverrideBeatsTheOperatorImage(t *testing.T) {
	const want = "example.test/redis-operator:chosen"
	t.Setenv("OPERATOR_IMAGE", "example.test/redis-operator:built-by-ci")

	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.InstanceManagerImage = want
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	assert.Equal(t, want, ss.Spec.Template.Spec.InitContainers[0].Image)
}

// An empty variable is the same as no variable, so a Deployment that sets it
// from a value the user left blank does not break the pods.
func TestInstanceManagerIgnoresAnEmptyOperatorImage(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE", "")

	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	assert.Equal(t, redisfailoverv1.DefaultInstanceManagerImage,
		ss.Spec.Template.Spec.InitContainers[0].Image)
}

// The init container runs the operator image, not the Redis image, so the
// Redis pull policy does not describe it. The Deployment gives the operator
// its own policy in OPERATOR_IMAGE_PULL_POLICY. Without this, an image that
// is loaded into the node and never published cannot be pulled at all.
func TestInstanceManagerInitContainerUsesTheOperatorPullPolicy(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE_PULL_POLICY", "Never")

	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	assert.Equal(t, corev1.PullNever, ss.Spec.Template.Spec.InitContainers[0].ImagePullPolicy)
}

// Without the variable the behaviour does not change: the init container
// keeps the policy it had.
func TestInstanceManagerInitContainerFallsBackToTheRedisPullPolicy(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE_PULL_POLICY", "")

	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.ImagePullPolicy = corev1.PullIfNotPresent
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.InitContainers)
	assert.Equal(t, corev1.PullIfNotPresent, ss.Spec.Template.Spec.InitContainers[0].ImagePullPolicy)
}

// The Redis container must keep the Redis policy, whatever the operator runs.
func TestTheOperatorPullPolicyDoesNotReachTheRedisContainer(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE_PULL_POLICY", "Never")

	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.ImagePullPolicy = corev1.PullIfNotPresent
	})

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	assert.Equal(t, corev1.PullIfNotPresent, ss.Spec.Template.Spec.Containers[0].ImagePullPolicy)
}
