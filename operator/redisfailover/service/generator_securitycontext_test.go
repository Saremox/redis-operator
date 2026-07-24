package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func int64Ptr(v int64) *int64 { return &v }
func boolPtr(v bool) *bool    { return &v }

func TestGetSecurityContextMerge(t *testing.T) {
	t.Run("nil user context returns all defaults", func(t *testing.T) {
		got := getSecurityContext(nil)
		assert.Equal(t, int64(1000), *got.RunAsUser)
		assert.Equal(t, int64(1000), *got.RunAsGroup)
		assert.True(t, *got.RunAsNonRoot)
		assert.Equal(t, int64(1000), *got.FSGroup)
	})

	t.Run("partial user context keeps defaults for unset fields", func(t *testing.T) {
		got := getSecurityContext(&corev1.PodSecurityContext{
			RunAsUser: int64Ptr(2000),
		})
		// User value wins.
		assert.Equal(t, int64(2000), *got.RunAsUser)
		// Defaults still fill the rest.
		assert.Equal(t, int64(1000), *got.RunAsGroup)
		assert.True(t, *got.RunAsNonRoot)
		assert.Equal(t, int64(1000), *got.FSGroup)
	})

	t.Run("user context does not mutate the input", func(t *testing.T) {
		user := &corev1.PodSecurityContext{RunAsUser: int64Ptr(2000)}
		_ = getSecurityContext(user)
		assert.Nil(t, user.FSGroup, "input must not be mutated")
	})
}

func TestGetContainerSecurityContextMerge(t *testing.T) {
	t.Run("nil user context returns all defaults", func(t *testing.T) {
		got := getContainerSecurityContext(nil)
		assert.Equal(t, int64(1000), *got.RunAsUser)
		assert.False(t, *got.Privileged)
		assert.True(t, *got.ReadOnlyRootFilesystem)
		assert.False(t, *got.AllowPrivilegeEscalation)
		assert.Equal(t, []corev1.Capability{"ALL"}, got.Capabilities.Drop)
	})

	t.Run("partial user context keeps defaults for unset fields", func(t *testing.T) {
		got := getContainerSecurityContext(&corev1.SecurityContext{
			ReadOnlyRootFilesystem: boolPtr(false),
		})
		// User value wins.
		assert.False(t, *got.ReadOnlyRootFilesystem)
		// Defaults still fill the rest.
		assert.Equal(t, int64(1000), *got.RunAsUser)
		assert.False(t, *got.AllowPrivilegeEscalation)
		assert.Equal(t, []corev1.Capability{"ALL"}, got.Capabilities.Drop)
	})
}
