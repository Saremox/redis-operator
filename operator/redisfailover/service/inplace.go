package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
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

// ResizeClass tells whether a later attempt of the resize can succeed.
type ResizeClass string

const (
	// ResizeUserRevision means that the revisions differ in more than cpu and
	// memory, so only a recreate moves the pod.
	ResizeUserRevision ResizeClass = "UserRevision"
	// ResizeTransient means that the resize can still succeed.
	ResizeTransient ResizeClass = "Transient"
	// ResizePermanent means that the cluster refuses the resize, or that the
	// kubelet did not apply it in time.
	ResizePermanent ResizeClass = "Permanent"
)

// AutoscalerLimits are the memory limits of the redis container with
// autoscaling. The zero value means that no step of the autoscaler is
// possible.
type AutoscalerLimits struct {
	// Effective is the limit of the pods, or 0. Spec is the limit of
	// redis.resources, the limit before the step when the spec limit is at
	// the target of the step, or 0 without a known state. Only a revision to
	// an Effective above Spec can be a step.
	Spec, Effective int64
	// Step is the limit of the last autoscaler step, or 0. A spec limit at or
	// above it and a memoryMax below it end it. A pod with this limit keeps
	// it, also when it only requests it since a revert.
	Step int64
	// StepMax is memoryMax when the state of the last step is not known, or
	// 0. Then a pod with a larger limit up to StepMax keeps it, as for Step.
	StepMax int64
}

// ResizeResult is the outcome of ResizePodInPlace.
type ResizeResult struct {
	Action ResizeAction
	// Class is empty for ResizeDone.
	Class ResizeClass
	// Message is the progress of a resize, the reason for a recreate, or
	// empty.
	Message string
	// AutoscalerRevision reports that the update revision is a step of the
	// memory autoscaler from the pod's revision, that the pod requests the
	// Step limit, or that a revision cannot be read with autoscaler limits.
	// The rollout never recreates such a pod.
	AutoscalerRevision bool
}

// inPlaceResizeTimeout bounds how long a deferred or failing resize is waited
// for before the pod is recreated instead.
var inPlaceResizeTimeout = 5 * time.Minute

// memoryBelowUsage is the text that the kubelet error
// (validateMemoryResizeAction) contains for a refused pod limit and for a
// refused container limit.
const memoryBelowUsage = "below current usage"

// resourceLimitRejections are texts of the errors of the ResourceQuota
// admission and of the LimitRanger admission. The messages are the same for
// a pod create and a resize.
var resourceLimitRejections = []string{"exceeded quota: ", "insufficient quota to consume: ", " usage per ", " max limit to request ratio per "}

var resizableResources = []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}

func rejectedByResourceLimits(err error) bool {
	for _, s := range resourceLimitRejections {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}

// ResizePodInPlace moves a redis pod to the update revision without
// recreating it when the revisions differ only in container cpu and memory.
// The kubelet applies the resize without restarting the containers; once the
// applied resources match, the pod's revision label is updated, which
// UpdateRedisesPods then treats as up to date. The StatefulSet uses OnDelete,
// so its controller never replaces the relabelled pod.
func (r *RedisFailoverHealer) ResizePodInPlace(rf *redisfailoverv1.RedisFailover, podName, updateRevision string, limits AutoscalerLimits) (ResizeResult, error) {
	s := &podResize{
		r:              r,
		rf:             rf,
		podName:        podName,
		updateRevision: updateRevision,
		logger:         r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).WithField("pod", podName),
	}
	unsupported, support, err := r.inPlaceResizeSupport(rf)
	if err != nil {
		return ResizeResult{}, err
	}
	if unsupported != "" && limits == (AutoscalerLimits{}) {
		// Without a step, no revision needs a read.
		return s.result(ResizeRecreate, ResizePermanent, unsupported)
	}
	pod, err := r.k8sService.GetPod(rf.Namespace, podName)
	if err != nil {
		return ResizeResult{}, err
	}
	current, update, reason, err := r.revisionTemplates(rf, pod, updateRevision)
	if err != nil {
		return ResizeResult{}, err
	}
	if reason != "" {
		// A revision that the operator cannot read can be a step.
		s.autoscaler = limits != (AutoscalerLimits{})
		return s.recreate(ResizePermanent, reason)
	}
	if message := s.keepMessage(pod, current, update, limits); message != "" {
		return s.keep(message)
	}
	var held int64
	if requested, ok := heldStepRequest(pod, update, support, limits); ok {
		// The pod requests the step since a revert. Before Kubernetes 1.35,
		// the API server refuses a lower limit in the pod spec, and the user
		// path would recreate the pod. The pod keeps the request.
		held, s.autoscaler = requested, true
	}
	if unsupported != "" {
		return s.result(ResizeRecreate, ResizePermanent, unsupported)
	}
	desired, reason, class, err := r.inPlaceResources(rf, pod, current, update, support, held)
	if err != nil {
		return ResizeResult{}, err
	}
	if reason != "" {
		return s.recreate(class, reason)
	}
	if !podRequests(pod, desired) {
		return s.requestResize(desired)
	}
	return s.followKubelet(pod, desired)
}

// podResize holds one call of ResizePodInPlace. autoscaler is the
// AutoscalerRevision of the result.
type podResize struct {
	r                       *RedisFailoverHealer
	rf                      *redisfailoverv1.RedisFailover
	podName, updateRevision string
	logger                  log.Logger
	autoscaler              bool
}

func (s *podResize) result(action ResizeAction, class ResizeClass, message string) (ResizeResult, error) {
	return ResizeResult{Action: action, Class: class, Message: message, AutoscalerRevision: s.autoscaler}, nil
}

func (s *podResize) recreate(class ResizeClass, reason string) (ResizeResult, error) {
	// The handler logs the refusal of an autoscaler revision, because it
	// repeats at each reconcile.
	if !s.autoscaler || class != ResizePermanent {
		s.logger.Infof("Recreating the pod instead of resizing it in place: %s", reason)
	}
	return s.result(ResizeRecreate, class, reason)
}

func (s *podResize) waiting(class ResizeClass, format string, args ...any) (ResizeResult, error) {
	return s.result(ResizeWaiting, class, fmt.Sprintf(format, args...))
}

// relabel moves the pod to the update revision.
func (s *podResize) relabel() error {
	if err := s.r.k8sService.UpdatePodLabels(s.rf.Namespace, s.podName, map[string]string{appsv1.ControllerRevisionHashLabelKey: s.updateRevision}); err != nil {
		return err
	}
	return s.r.ClearResizeRequested(s.rf, s.podName)
}

// keepMessage returns the log message when the pod keeps its resources and
// only moves to the update revision, or "".
func (s *podResize) keepMessage(pod *corev1.Pod, current, update *corev1.PodTemplateSpec, limits AutoscalerLimits) string {
	if limits.Step != 0 && autoscalerRevision(update, current, AutoscalerLimits{Effective: limits.Step}) || stepRevision(update, current, limits.StepMax) {
		// The StatefulSet created the pod from the template of a step, and
		// the update revision has the limit before it, for example after a
		// revert. A resize or a recreate would shrink the pod, so it only
		// moves to the update revision with its larger limit.
		return "Kept the memory limit of the autoscaler step on the pod"
	}
	s.autoscaler = autoscalerRevision(current, update, limits)
	if (pod.Labels[appsv1.ControllerRevisionHashLabelKey] == s.updateRevision || s.autoscaler) && keepsTemplate(pod, update) {
		// Only the resize request makes the pod stale, for example a request
		// of a step after its revert, or the pod already runs with the limit
		// of a step, for example after a resize from outside. The pod runs
		// with the template limit or a larger one, so it keeps its resources.
		return "Kept the memory limit of the pod, which has the resources of the update revision"
	}
	return ""
}

func (s *podResize) keep(message string) (ResizeResult, error) {
	if err := s.relabel(); err != nil {
		return ResizeResult{}, err
	}
	s.logger.Info(message)
	return s.result(ResizeDone, "", "")
}

// requestResize writes the desired resources to the pod spec.
func (s *podResize) requestResize(desired map[string]corev1.ResourceRequirements) (ResizeResult, error) {
	if err := s.r.k8sService.ResizePod(s.rf.Namespace, s.podName, desired); err != nil {
		// The same admission rejects the recreated pod, which then stays missing.
		if apierrors.IsForbidden(err) && rejectedByResourceLimits(err) {
			s.logger.Debugf("A ResourceQuota or LimitRange blocks the resize: %v", err)
			return s.waiting(ResizePermanent, "resize of pod %s is blocked by a ResourceQuota or LimitRange: %v", s.podName, err)
		}
		// Rejected by the API server, e.g. missing RBAC or a disabled feature gate.
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) {
			return s.recreate(ResizePermanent, "resize rejected: "+err.Error())
		}
		return ResizeResult{}, err
	}
	// The annotation change queues the next reconcile, so a held resize must not write it.
	if err := s.r.markResizeRequested(s.rf, s.podName); err != nil {
		return ResizeResult{}, err
	}
	s.logger.Infof("Resizing the pod in place")
	return s.waiting(ResizeTransient, "resizing pod %s in place", s.podName)
}

// followKubelet waits for the kubelet to apply the requested resources.
func (s *podResize) followKubelet(pod *corev1.Pod, desired map[string]corev1.ResourceRequirements) (ResizeResult, error) {
	if s.autoscaler && !ResizeRequested(pod) && !podApplied(pod, desired) && !infeasible(pod) {
		// The spec requests the target of the step since an earlier step
		// that the operator reverted. Without a request time, a deferred
		// resize would time out at once.
		if err := s.r.markResizeRequested(s.rf, s.podName); err != nil {
			return ResizeResult{}, err
		}
		return s.waiting(ResizeTransient, "resize of pod %s in progress", s.podName)
	}
	if c := podCondition(pod, corev1.PodResizePending); c != nil {
		if infeasible(pod) {
			return s.recreate(ResizePermanent, "resize infeasible: "+c.Message)
		}
		if sinceLatestRequest(pod, c.LastTransitionTime.Time) > inPlaceResizeTimeout {
			return s.recreate(ResizePermanent, "resize deferred for too long: "+c.Message)
		}
		return s.waiting(ResizeTransient, "resize of pod %s deferred: %s", s.podName, c.Message)
	}
	if c := podCondition(pod, corev1.PodResizeInProgress); c != nil {
		// The usage includes the page cache, so the kubelet refuses a memory
		// limit below the usage again on each retry. Only the message
		// identifies this error.
		if c.Reason == corev1.PodReasonError && (refusesDesiredMemory(pod, c, desired) || sinceLatestRequest(pod, c.LastTransitionTime.Time) > inPlaceResizeTimeout) {
			return s.recreate(ResizePermanent, "resize failed: "+c.Message)
		}
		return s.waiting(ResizeTransient, "resize of pod %s in progress", s.podName)
	}
	return s.finish(pod, desired)
}

// finish moves the pod to the update revision once the kubelet applied the
// desired resources.
func (s *podResize) finish(pod *corev1.Pod, desired map[string]corev1.ResourceRequirements) (ResizeResult, error) {
	if !podApplied(pod, desired) {
		// The kubelet may not have picked the resize up yet.
		requested, err := time.Parse(time.RFC3339, pod.Annotations[resizeRequestedAnnotation])
		if err != nil {
			if err := s.r.markResizeRequested(s.rf, s.podName); err != nil {
				return ResizeResult{}, err
			}
		} else if time.Since(requested) > inPlaceResizeTimeout {
			return s.recreate(ResizePermanent, "resize not applied by the kubelet")
		}
		return s.waiting(ResizeTransient, "resize of pod %s in progress", s.podName)
	}

	if err := s.relabel(); err != nil {
		return ResizeResult{}, err
	}
	s.logger.Infof("Resized the pod in place")
	return s.result(ResizeDone, "", "")
}

// sinceLatestRequest returns the time since t or since the latest resize
// request. A condition keeps its transition time when a newer resize
// supersedes the one it reported on.
func sinceLatestRequest(pod *corev1.Pod, t time.Time) time.Duration {
	if requested, err := time.Parse(time.RFC3339, pod.Annotations[resizeRequestedAnnotation]); err == nil && requested.After(t) {
		t = requested
	}
	return time.Since(t)
}

// inPlaceResizeSupport returns the reason that no in-place resize is
// possible, or "".
func (r *RedisFailoverHealer) inPlaceResizeSupport(rf *redisfailoverv1.RedisFailover) (string, k8s.PodResizeSupport, error) {
	if rf.Spec.Redis.InPlaceResize == "Disabled" {
		return "redis.inPlaceResize is Disabled", k8s.PodResizeSupport{}, nil
	}
	support, err := r.k8sService.PodResizeSupport()
	if err != nil {
		return "", k8s.PodResizeSupport{}, err
	}
	if !support.Supported {
		return "an in-place resize needs Kubernetes 1.33 or later", support, nil
	}
	return "", support, nil
}

// heldStepRequest returns the requested redis memory limit and true when it
// is the limit of a step above the update revision that the pod has to keep.
func heldStepRequest(pod *corev1.Pod, update *corev1.PodTemplateSpec, support k8s.PodResizeSupport, limits AutoscalerLimits) (int64, bool) {
	var ul int64
	if c := redisContainer(update); c != nil {
		ul = c.Resources.Limits.Memory().Value()
	}
	requested := RequestedRedisMemoryLimit(pod)
	return requested, !support.MemoryLimitDecrease && requested > ul && (requested == limits.Step || requested <= limits.StepMax)
}

// ClearResizeRequested removes the time of the last resize request from the
// pod. With the time, the rollout treats the pod as stale, and a later resize
// to the same resources times out at once.
func (r *RedisFailoverHealer) ClearResizeRequested(rf *redisfailoverv1.RedisFailover, podName string) error {
	return r.k8sService.UpdatePodAnnotations(rf.Namespace, podName, map[string]string{resizeRequestedAnnotation: ""})
}

// infeasible reports whether the kubelet cannot apply the resize of pod on
// its node.
func infeasible(pod *corev1.Pod) bool {
	c := podCondition(pod, corev1.PodResizePending)
	return c != nil && c.Reason == corev1.PodReasonInfeasible
}

// refusesDesiredMemory reports whether c refuses a desired memory limit as
// below the usage. A condition without an observed generation can be from a
// superseded request, so the refused limit must be a desired limit.
func refusesDesiredMemory(pod *corev1.Pod, c *corev1.PodCondition, desired map[string]corev1.ResourceRequirements) bool {
	if !strings.Contains(c.Message, memoryBelowUsage) {
		return false
	}
	if c.ObservedGeneration != 0 {
		return true
	}
	for name, d := range desired {
		if q, ok := d.Limits[corev1.ResourceMemory]; ok && strings.Contains(c.Message, fmt.Sprintf("container %q memory limit (%d) below", name, q.Value())) {
			return true
		}
	}
	limit := podMemoryLimit(pod, desired)
	return limit != 0 && strings.Contains(c.Message, fmt.Sprintf("pod memory limit (%d) below", limit))
}

// podMemoryLimit returns the pod memory limit for the desired container
// limits, or 0 for pod-level resources or a container without a memory limit.
// It copies PodLimits (k8s.io/component-helpers), because the result must
// equal the number in the kubelet message.
func podMemoryLimit(pod *corev1.Pod, desired map[string]corev1.ResourceRequirements) int64 {
	if pod.Spec.Resources != nil {
		return 0
	}
	limit := func(c corev1.Container) int64 {
		r := c.Resources
		if d, ok := desired[c.Name]; ok {
			r = d
		}
		return r.Limits.Memory().Value()
	}
	var total, sidecars, init int64
	for _, c := range pod.Spec.Containers {
		l := limit(c)
		if l == 0 {
			return 0
		}
		total += l
	}
	// An init container runs at the same time as the native sidecars before it.
	for _, c := range pod.Spec.InitContainers {
		l := limit(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			total += l
			sidecars += l
			l = sidecars
		} else {
			l += sidecars
		}
		if l == 0 {
			return 0
		}
		init = max(init, l)
	}
	return max(total, init) + pod.Spec.Overhead.Memory().Value()
}

// ResizeRequested reports whether the pod has an in-place resize that the
// kubelet did not apply yet.
func ResizeRequested(pod *corev1.Pod) bool {
	return pod.Annotations[resizeRequestedAnnotation] != ""
}

func (r *RedisFailoverHealer) markResizeRequested(rf *redisfailoverv1.RedisFailover, podName string) error {
	return r.k8sService.UpdatePodAnnotations(rf.Namespace, podName, map[string]string{resizeRequestedAnnotation: time.Now().UTC().Format(time.RFC3339)})
}

// autoscalerRevision reports whether update is a step of the memory
// autoscaler from current. effectiveRF changes only the memory limit of the
// redis container, and a memory request equal to that limit. A step only
// raises the limit to an effective limit above the spec limit. A lower limit
// holds a change of the user, for example a lower memoryMax. A raise of the
// spec below the effective limit does not change the template, so a pod from
// before it keeps the step.
func autoscalerRevision(current, update *corev1.PodTemplateSpec, limits AutoscalerLimits) bool {
	if limits.Effective == 0 {
		return false
	}
	current = current.DeepCopy()
	c, u := redisContainer(current), redisContainer(update)
	if c == nil || u == nil {
		return false
	}
	cl, cok := c.Resources.Limits[corev1.ResourceMemory]
	ul, uok := u.Resources.Limits[corev1.ResourceMemory]
	if !cok || !uok || ul.Value() != limits.Effective || ul.Value() <= limits.Spec || cl.Value() >= ul.Value() {
		return false
	}
	cr, cok := c.Resources.Requests[corev1.ResourceMemory]
	ur, uok := u.Resources.Requests[corev1.ResourceMemory]
	if cok != uok || cok && !cr.Equal(ur) && (!cr.Equal(cl) || !ur.Equal(ul)) {
		return false
	}
	c.Resources.Limits[corev1.ResourceMemory] = ul
	if cok {
		c.Resources.Requests[corev1.ResourceMemory] = ur
	}
	return equality.Semantic.DeepEqual(current, update)
}

// stepRevision reports whether current differs from update only by a larger
// redis memory limit up to stepMax, as a revision of an unknown step.
func stepRevision(update, current *corev1.PodTemplateSpec, stepMax int64) bool {
	c := redisContainer(current)
	if stepMax == 0 || c == nil {
		return false
	}
	cl := c.Resources.Limits.Memory().Value()
	return cl <= stepMax && autoscalerRevision(update, current, AutoscalerLimits{Effective: cl})
}

func redisContainer(t *corev1.PodTemplateSpec) *corev1.Container {
	for i := range t.Spec.Containers {
		if t.Spec.Containers[i].Name == redisContainerName {
			return &t.Spec.Containers[i]
		}
	}
	return nil
}

// revisionTemplates returns the templates of the pod's revision and of the
// update revision, or the reason that one is not available.
func (r *RedisFailoverHealer) revisionTemplates(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod, updateRevision string) (current, update *corev1.PodTemplateSpec, reason string, err error) {
	current, err = r.revisionTemplate(rf, pod.Labels[appsv1.ControllerRevisionHashLabelKey])
	if current == nil || err != nil {
		return nil, nil, "the pod's revision is not available", err
	}
	update, err = r.revisionTemplate(rf, updateRevision)
	if update == nil || err != nil {
		return nil, nil, "the update revision is not available", err
	}
	return current, update, "", nil
}

// inPlaceResources returns the container resources of the update revision
// when the pod can be moved to it by an in-place resize, or the reason and
// the class when it cannot. A held memory limit of the redis container stays
// in place of the limit of the update revision.
func (r *RedisFailoverHealer) inPlaceResources(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod, current, update *corev1.PodTemplateSpec, support k8s.PodResizeSupport, held int64) (map[string]corev1.ResourceRequirements, string, ResizeClass, error) {
	if !equality.Semantic.DeepEqual(withoutResources(current), withoutResources(update)) {
		return nil, "the update changes more than container resources", ResizeUserRevision, nil
	}

	old := map[string]corev1.ResourceRequirements{}
	for _, c := range current.Spec.Containers {
		old[c.Name] = withDefaultRequests(c.Resources)
	}
	actual, running, reported := podResources(pod)
	if !reported {
		// E.g. a kubelet without in-place resize support, which would leave
		// the pod spec that maxmemory derives from ahead of the container.
		return nil, "the kubelet does not report the container's resources", ResizePermanent, nil
	}
	desired := map[string]corev1.ResourceRequirements{}
	lowersMemoryLimit := false
	var loweredRedisLimit int64
	for _, c := range update.Spec.Containers {
		d := withDefaultRequests(c.Resources)
		if reason := notResizable(old[c.Name], d); reason != "" {
			return nil, reason, ResizeUserRevision, nil
		}
		// The pod may already run a superseded revision's values.
		base := actual[c.Name]
		target := inPlaceTarget(c.Name, base, d, held)
		desired[c.Name] = target
		belowRunning, belowSpec := lowersMemory(target, running[c.Name], base)
		lowersMemoryLimit = lowersMemoryLimit || belowRunning || belowSpec
		if belowRunning && c.Name == redisContainerName {
			loweredRedisLimit = target.Limits.Memory().Value()
		}
	}
	if lowersMemoryLimit && !support.MemoryLimitDecrease {
		return nil, "lowering a memory limit in place needs Kubernetes 1.35", ResizePermanent, nil
	}
	if loweredRedisLimit != 0 {
		reason, err := r.memoryInUseExceeds(rf, pod, loweredRedisLimit)
		if reason != "" || err != nil {
			return nil, reason, ResizePermanent, err
		}
	}
	return desired, "", "", nil
}

// podResources returns the resources of the pod spec, and the running
// resources that the kubelet reports, by container. The pod's own resources
// are the base: admission (e.g. a LimitRange) may have added values that
// neither revision has. reported is false when the kubelet does not report
// the resources of the redis container.
func podResources(pod *corev1.Pod) (actual, running map[string]corev1.ResourceRequirements, reported bool) {
	actual, running = map[string]corev1.ResourceRequirements{}, map[string]corev1.ResourceRequirements{}
	for _, c := range pod.Spec.Containers {
		actual[c.Name], running[c.Name] = c.Resources, c.Resources
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Resources != nil {
			running[cs.Name] = *cs.Resources
			reported = reported || cs.Name == redisContainerName
		}
	}
	return actual, running, reported
}

// notResizable returns the reason that an in-place resize cannot change the
// resources o to d, or "".
func notResizable(o, d corev1.ResourceRequirements) string {
	for _, list := range [][2]corev1.ResourceList{{o.Requests, d.Requests}, {o.Limits, d.Limits}} {
		if len(list[0]) != len(list[1]) {
			return "requests or limits are added or removed"
		}
		for name, q := range list[0] {
			n, ok := list[1][name]
			if !ok {
				return "requests or limits are added or removed"
			}
			if name != corev1.ResourceCPU && name != corev1.ResourceMemory && !q.Equal(n) {
				return "only cpu and memory can be resized in place"
			}
		}
	}
	return ""
}

// inPlaceTarget returns base with the cpu and memory of d. With a held
// limit, the redis container keeps the memory of base.
func inPlaceTarget(container string, base, d corev1.ResourceRequirements, held int64) corev1.ResourceRequirements {
	target := *base.DeepCopy()
	target.Requests = overlay(target.Requests, d.Requests)
	target.Limits = overlay(target.Limits, d.Limits)
	if container == redisContainerName && held != 0 {
		target.Limits[corev1.ResourceMemory] = base.Limits[corev1.ResourceMemory]
		if q, ok := base.Requests[corev1.ResourceMemory]; ok {
			target.Requests[corev1.ResourceMemory] = q
		}
	}
	return target
}

// lowersMemory reports whether the memory limit of target is below the
// running limit, and below the limit of the pod spec base. Before Kubernetes
// 1.35, the API server also refuses a limit below the pod spec, also when the
// container runs with less.
func lowersMemory(target, running, base corev1.ResourceRequirements) (belowRunning, belowSpec bool) {
	if q, ok := running.Limits[corev1.ResourceMemory]; ok && target.Limits.Memory().Cmp(q) < 0 {
		belowRunning = true
	}
	if q, ok := base.Limits[corev1.ResourceMemory]; ok && target.Limits.Memory().Cmp(q) < 0 {
		belowSpec = true
	}
	return belowRunning, belowSpec
}

// memoryInUseExceeds returns the reason to recreate the pod when the memory
// that Redis holds does not fit the lowered limit. Redis often keeps the memory
// that evictions free, and a fork for BGSAVE or a full sync needs more. A
// recreated pod loads compacted data.
func (r *RedisFailoverHealer) memoryInUseExceeds(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod, limit int64) (string, error) {
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return "", err
	}
	info, err := r.redisClient.GetMemoryInfo(pod.Status.PodIP, getRedisPort(rf.Spec.Redis.Port), password)
	if err != nil {
		return "", err
	}
	if info.UsedMemoryRSS+redisfailoverv1.MaxMemoryReserve > limit {
		return fmt.Sprintf("the Redis memory in use (%s) leaves less than %dMi below the lowered limit %s",
			formatBytes(info.UsedMemoryRSS), redisfailoverv1.MaxMemoryReserve>>20, formatBytes(limit)), nil
	}
	return "", nil
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
		return nil, fmt.Errorf("decode controllerrevision %s/%s: %w", rf.Namespace, revision, err)
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

// keepsTemplate reports whether the pod spec and the applied resources have
// the resources of the template t, with a redis memory limit at or above
// the one of t.
func keepsTemplate(pod *corev1.Pod, t *corev1.PodTemplateSpec) bool {
	spec, applied := map[string]*corev1.ResourceRequirements{}, map[string]*corev1.ResourceRequirements{}
	for i := range pod.Spec.Containers {
		spec[pod.Spec.Containers[i].Name] = &pod.Spec.Containers[i].Resources
	}
	for _, cs := range pod.Status.ContainerStatuses {
		applied[cs.Name] = cs.Resources
	}
	for _, c := range t.Spec.Containers {
		for _, got := range []*corev1.ResourceRequirements{spec[c.Name], applied[c.Name]} {
			if !keepsResources(got, c) {
				return false
			}
		}
	}
	return true
}

// keepsResources reports whether got has the resources of the template
// container c, with a redis memory limit at or above the one of c.
func keepsResources(got *corev1.ResourceRequirements, c corev1.Container) bool {
	if got == nil {
		return false
	}
	want := withDefaultRequests(c.Resources)
	if q, ok := want.Limits[corev1.ResourceMemory]; ok && c.Name == redisContainerName {
		if got.Limits.Memory().Cmp(q) < 0 {
			return false
		}
		want.Limits[corev1.ResourceMemory] = got.Limits[corev1.ResourceMemory]
		if r, ok := got.Requests[corev1.ResourceMemory]; ok && want.Requests.Memory().Equal(q) {
			want.Requests[corev1.ResourceMemory] = r
		}
	}
	return sameResizable(*got, want)
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
