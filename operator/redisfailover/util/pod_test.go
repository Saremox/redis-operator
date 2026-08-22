package util_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

func TestPodIsScheduling(t *testing.T) {
	now := metav1.NewTime(time.Now())

	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "pod stuck in ContainerCreating/ImagePullBackOff reports Phase Pending with no DeletionTimestamp - this is the exact condition that must block premature master-pod deletion during a stuck rollout",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expected: true,
		},
		{
			name: "pending phase with no deletion timestamp is scheduling",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expected: true,
		},
		{
			name: "deletion timestamp set with Running phase is scheduling regardless of phase",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expected: true,
		},
		{
			name: "deletion timestamp set with Failed phase is still scheduling",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expected: true,
		},
		{
			name: "deletion timestamp set with Succeeded phase is still scheduling",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			expected: true,
		},
		{
			name: "deletion timestamp set with Pending phase is scheduling",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expected: true,
		},
		{
			name: "running phase with no deletion timestamp is not scheduling",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expected: false,
		},
		{
			name: "failed phase with no deletion timestamp is not scheduling",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expected: false,
		},
		{
			name: "succeeded phase with no deletion timestamp is not scheduling",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			expected: false,
		},
		{
			name: "unknown phase with no deletion timestamp is not scheduling",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodUnknown,
				},
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, util.PodIsScheduling(test.pod))
		})
	}
}

func TestPodIsTerminal(t *testing.T) {
	now := metav1.NewTime(time.Now())

	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "failed phase is terminal",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expected: true,
		},
		{
			name: "succeeded phase is terminal",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			expected: true,
		},
		{
			name: "pending phase is not terminal",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expected: false,
		},
		{
			name: "running phase is not terminal",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expected: false,
		},
		{
			name: "unknown phase is not terminal",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodUnknown,
				},
			},
			expected: false,
		},
		{
			// This distinction matters for AreAllRunning: a DeletionTimestamp alone makes a
			// pod "scheduling" (PodIsScheduling), which short-circuits the whole check to
			// false, but it must NOT make the pod "terminal" (PodIsTerminal) - only an actual
			// Failed/Succeeded phase does that. Terminal pods are skipped/not counted by
			// AreAllRunning, whereas a scheduling pod fails the check outright.
			name: "deletion timestamp alone with Running phase does not make the pod terminal",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expected: false,
		},
		{
			name: "deletion timestamp alone with Pending phase does not make the pod terminal",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expected, util.PodIsTerminal(test.pod))
		})
	}
}
