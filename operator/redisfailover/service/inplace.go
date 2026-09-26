package service

import (
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/k8s"
)

// ResizeAction is what the rollout does with a pod that is not on the update revision.
type ResizeAction int

const (
	// ResizeRecreate means the pod has to be deleted and recreated.
	ResizeRecreate ResizeAction = iota
	// ResizeWaiting means an in-place resize of the pod is in progress.
	ResizeWaiting
	// ResizeDone means the pod was resized in place and moved to the update revision.
	ResizeDone
)

// ResizeResult is the outcome of ResizePodInPlace.
type ResizeResult struct {
	Action ResizeAction
	// Message explains a resize that is stuck or fell back to recreating the pod.
	Message string
}

// inPlaceResizeTimeout bounds how long a deferred or failing resize is waited
// for before the pod is recreated instead.
var inPlaceResizeTimeout = 5 * time.Minute

var resizableResources = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

// ResizePodInPlace moves a redis pod to the update revision without
// recreating it when the revisions differ only in container cpu and memory.
// The kubelet applies the resize without restarting the containers; once the
// applied resources match, the pod's revision label is updated, which
// UpdateRedisesPods then treats as up to date. The StatefulSet uses OnDelete,
// so its controller never replaces the relabelled pod.
func (r *RedisFailoverHealer) ResizePodInPlace(rf *redisfailoverv1.RedisFailover, podName, updateRevision string) (ResizeResult, error) {
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).WithField("pod", podName)
	recreate := func(reason string) (ResizeResult, error) {
		logger.Infof("Recreating the pod instead of resizing it in place: %s", reason)
		return ResizeResult{Action: ResizeRecreate, Message: reason}, nil
	}
	waiting := func(format string, args ...interface{}) (ResizeResult, error) {
		return ResizeResult{Action: ResizeWaiting, Message: fmt.Sprintf(format, args...)}, nil
	}

	if rf.Spec.Redis.InPlaceResize == "Disabled" {
		return ResizeResult{Action: ResizeRecreate}, nil
	}
	support, err := r.k8sService.PodResizeSupport()
	if err != nil {
		return ResizeResult{}, err
	}
	if !support.Supported {
		return ResizeResult{Action: ResizeRecreate}, nil
	}
	pod, err := r.k8sService.GetPod(rf.Namespace, podName)
	if err != nil {
		return ResizeResult{}, err
	}
	desired, reason, err := r.inPlaceResources(rf, pod, updateRevision, support)
	if err != nil {
		return ResizeResult{}, err
	}
	if reason != "" {
		return recreate(reason)
	}

	if !podRequests(pod, desired) {
		if err := r.markResizeRequested(rf, podName); err != nil {
			return ResizeResult{}, err
		}
		if err := r.k8sService.ResizePod(rf.Namespace, podName, desired); err != nil {
			// Rejected by the API server, e.g. missing RBAC or a disabled feature gate.
			if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) {
				return recreate("resize rejected: " + err.Error())
			}
			return ResizeResult{}, err
		}
		logger.Infof("Resizing the pod in place")
		return waiting("resizing pod %s in place", podName)
	}

	// A condition keeps its transition time when a newer resize supersedes
	// the one it reported on.
	sinceLatest := func(t time.Time) time.Duration {
		if requested, err := time.Parse(time.RFC3339, pod.Annotations[resizeRequestedAnnotation]); err == nil && requested.After(t) {
			t = requested
		}
		return time.Since(t)
	}
	if c := podCondition(pod, corev1.PodResizePending); c != nil {
		if c.Reason == corev1.PodReasonInfeasible {
			return recreate("resize infeasible: " + c.Message)
		}
		if sinceLatest(c.LastTransitionTime.Time) > inPlaceResizeTimeout {
			return recreate("resize deferred for too long: " + c.Message)
		}
		return waiting("resize of pod %s deferred: %s", podName, c.Message)
	}
	if c := podCondition(pod, corev1.PodResizeInProgress); c != nil {
		// E.g. a memory limit below the usage the kubelet sees, which counts
		// the page cache.
		if c.Reason == corev1.PodReasonError && sinceLatest(c.LastTransitionTime.Time) > inPlaceResizeTimeout {
			return recreate("resize failed: " + c.Message)
		}
		return waiting("resize of pod %s in progress", podName)
	}
	if !podApplied(pod, desired) {
		// The kubelet may not have picked the resize up yet.
		requested, err := time.Parse(time.RFC3339, pod.Annotations[resizeRequestedAnnotation])
		if err != nil {
			if err := r.markResizeRequested(rf, podName); err != nil {
				return ResizeResult{}, err
			}
		} else if time.Since(requested) > inPlaceResizeTimeout {
			return recreate("resize not applied by the kubelet")
		}
		return waiting("resize of pod %s in progress", podName)
	}

	if err := r.k8sService.UpdatePodLabels(rf.Namespace, podName, map[string]string{appsv1.ControllerRevisionHashLabelKey: updateRevision}); err != nil {
		return ResizeResult{}, err
	}
	logger.Infof("Resized the pod in place")
	return ResizeResult{Action: ResizeDone}, nil
}

func (r *RedisFailoverHealer) markResizeRequested(rf *redisfailoverv1.RedisFailover, podName string) error {
	return r.k8sService.UpdatePodAnnotations(rf.Namespace, podName, map[string]string{resizeRequestedAnnotation: time.Now().UTC().Format(time.RFC3339)})
}

// inPlaceResources returns the container resources of the update revision
// when the pod can be moved to it by an in-place resize, or the reason it
// cannot.
func (r *RedisFailoverHealer) inPlaceResources(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod, updateRevision string, support k8s.PodResizeSupport) (map[string]corev1.ResourceRequirements, string, error) {
	current, err := r.revisionTemplate(rf, pod.Labels[appsv1.ControllerRevisionHashLabelKey])
	if current == nil || err != nil {
		return nil, "the pod's revision is not available", err
	}
	update, err := r.revisionTemplate(rf, updateRevision)
	if update == nil || err != nil {
		return nil, "the update revision is not available", err
	}
	if !equality.Semantic.DeepEqual(withoutResources(current), withoutResources(update)) {
		return nil, "the update changes more than container resources", nil
	}

	old := map[string]corev1.ResourceRequirements{}
	for _, c := range current.Spec.Containers {
		old[c.Name] = withDefaultRequests(c.Resources)
	}
	// The pod's own resources are the base: admission (e.g. a LimitRange) may
	// have added values that neither revision has.
	actual, running := map[string]corev1.ResourceRequirements{}, map[string]corev1.ResourceRequirements{}
	for _, c := range pod.Spec.Containers {
		actual[c.Name], running[c.Name] = c.Resources, c.Resources
	}
	reported := false
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Resources != nil {
			running[cs.Name] = *cs.Resources
			reported = reported || cs.Name == redisContainerName
		}
	}
	if !reported {
		// E.g. a kubelet without in-place resize support, which would leave
		// the pod spec that maxmemory derives from ahead of the container.
		return nil, "the kubelet does not report the container's resources", nil
	}
	desired := map[string]corev1.ResourceRequirements{}
	lowersMemoryLimit := false
	for _, c := range update.Spec.Containers {
		d, o := withDefaultRequests(c.Resources), old[c.Name]
		for _, list := range [][2]corev1.ResourceList{{o.Requests, d.Requests}, {o.Limits, d.Limits}} {
			if len(list[0]) != len(list[1]) {
				return nil, "requests or limits are added or removed", nil
			}
			for name, q := range list[0] {
				n, ok := list[1][name]
				if !ok {
					return nil, "requests or limits are added or removed", nil
				}
				if name != corev1.ResourceCPU && name != corev1.ResourceMemory && !q.Equal(n) {
					return nil, "only cpu and memory can be resized in place", nil
				}
			}
		}
		// The pod may already run a superseded revision's values.
		base := actual[c.Name]
		target := *base.DeepCopy()
		target.Requests = overlay(target.Requests, d.Requests)
		target.Limits = overlay(target.Limits, d.Limits)
		desired[c.Name] = target
		if q, ok := running[c.Name].Limits[corev1.ResourceMemory]; ok && target.Limits.Memory().Cmp(q) < 0 {
			lowersMemoryLimit = true
		}
	}
	if lowersMemoryLimit && !support.MemoryLimitDecrease {
		return nil, "lowering a memory limit in place needs Kubernetes 1.35", nil
	}
	return desired, "", nil
}

// overlay sets the cpu and memory values of new on list.
func overlay(list, new corev1.ResourceList) corev1.ResourceList {
	for _, name := range resizableResources {
		if n, ok := new[name]; ok {
			if list == nil {
				list = corev1.ResourceList{}
			}
			list[name] = n
		}
	}
	return list
}

func (r *RedisFailoverHealer) revisionTemplate(rf *redisfailoverv1.RedisFailover, revision string) (*corev1.PodTemplateSpec, error) {
	// The revision hash label and the update revision are the ControllerRevision's name.
	cr, err := r.k8sService.GetControllerRevision(rf.Namespace, revision)
	// Forbidden without the RBAC added for in-place resize: recreate instead.
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The StatefulSet controller stores the template as a patch of the
	// StatefulSet: {"spec":{"template":{...}}}.
	var data struct {
		Spec struct {
			Template corev1.PodTemplateSpec `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cr.Data.Raw, &data); err != nil {
		return nil, err
	}
	return &data.Spec.Template, nil
}

func withoutResources(t *corev1.PodTemplateSpec) *corev1.PodTemplateSpec {
	t = t.DeepCopy()
	for i := range t.Spec.Containers {
		t.Spec.Containers[i].Resources.Requests = nil
		t.Spec.Containers[i].Resources.Limits = nil
	}
	return t
}

// withDefaultRequests sets unset requests to their limits, as the API server
// does for pods but not for the templates they are created from.
func withDefaultRequests(r corev1.ResourceRequirements) corev1.ResourceRequirements {
	r = *r.DeepCopy()
	for name, q := range r.Limits {
		if _, ok := r.Requests[name]; !ok {
			if r.Requests == nil {
				r.Requests = corev1.ResourceList{}
			}
			r.Requests[name] = q
		}
	}
	return r
}

// podRequests reports whether the pod spec already requests the desired resources.
func podRequests(pod *corev1.Pod, desired map[string]corev1.ResourceRequirements) bool {
	for _, c := range pod.Spec.Containers {
		if d, ok := desired[c.Name]; ok && !sameResizable(c.Resources, d) {
			return false
		}
	}
	return true
}

// podApplied reports whether the kubelet reports the desired resources as applied.
func podApplied(pod *corev1.Pod, desired map[string]corev1.ResourceRequirements) bool {
	applied := map[string]*corev1.ResourceRequirements{}
	for _, cs := range pod.Status.ContainerStatuses {
		applied[cs.Name] = cs.Resources
	}
	for name, d := range desired {
		if a := applied[name]; a == nil || !sameResizable(*a, d) {
			return false
		}
	}
	return true
}

func sameResizable(a, b corev1.ResourceRequirements) bool {
	for _, name := range resizableResources {
		for _, list := range [][2]corev1.ResourceList{{a.Requests, b.Requests}, {a.Limits, b.Limits}} {
			qa, oka := list[0][name]
			qb, okb := list[1][name]
			if oka != okb || !qa.Equal(qb) {
				return false
			}
		}
	}
	return true
}

// podCondition returns the pod's true condition of type t, ignoring one left
// from an earlier resize request.
func podCondition(pod *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.ObservedGeneration != 0 && c.ObservedGeneration < pod.Generation {
			continue
		}
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
