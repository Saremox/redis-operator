package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestMapsEqual(t *testing.T) {
	assert := assert.New(t)

	assert.True(mapsEqual(nil, nil))
	assert.True(mapsEqual(nil, map[string]string{}))
	assert.True(mapsEqual(map[string]string{}, nil))
	assert.True(mapsEqual(map[string]string{"a": "1"}, map[string]string{"a": "1"}))

	assert.False(mapsEqual(map[string]string{"a": "1"}, nil))
	assert.False(mapsEqual(map[string]string{"a": "1"}, map[string]string{"a": "2"}))
	assert.False(mapsEqual(map[string]string{"a": "1"}, map[string]string{"b": "1"}))
	assert.False(mapsEqual(map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}))
}

func TestNormalizePodSpecForComparison(t *testing.T) {
	assert := assert.New(t)

	spec := &corev1.PodSpec{
		RestartPolicy:            corev1.RestartPolicyAlways,
		SchedulerName:            "default-scheduler",
		DeprecatedServiceAccount: "sa",
		Containers: []corev1.Container{
			{Name: "a", TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile},
		},
		InitContainers: []corev1.Container{
			{Name: "init", TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: corev1.TerminationMessageReadFile},
		},
	}

	normalizePodSpecForComparison(spec)

	assert.Equal(corev1.RestartPolicy(""), spec.RestartPolicy)
	assert.Empty(spec.SchedulerName)
	assert.Empty(spec.DeprecatedServiceAccount)
	assert.Empty(spec.Containers[0].TerminationMessagePath)
	assert.Empty(spec.Containers[0].TerminationMessagePolicy)
	assert.Empty(spec.InitContainers[0].TerminationMessagePath)
	assert.Empty(spec.InitContainers[0].TerminationMessagePolicy)
	// The container's identity must be untouched.
	assert.Equal("a", spec.Containers[0].Name)
}
