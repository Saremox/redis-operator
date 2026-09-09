package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func podsWithPhases(phases ...corev1.PodPhase) *corev1.PodList {
	pods := &corev1.PodList{}
	for _, p := range phases {
		pods.Items = append(pods.Items, corev1.Pod{Status: corev1.PodStatus{Phase: p}})
	}
	return pods
}

func TestAreQuorumRunning(t *testing.T) {
	run, pend, fail := corev1.PodRunning, corev1.PodPending, corev1.PodFailed

	tests := []struct {
		name     string
		phases   []corev1.PodPhase
		replicas int
		expected bool
	}{
		{"all three running", []corev1.PodPhase{run, run, run}, 3, true},
		{"two of three running is quorum", []corev1.PodPhase{run, run, pend}, 3, true},
		{"one of three running is below quorum", []corev1.PodPhase{run, pend, pend}, 3, false},
		{"failed pod does not count", []corev1.PodPhase{run, run, fail}, 3, true},
		{"single replica running", []corev1.PodPhase{run}, 1, true},
		{"single replica pending", []corev1.PodPhase{pend}, 1, false},
		{"five replicas three running is quorum", []corev1.PodPhase{run, run, run, pend, pend}, 5, true},
		{"five replicas two running below quorum", []corev1.PodPhase{run, run, pend, pend, pend}, 5, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := rfservice.AreQuorumRunning(podsWithPhases(test.phases...), test.replicas)
			assert.Equal(t, test.expected, got)
		})
	}
}
