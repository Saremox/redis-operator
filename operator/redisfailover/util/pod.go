package util

import v1 "k8s.io/api/core/v1"

// PodIsTerminal reports whether the pod is in the Failed or the Succeeded phase.
func PodIsTerminal(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded
}

// PodIsScheduling reports whether the pod has a deletion timestamp or is in the
// Pending phase.
func PodIsScheduling(pod *v1.Pod) bool {
	return pod.DeletionTimestamp != nil || pod.Status.Phase == v1.PodPending
}

// PodIsReady reports whether the Ready condition of the pod is True.
func PodIsReady(pod *v1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == v1.PodReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}
