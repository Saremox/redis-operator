package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// withSentinel turns Sentinel on. sentinel.enabled defaults to false, so the
// RedisFailover that redisStatefulSetFor builds is sentinel-less.
func withSentinel(rf *redisfailoverv1.RedisFailover) {
	rf.Spec.Sentinel.Enabled = ptr.To(true)
}

// requirePreStopHook asserts that the Redis container runs the shutdown
// script before it stops.
func requirePreStopHook(t *testing.T, ss *appsv1.StatefulSet, why string) {
	t.Helper()

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	lifecycle := ss.Spec.Template.Spec.Containers[0].Lifecycle
	require.NotNil(t, lifecycle, why)
	require.NotNil(t, lifecycle.PreStop)
	require.NotNil(t, lifecycle.PreStop.Exec)
	assert.Equal(t, []string{"/bin/sh", "/redis-shutdown/shutdown.sh"},
		lifecycle.PreStop.Exec.Command)
}

// In Sentinel mode the hook asks Sentinel to fail the master over before it
// stops, and nothing else does this. It must stay.
func TestRedisKeepsThePreStopHookWithSentinel(t *testing.T) {
	ss := redisStatefulSetFor(t, withSentinel)

	requirePreStopHook(t, ss, "Sentinel mode needs the preStop hook to fail over")
}

// A custom script replaces the default one, so the user decides what runs. The
// hook must stay, or the script never runs.
func TestRedisKeepsThePreStopHookWithACustomShutdownConfigMap(t *testing.T) {
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		rf.Spec.Redis.ShutdownConfigMap = "my-shutdown"
	})

	requirePreStopHook(t, ss, "a custom shutdown script must still run")
}

// The hook follows sentinel.enabled, not SentinelsAllowed. Otherwise the start
// or the end of a bootstrap changes the pod template and restarts the pods.
func TestRedisKeepsThePreStopHookInABootstrapWithoutSentinels(t *testing.T) {
	ss := redisStatefulSetFor(t, func(rf *redisfailoverv1.RedisFailover) {
		withSentinel(rf)
		rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "10.0.0.1"}
	})

	requirePreStopHook(t, ss, "a bootstrap must not change the pod template")
}

// Without Sentinel the script is only "redis-cli save". redis-server is PID 1
// and redis.conf has save points, so it saves on SIGTERM by itself. The hook
// adds nothing and costs the grace period, so the container gets none.
func TestRedisHasNoPreStopHookWithoutSentinel(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	assert.Nil(t, ss.Spec.Template.Spec.Containers[0].Lifecycle,
		"without Sentinel the hook only runs a synchronous SAVE")
}

// The ConfigMap and its mount stay in both modes, so that nothing else has to
// change and a user who runs the script by hand still finds it.
func TestRedisKeepsTheShutdownMountWithoutSentinel(t *testing.T) {
	ss := redisStatefulSetFor(t, nil)

	require.NotEmpty(t, ss.Spec.Template.Spec.Containers)
	var found bool
	for _, m := range ss.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/redis-shutdown" {
			found = true
		}
	}
	assert.True(t, found, "the shutdown ConfigMap must stay mounted")
}
