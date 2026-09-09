package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
)

func rfWithEvictionProtection(enabled bool) *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{PreventMasterEviction: enabled},
		},
	}
}

func podNamed(name string, annotations map[string]string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
}

func TestApplyMasterEvictionAnnotation(t *testing.T) {
	t.Run("flag disabled makes no annotation call", func(t *testing.T) {
		ms := &mK8SService.Services{}
		err := applyMasterEvictionAnnotation(ms, rfWithEvictionProtection(false), podNamed("p0", nil), true)
		assert.NoError(t, err)
		ms.AssertNotCalled(t, "UpdatePodAnnotations", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("master is pinned with safe-to-evict false", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("UpdatePodAnnotations", "testns", "p0", map[string]string{
			masterSafeToEvictAnnotation: "false",
		}).Once().Return(nil)

		err := applyMasterEvictionAnnotation(ms, rfWithEvictionProtection(true), podNamed("p0", nil), true)
		assert.NoError(t, err)
		ms.AssertExpectations(t)
	})

	t.Run("slave is marked evictable with safe-to-evict true", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("UpdatePodAnnotations", "testns", "p1", map[string]string{
			masterSafeToEvictAnnotation: "true",
		}).Once().Return(nil)

		err := applyMasterEvictionAnnotation(ms, rfWithEvictionProtection(true), podNamed("p1", nil), false)
		assert.NoError(t, err)
		ms.AssertExpectations(t)
	})

	t.Run("no call when annotation already at desired value", func(t *testing.T) {
		ms := &mK8SService.Services{}
		pod := podNamed("p0", map[string]string{masterSafeToEvictAnnotation: "false"})
		err := applyMasterEvictionAnnotation(ms, rfWithEvictionProtection(true), pod, true)
		assert.NoError(t, err)
		ms.AssertNotCalled(t, "UpdatePodAnnotations", mock.Anything, mock.Anything, mock.Anything)
	})
}
