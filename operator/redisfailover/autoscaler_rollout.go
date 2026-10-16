package redisfailover

import (
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

// autoscaleRollout is what UpdateRedisesPods found for the memory autoscaler
// in the current reconcile.
type autoscaleRollout struct {
	// settled is true when the rollout waits on nothing, the StatefulSet
	// controller observed the latest template, and each pod is ready on the
	// update revision.
	settled bool
	// applied is true when the rollout is complete and each pod runs with the
	// effective memory limit or a larger one. anyApplied is true when one pod
	// does, or runs or starts on the update revision with that limit.
	applied, anyApplied bool
	// refusal is the permanent refusal of a resize to an autoscaler revision.
	refusal *resizeRefusal
	// complete is true when the other fields describe all pods, and no pod
	// is missing or terminating. A revert needs it, because an error can end
	// the rollout after a refusal.
	complete bool
	// requested holds the pods with an in-place resize that the kubelet did
	// not apply yet.
	requested []string
	// vpa is true when a VerticalPodAutoscaler changed a pod.
	vpa bool
	// updateRevision is the update revision of the StatefulSet.
	updateRevision string
	// restartOnResize is true when the redis container of a pod or of the
	// template restarts on a memory resize.
	restartOnResize bool
}

// vpaAnnotations are the pod annotations of the VerticalPodAutoscaler
// admission controller. It changes the pod resources at each pod create, so
// a step and the VerticalPodAutoscaler would work against each other.
var vpaAnnotations = []string{"vpaUpdates", "vpaObservedContainers"}

// resizeRefusal is a permanent refusal of the in-place resize of pod.
type resizeRefusal struct {
	pod     string
	class   rfservice.ResizeClass
	message string
}

// recordAutoscaleRollout completes the autoscaleRollout while the
// autoscaler makes steps. It reads the StatefulSet and its pods, so the other
// modes do not call it.
func (r *RedisFailoverHandler) recordAutoscaleRollout(rf *redisfailoverv1.RedisFailover, waits bool) error {
	if !r.autoscaleStepping(rf) {
		return nil
	}
	key := failoverKey(rf)
	v, _ := r.autoscaleRollout.Load(key)
	refused, _ := v.(autoscaleRollout)
	rollout := autoscaleRollout{refusal: refused.refusal}
	name := rfservice.GetRedisName(rf)
	ss, err := r.k8sservice.GetStatefulSet(rf.Namespace, name)
	if err != nil {
		return err
	}
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, name)
	if err != nil {
		return err
	}
	limit := rf.Spec.Redis.Resources.Limits.Memory().Value()
	// A missing or terminating pod comes back from the template of the
	// StatefulSet, and the result cannot tell which template that is.
	rollout.complete = len(pods.Items) >= int(rf.Spec.Redis.Replicas) && !slices.ContainsFunc(pods.Items, func(p corev1.Pod) bool { return p.DeletionTimestamp != nil })
	observed := ss.Status.ObservedGeneration >= ss.Generation
	rollout.settled = rollout.complete && !waits && rollout.refusal == nil && observed
	rollout.applied = rollout.complete && observed
	rollout.updateRevision = ss.Status.UpdateRevision
	rollout.restartOnResize = restartsOnMemoryResize(ss.Spec.Template.Spec.Containers)
	for i := range pods.Items {
		r.recordRolloutPod(rf, &rollout, &pods.Items[i], ss.Status.UpdateRevision, observed, limit)
	}
	r.autoscaleRollout.Store(key, rollout)
	return nil
}

// recordRolloutPod adds pod to rollout. observed tells that the StatefulSet
// controller observed the latest template with updateRevision.
func (r *RedisFailoverHandler) recordRolloutPod(rf *redisfailoverv1.RedisFailover, rollout *autoscaleRollout, pod *corev1.Pod, updateRevision string, observed bool, limit int64) {
	updated := pod.Labels[appsv1.ControllerRevisionHashLabelKey] == updateRevision
	requested := rfservice.ResizeRequested(pod)
	if requested && updated && observed && rfservice.AppliedRedisMemoryLimit(pod) >= limit {
		// The rollout moved the pod after the resize, and the removal of
		// the request failed. Nothing else removes it, and it would hold
		// each later step.
		err := r.rfHealer.ClearResizeRequested(rf, pod.Name)
		r.logClearError(rf, pod.Name, err)
		requested = err != nil
	}
	if !updated || requested || !util.PodIsReady(pod) {
		rollout.settled = false
	}
	if requested {
		rollout.requested = append(rollout.requested, pod.Name)
	}
	if !updated || rfservice.AppliedRedisMemoryLimit(pod) < limit {
		rollout.applied = false
	}
	rollout.anyApplied = rollout.anyApplied || stepPod(pod, limit)
	for _, a := range vpaAnnotations {
		_, found := pod.Annotations[a]
		rollout.vpa = rollout.vpa || found
	}
	rollout.restartOnResize = rollout.restartOnResize || restartsOnMemoryResize(pod.Spec.Containers)
}

// stepPod reports whether pod applied the memory limit to, or starts with
// it. A revert would make such a pod larger than the template. A pod that no
// node can hold has no data.
func stepPod(pod *corev1.Pod, to int64) bool {
	applied := rfservice.AppliedRedisMemoryLimit(pod)
	return applied >= to || applied == 0 && rfservice.RequestedRedisMemoryLimit(pod) >= to && !unplaceable(pod)
}

// stepPodsApplied lists the pods for a revert without the rollout result of
// this reconcile. It reports whether a pod is a stepPod, or, with absent,
// whether a pod is missing or terminates. It returns the pods with a resize
// request. Without the StatefulSet, the pods are absent.
func (r *RedisFailoverHandler) stepPodsApplied(rf *redisfailoverv1.RedisFailover, to int64, absent bool) (bool, []string, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if apierrors.IsNotFound(err) {
		return absent, nil, nil
	}
	if err != nil {
		return true, nil, err
	}
	if absent && len(pods.Items) < int(rf.Spec.Redis.Replicas) {
		return true, nil, nil
	}
	var requested []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			if absent {
				return true, nil, nil
			}
			continue
		}
		if stepPod(pod, to) {
			return true, nil, nil
		}
		if rfservice.ResizeRequested(pod) {
			requested = append(requested, pod.Name)
		}
	}
	return false, requested, nil
}

// restartsOnMemoryResize reports whether the redis container restarts on a
// change of its memory, for example after a mutating webhook. A step would
// then restart Redis.
func restartsOnMemoryResize(containers []corev1.Container) bool {
	for _, c := range containers {
		if c.Name != "redis" {
			continue
		}
		for _, p := range c.ResizePolicy {
			if p.ResourceName == corev1.ResourceMemory && p.RestartPolicy == corev1.RestartContainer {
				return true
			}
		}
	}
	return false
}

// logResizeRefusal logs a refusal at Warning when it differs from the last
// one, and at Debug otherwise. The rollout tries the resize at each reconcile.
func (r *RedisFailoverHandler) logResizeRefusal(rf *redisfailoverv1.RedisFailover, refusal *resizeRefusal) {
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("pod", refusal.pod)
	text := refusal.String()
	if last, _ := r.autoscaleRefusalLogged.Swap(failoverKey(rf), text); last == text {
		logger.Debugf("The resize to the autoscaler revision failed: %s", refusal.message)
		return
	}
	logger.Warningf("The resize to the autoscaler revision failed: %s", refusal.message)
}

// logClearError logs a failed removal of a resize request at Warning when it
// differs from the last one, and at Debug otherwise.
func (r *RedisFailoverHandler) logClearError(rf *redisfailoverv1.RedisFailover, pod string, err error) {
	if err == nil {
		r.autoscaleClearLogged.Delete(failoverKey(rf))
		return
	}
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("pod", pod)
	text := pod + ": " + err.Error()
	if last, _ := r.autoscaleClearLogged.Swap(failoverKey(rf), text); last == text {
		logger.Debugf("Autoscaler: the operator cannot remove the resize request: %s", err.Error())
		return
	}
	logger.Warningf("Autoscaler: the operator cannot remove the resize request: %s", err.Error())
}

func (f *resizeRefusal) String() string {
	if f.message == "" {
		return "pod " + f.pod
	}
	return "pod " + f.pod + ": " + f.message
}
