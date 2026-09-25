package util

import v1 "k8s.io/api/core/v1"

func PodIsTerminal(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded
}

func PodIsScheduling(pod *v1.Pod) bool {
	return pod.DeletionTimestamp != nil || pod.Status.Phase == v1.PodPending
}

func PodIsReady(pod *v1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == v1.PodReady {
			return c.Status == v1.ConditionTrue
		}
	}
	return false
}
