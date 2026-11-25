package redisfailover

import (
	"slices"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	appsv1 "k8s.io/api/apps/v1"
)

// unschedulableStalePod returns a pod on a stale revision that the scheduler
// cannot place and that never started a container, or nil.
func (r *RedisFailoverHandler) unschedulableStalePod(rf *redisfailoverv1.RedisFailover) (*corev1.Pod, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	var candidates []*corev1.Pod
	for i := range pods.Items {
		if pod := &pods.Items[i]; unplaceable(pod) {
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// Before the StatefulSet controller sees a change of the spec, the update
	// revision in the status is the one before. A pod that the controller
	// created on the new revision would then look stale.
	ss, err := r.k8sservice.GetStatefulSet(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	if ss == nil || ss.Status.UpdateRevision == "" || ss.Status.ObservedGeneration < ss.Generation {
		return nil, nil
	}
	for _, pod := range candidates {
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] != ss.Status.UpdateRevision {
			return pod, nil
		}
	}
	return nil, nil
}

// unplaceable reports whether pod is Pending because the scheduler cannot
// place it, and never started a container. Such a pod holds no data.
func unplaceable(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodPending || pod.DeletionTimestamp != nil {
		return false
	}
	unschedulable := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled {
			unschedulable = c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable
		}
	}
	started := slices.ContainsFunc(slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses), func(cs corev1.ContainerStatus) bool {
		return cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil || cs.RestartCount > 0
	})
	return unschedulable && !started
}
