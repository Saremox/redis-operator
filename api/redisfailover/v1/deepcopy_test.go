package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestSentinelSettingsDeepCopyClonesPointerFields guards against
// zz_generated.deepcopy.go going stale again: SentinelSettings.DeepCopyInto
// had explicit clone blocks for every other pointer/slice/map field, but was
// never regenerated after Enabled and FailoverTimeout were added, so a
// DeepCopy() shared those two pointers between the original and the copy
// instead of cloning them - exactly the shared-mutable-state bug deep-copying
// informer-cache objects before reconciling is meant to prevent.
func TestSentinelSettingsDeepCopyClonesPointerFields(t *testing.T) {
	assert := assert.New(t)

	original := &SentinelSettings{
		Enabled:         ptr.To(true),
		FailoverTimeout: &metav1.Duration{Duration: 10},
	}

	clone := original.DeepCopy()

	if assert.NotNil(clone.Enabled) && assert.NotNil(clone.FailoverTimeout) {
		assert.NotSame(original.Enabled, clone.Enabled, "Enabled must be a distinct pointer, not shared with the original")
		assert.NotSame(original.FailoverTimeout, clone.FailoverTimeout, "FailoverTimeout must be a distinct pointer, not shared with the original")

		*clone.Enabled = false
		clone.FailoverTimeout.Duration = 20

		assert.True(*original.Enabled, "mutating the clone must not affect the original")
		assert.Equal(metav1.Duration{Duration: 10}, *original.FailoverTimeout, "mutating the clone must not affect the original")
	}
}

func TestSettingsDeepCopyClonesEnv(t *testing.T) {
	assert := assert.New(t)
	env := func() []corev1.EnvVar {
		return []corev1.EnvVar{{
			Name:      "FROM_SECRET",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "a"}},
		}}
	}
	redis := &RedisSettings{Env: env()}
	sentinel := &SentinelSettings{Env: env()}

	redisClone := redis.DeepCopy()
	sentinelClone := sentinel.DeepCopy()
	redisClone.Env[0].ValueFrom.SecretKeyRef.Key = "b"
	sentinelClone.Env[0].ValueFrom.SecretKeyRef.Key = "b"

	assert.Equal(env(), redis.Env, "mutating the clone must not affect the original")
	assert.Equal(env(), sentinel.Env, "mutating the clone must not affect the original")
}
