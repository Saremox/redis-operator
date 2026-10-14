package redisfailover

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

// errForeignState is the error for a state ConfigMap that another object
// controls. The autoscaler does not change or delete that ConfigMap.
var errForeignState = errors.New("the state ConfigMap is not controlled by the RedisFailover")

// autoscaleSample is the last sample of a RedisFailover in memory. The
// persisted LastSample is written only together with other changes.
type autoscaleSample struct {
	at    time.Time
	ratio float64
	// state is the state after the sample, when sampled is true. It
	// continues the ConfigMap with the resourceVersion version ("" for no
	// ConfigMap). It is newer than that ConfigMap when the write failed or was
	// not necessary.
	state   autoscale.State
	version string
	sampled bool
}

// autoscaleRead is the state that Handle reads before Ensure. Ensure and the
// checks then use the effective memory limit, and the autoscaler uses the
// same state without a second read.
type autoscaleRead struct {
	state autoscale.State
	cm    *corev1.ConfigMap
	// blocked is the reason for a state of another owner or of a newer
	// operator, and err tells why. The autoscaler does not use or write such
	// a state.
	blocked string
	err     error
	// resumed tells that state took its Effective from the StatefulSet, and
	// that the autoscaler must write it.
	resumed bool
	// spec is the memory limit of redis.resources, and effective the memory
	// limit of the pods.
	spec, effective int64
}

// readAutoscale reads the state and resolves the effective memory limit. A
// state without a limit freezes it at the limit of the live StatefulSet,
// because the spec limit can shrink the pods.
func (r *RedisFailoverHandler) readAutoscale(rf *redisfailoverv1.RedisFailover) (autoscaleRead, error) {
	spec := rf.Spec.Redis.Resources.Limits.Memory().Value()
	maxLimit := rf.Spec.Redis.Autoscaling.MemoryMax.Value()
	read := autoscaleRead{spec: spec, effective: spec}
	var err error
	read.state, read.cm, err = r.readAutoscaleState(rf)
	switch {
	case errors.Is(err, errForeignState):
		read.blocked, read.err = autoscale.ReasonForeignState, err
	case err != nil:
		return read, err
	case read.state.Version > autoscale.StateVersion:
		read.blocked = autoscale.ReasonNewerState
		read.err = fmt.Errorf("the state ConfigMap has version %d, this operator reads version %d", read.state.Version, autoscale.StateVersion)
	case read.state.Version != 0:
		if err := r.holdUnappliedRevert(rf, &read, spec, maxLimit); err != nil {
			return read, err
		}
		// No operator sampled an expired state for MaxStateAge. Its Effective
		// can be a leftover of an earlier mode Auto, or of a step whose
		// template never reached the pods. Only the template shows the limit
		// of the pods. The template wins, because a leftover would resize the
		// pods without a decision, and a lower Effective would shrink them.
		if read.state.Effective != 0 && r.autoscaleStateExpired(rf, read) {
			live, err := r.liveRedisMemoryLimit(rf, spec)
			if err != nil {
				return read, err
			}
			read.state = autoscale.Resume(read.state, live, spec, maxLimit)
			read.resumed = true
		}
		read.effective = autoscale.EffectiveLimit(read.state, spec, maxLimit)
		return read, nil
	}
	// In mode Recommend, only an effective limit in the status shows that a
	// step of an earlier mode Auto changed the pods. Thus an instance without
	// a step makes no extra call.
	// A status without it, for example after a restore, gets one read after
	// each operator start.
	_, looked := r.autoscaleLiveRead.Load(failoverKey(rf))
	if rf.Spec.Redis.Autoscaling.Mode != redisfailoverv1.AutoscalingModeAuto && rf.Status.Autoscaling.EffectiveMemory == "" && looked {
		return read, nil
	}
	live, err := r.liveRedisMemoryLimit(rf, spec)
	if err != nil {
		return read, err
	}
	r.autoscaleLiveRead.Store(failoverKey(rf), true)
	read.effective = max(spec, min(live, maxLimit))
	return read, nil
}

// holdUnappliedRevert checks a Reverted state that Ensure did not apply. The
// StatefulSet can have created a pod from the template of the step since the
// revert, for example while the operator was down. Ensure would then make
// that pod stale and larger than the template, so the step is held.
func (r *RedisFailoverHandler) holdUnappliedRevert(rf *redisfailoverv1.RedisFailover, read *autoscaleRead, spec, maxLimit int64) error {
	s := read.state
	if s.Phase != autoscale.PhaseReverted || s.RevertApplied || s.To <= autoscale.EffectiveLimit(s, spec, maxLimit) {
		return nil
	}
	// A missing pod has no data of the step. Step of autoscalerLimits keeps a
	// pod that the StatefulSet creates later from the template of the step.
	// A hold for it would wait for a pod that a quota can refuse.
	applied, _, err := r.stepPodsApplied(rf, s.To, false)
	if err != nil || !applied {
		return err
	}
	s.Phase, s.Refusal, s.Effective = autoscale.PhaseHeld, autoscale.RefusalStepPod, s.To
	read.state, read.resumed = autoscale.Bound(s, spec, maxLimit), true
	return nil
}

// autoscaleStateExpired reports whether the last sample of the state, or of
// the same state in memory, is older than MaxStateAge.
func (r *RedisFailoverHandler) autoscaleStateExpired(rf *redisfailoverv1.RedisFailover, read autoscaleRead) bool {
	last := read.state.LastSample
	v, _ := r.autoscaleSamples.Load(failoverKey(rf))
	if sample, _ := v.(autoscaleSample); sample.sampled && sample.version == read.cm.ResourceVersion {
		last = max(last, sample.state.LastSample)
	}
	return last != 0 && r.now().Unix()-last > int64(autoscale.MaxStateAge/time.Second)
}

// liveRedisMemoryLimit returns the memory limit of the redis container in the
// live StatefulSet, or spec when the StatefulSet does not exist.
func (r *RedisFailoverHandler) liveRedisMemoryLimit(rf *redisfailoverv1.RedisFailover, spec int64) (int64, error) {
	ss, err := r.k8sservice.GetStatefulSet(rf.Namespace, rfservice.GetRedisName(rf))
	if apierrors.IsNotFound(err) {
		return spec, nil
	}
	if err != nil {
		return 0, err
	}
	return rfservice.TemplateRedisMemoryLimit(ss), nil
}

// autoscalerLimits returns the memory limits of this reconcile while the
// autoscaler makes steps, when the effective limit differs from the spec
// limit. Only then a revision can be a step of the autoscaler. Otherwise, a
// frozen effective limit makes no step, so each revision takes the user path.
func (r *RedisFailoverHandler) autoscalerLimits(rf *redisfailoverv1.RedisFailover) rfservice.AutoscalerLimits {
	if !rf.AutoscalingEnabled() {
		return rfservice.AutoscalerLimits{}
	}
	v, _ := r.autoscaleReads.Load(failoverKey(rf))
	read, ok := v.(autoscaleRead)
	if !ok {
		return rfservice.AutoscalerLimits{}
	}
	var limits rfservice.AutoscalerLimits
	if r.autoscaleStepping(rf) && read.effective != read.spec {
		limits.Spec, limits.Effective = read.spec, read.effective
	}
	// The last step keeps the pods of its template in each phase and mode,
	// also after the back-off of a revert. Only the limits end it: a user
	// shrinks such a pod with a memoryMax below its limit, or with mode Off.
	if s := read.state; s.To > read.spec && s.To <= rf.Spec.Redis.Autoscaling.MemoryMax.Value() {
		limits.Step = s.To
	}
	return limits
}

// autoscaleStepping reports whether the autoscaler makes steps in this
// reconcile. Mode Auto makes steps. Mode Recommend completes a step of an
// earlier mode Auto, because the user path would recreate its pods.
func (r *RedisFailoverHandler) autoscaleStepping(rf *redisfailoverv1.RedisFailover) bool {
	switch {
	case !rf.AutoscalingEnabled():
		return false
	case rf.Spec.Redis.Autoscaling.Mode == redisfailoverv1.AutoscalingModeAuto:
		return true
	}
	v, _ := r.autoscaleReads.Load(failoverKey(rf))
	read, _ := v.(autoscaleRead)
	return stepUnfinished(read.state)
}

// stepUnfinished reports whether the template of a step can be ahead of
// the pods.
func stepUnfinished(s autoscale.State) bool {
	return s.Phase == autoscale.PhaseApplying || s.Phase == autoscale.PhaseHeld
}

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
		pod := &pods.Items[i]
		updated := pod.Labels[appsv1.ControllerRevisionHashLabelKey] == ss.Status.UpdateRevision
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
	r.autoscaleRollout.Store(key, rollout)
	return nil
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

// autoscale runs the memory autoscaler after the checks and sets
// rf.Status.Autoscaling. The Recommend mode only reports, so an error is
// logged and never fails the reconcile or makes the RedisFailover NotHealthy.
func (r *RedisFailoverHandler) autoscale(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus) {
	key := failoverKey(rf)
	v, ok := r.autoscaleReads.LoadAndDelete(key)
	if !rf.AutoscalingEnabled() {
		// Without an earlier status, Handle removed the state. This keeps the
		// API calls of the checks of an instance without autoscaling.
		if observed != (redisfailoverv1.AutoscalingStatus{}) {
			r.stopAutoscaler(rf, observed)
		}
		return
	}
	r.autoscaleOff.Delete(key)

	master, _ := r.autoscaleMasters.LoadAndDelete(key)
	read, _ := v.(autoscaleRead)
	if !ok {
		// CheckAndHeal ran without Handle.
		var err error
		if read, err = r.readAutoscale(rf); err != nil {
			rf.Status.Autoscaling = observed
			r.logAutoscale(rf, err)
			return
		}
	}
	// The autoscaler cannot use a state of another owner or of a newer
	// operator, and it does not overwrite it. No metric value is known.
	if read.blocked != "" {
		rf.Status.Autoscaling = autoscale.ReportStep(redisfailoverv1.AutoscalingStatus{Reason: read.blocked}, autoscale.State{}, read.spec, read.effective)
		r.mClient.DeleteAutoscaler(rf.Namespace, rf.Name)
		r.logAutoscale(rf, read.err)
		return
	}
	stored, cm := read.state, read.cm
	limit := read.effective
	maxLimit := rf.Spec.Redis.Autoscaling.MemoryMax.Value()
	now := r.now()
	sample, _ := r.autoscaleSamples.Load(key)
	last, found := sample.(autoscaleSample)
	if !found {
		last.ratio = stored.Window.PeakRatio
	}
	// While the writes fail, each sample must continue from the last sample
	// and not from the stored state, or the status changes at each reconcile.
	// A write of another operator changes the resourceVersion, and then the
	// ConfigMap wins. The sample times cannot tell this, because the clock
	// can go back.
	version := ""
	if cm != nil {
		version = cm.ResourceVersion
	}
	state := stored
	if last.sampled && last.version == version && !read.resumed {
		state = last.state
	}
	unbound := state
	if state.Version == 0 && limit != read.spec {
		// The write of the state keeps the limit that readAutoscale froze, and
		// no step starts from it.
		state.Version, state.Effective, state.SpecLimit = autoscale.StateVersion, limit, read.spec
		state.Frozen, state.Generation = true, rf.Generation
	}
	if state.Frozen && state.Generation != rf.Generation {
		// A change of the spec ends the freeze in each mode. Plan runs only
		// in mode Auto.
		state.Frozen = false
	}
	state = autoscale.Bound(state, read.spec, maxLimit)
	if ok && read.state.Phase == autoscale.PhaseReverted && !read.state.RevertApplied && state.Phase == autoscale.PhaseReverted {
		// Ensure of this reconcile applied the limit before the step.
		state.RevertApplied = true
	}
	// The next reconcile gives the pods the limit of the stored state, so a
	// change of the limit cannot wait for the next sample.
	write := state != unbound || read.resumed
	// A leftover state, for example of a mode change to Off that the status
	// did not show, must not give a recommendation of an earlier time.
	state = autoscale.Expire(state, now)
	var errs []error
	// The persisted time limits the samples after an operator restart. It is
	// written only with other changes, so the time in memory is necessary too.
	if sampleDue(now, time.Unix(stored.LastSample, 0)) && sampleDue(now, last.at) {
		in := autoscale.Input{
			Now:      now,
			Healthy:  rf.Status.State == redisfailoverv1.HealthyState,
			Limit:    limit,
			Max:      maxLimit,
			Workload: rf.Spec.Redis.Autoscaling.Workload,
			Policy:   rf.MaxMemoryPolicy(),
		}
		if ip, _ := master.(string); in.Healthy && ip != "" {
			info, err := r.rfChecker.GetAutoscalerInfo(rf, ip)
			if err != nil {
				errs = append(errs, fmt.Errorf("no autoscaler sample of %s: %w", ip, err))
			}
			in.Info = info
		}
		state = autoscale.Observe(state, in)
		last.at, last.state, last.version, last.sampled = now, state, version, true
		if in.Info != nil {
			last.ratio = autoscale.UsedRatio(in.Info, limit)
		}
		r.autoscaleSamples.Store(key, last)
		// A write changes the resourceVersion, so the next reconcile reads
		// this state from the ConfigMap.
		write = write || cm == nil || state.NeedsWrite(stored)
	}

	next, action, wait := state, autoscale.ActionNone, ""
	if rf.Spec.Redis.Autoscaling.Mode == redisfailoverv1.AutoscalingModeAuto || stepUnfinished(state) {
		var err error
		next, action, wait, err = r.planStep(rf, read, state, now)
		errs = append(errs, err)
	}
	if write || action != autoscale.ActionNone {
		if err := r.writeAutoscaleState(rf, cm, next); err != nil {
			errs = append(errs, err)
			// The next Ensure derives the template from the stored state, so
			// a transition without its write did not happen.
			next, action = state, autoscale.ActionNone
		}
	}
	r.autoscaleStepWritten(rf, next, action)

	shown := shownStep(rf, next)
	report := autoscale.Report(next, limit, maxLimit)
	if wait != "" && report.RecommendedMemory != "" {
		report.Reason = wait
	}
	if refused := r.refusalReason(rf, shown); refused != "" {
		report.Reason = refused
	}
	rf.Status.Autoscaling = autoscale.ReportStep(report, shown, read.spec, limit)
	var recommended int64
	if rf.Status.Autoscaling.RecommendedMemory != "" {
		recommended = min(next.Recommended, maxLimit)
	}
	r.mClient.SetAutoscaler(rf.Namespace, rf.Name, recommended, last.ratio, next.HighWindows, limit)
	r.mClient.SetAutoscalerPhase(rf.Namespace, rf.Name, phaseValue(shown.Phase))
	r.logAutoscale(rf, errors.Join(errs...))
}

// shownStep returns the step fields of s for the status. Mode Recommend
// makes no step, so it shows only a step of an earlier mode Auto that it
// completes. The effective limit stays.
func shownStep(rf *redisfailoverv1.RedisFailover, s autoscale.State) autoscale.State {
	if rf.Spec.Redis.Autoscaling.Mode == redisfailoverv1.AutoscalingModeAuto || stepUnfinished(s) {
		return s
	}
	s.Phase, s.Refusal, s.StartedAt, s.RetryAt = "", "", 0, 0
	return s
}

// planStep runs the step lifecycle of state on the rollout of this reconcile.
// It returns the next state, the transition, and the reason that a
// recommended step does not start.
func (r *RedisFailoverHandler) planStep(rf *redisfailoverv1.RedisFailover, read autoscaleRead, state autoscale.State, now time.Time) (autoscale.State, autoscale.Action, string, error) {
	auto := rf.Spec.Redis.Autoscaling.Mode == redisfailoverv1.AutoscalingModeAuto
	v, _ := r.autoscaleRollout.Load(failoverKey(rf))
	rollout, _ := v.(autoscaleRollout)
	o, requested, listErr := r.stepObservation(rf, read, state, rollout, now)
	next, action := autoscale.Plan(state, o)
	var wait string
	var err error
	if action == autoscale.ActionStart {
		if wait, err = r.stepBlocked(rf, rollout); wait != "" {
			o.Settled = false
			next, action = autoscale.Plan(state, o)
		}
	}
	if action == autoscale.ActionRevert {
		// With the annotation, a later step to the same limit on a pod that
		// still requests it times out at once.
		if clearErr := r.clearResizeRequests(rf, requested); clearErr != nil {
			// A short API error gets one more ApplyTimeout. A revert that never
			// ends would keep the step in Applying.
			if now.Unix()-state.StartedAt < int64(2*autoscale.ApplyTimeout/time.Second) {
				return state, autoscale.ActionNone, "", fmt.Errorf("the revert of the autoscaler step waits: %w", clearErr)
			}
			state.Phase, state.Refusal = autoscale.PhaseHeld, autoscale.RefusalClear
			return state, autoscale.ActionHold, "", fmt.Errorf("the autoscaler holds the step: %w", clearErr)
		}
	}
	if auto && wait == "" {
		wait = autoscale.WaitReason(next, o)
	}
	return next, action, wait, errors.Join(listErr, err)
}

// stepObservation returns the observation of Plan, and the pods with a
// resize request for a revert.
func (r *RedisFailoverHandler) stepObservation(rf *redisfailoverv1.RedisFailover, read autoscaleRead, state autoscale.State, rollout autoscaleRollout, now time.Time) (autoscale.Observation, []string, error) {
	o := autoscale.Observation{Now: now, Spec: read.spec, Max: rf.Spec.Redis.Autoscaling.MemoryMax.Value(), Generation: rf.Generation}
	if rollout.complete {
		o.Settled = rf.Spec.Redis.Autoscaling.Mode == redisfailoverv1.AutoscalingModeAuto && rollout.settled && rf.Status.State == redisfailoverv1.HealthyState
		o.Applied, o.AnyApplied = rollout.applied, rollout.anyApplied
		if rollout.refusal != nil {
			o.Refusal = rollout.refusal.String()
		} else {
			// The next refusal is a change, also when its text is the same.
			r.autoscaleRefusalLogged.Delete(failoverKey(rf))
		}
		return o, rollout.requested, nil
	}
	// Without a complete rollout result, for example after a HoldRollout, a
	// lost quorum or a missing pod, only the timeout acts. A pod can have
	// applied the step, so only a list of the pods allows a revert.
	if state.Phase != autoscale.PhaseApplying || now.Unix()-state.StartedAt < int64(autoscale.ApplyTimeout/time.Second) {
		return o, nil, nil
	}
	applied, requested, err := r.stepPodsApplied(rf, state.To, true)
	o.AnyApplied = applied
	return o, requested, err
}

// clearResizeRequests removes the resize request of each pod. A pod that is
// gone has no request.
func (r *RedisFailoverHandler) clearResizeRequests(rf *redisfailoverv1.RedisFailover, pods []string) error {
	for _, pod := range pods {
		if err := r.rfHealer.ClearResizeRequested(rf, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// refusalReason returns the status reason of a refusal of this reconcile, or
// "". Held and Reverted show the refusal of the step. A refusal in another
// phase holds the rollout of a change of the spec.
func (r *RedisFailoverHandler) refusalReason(rf *redisfailoverv1.RedisFailover, s autoscale.State) string {
	v, _ := r.autoscaleRollout.Load(failoverKey(rf))
	rollout, _ := v.(autoscaleRollout)
	if rollout.refusal == nil || s.Phase == autoscale.PhaseHeld || s.Phase == autoscale.PhaseReverted {
		return ""
	}
	return autoscale.ReasonResizeRefused + ": " + rollout.refusal.String()
}

// stepBlocked returns why a step must not start, or "". A step changes the
// pods in place only, and maxmemory must follow the limit.
func (r *RedisFailoverHandler) stepBlocked(rf *redisfailoverv1.RedisFailover, rollout autoscaleRollout) (string, error) {
	switch {
	case rf.Spec.Redis.InPlaceResize == "Disabled":
		return autoscale.ReasonInPlaceDisabled, nil
	case rf.Spec.Redis.MaxMemory == nil || rf.ManagedMaxMemoryError() != nil:
		return autoscale.ReasonNoManagedMaxMemory, nil
	case rf.CustomConfigSets("maxmemory"):
		return autoscale.ReasonCustomMaxMemory, nil
	case rollout.vpa:
		return autoscale.ReasonVPA, nil
	case rollout.restartOnResize:
		return autoscale.ReasonRestartOnResize, nil
	}
	support, err := r.k8sservice.PodResizeSupport()
	if err != nil {
		return autoscale.ReasonPreconditionUnknown, err
	}
	if !support.Supported {
		return autoscale.ReasonNoInPlaceResize, nil
	}
	// Without the revisions, the rollout cannot tell a step from a change of
	// the user.
	if _, err := r.k8sservice.GetControllerRevision(rf.Namespace, rollout.updateRevision); err != nil {
		if apierrors.IsForbidden(err) {
			return autoscale.ReasonNoRevisionAccess, nil
		}
		return autoscale.ReasonPreconditionUnknown, err
	}
	return "", nil
}

// autoscaleStepWritten logs and counts a transition that the state write
// stored. Ensure applies a changed Effective in the next reconcile, and the
// controller does not watch the state ConfigMap.
func (r *RedisFailoverHandler) autoscaleStepWritten(rf *redisfailoverv1.RedisFailover, s autoscale.State, action autoscale.Action) {
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	switch action {
	case autoscale.ActionNone:
		return
	case autoscale.ActionStart:
		logger.Infof("Autoscaler: a step of the memory limit from %s to %s starts", quantity(s.From), quantity(s.To))
	case autoscale.ActionDone:
		logger.Infof("Autoscaler: the step to %s is done", quantity(s.To))
		r.mClient.CountAutoscalerStep(rf.Namespace, rf.Name, metrics.AutoscalerStepDone)
	case autoscale.ActionRevert:
		logger.Warningf("Autoscaler: the operator reverted the step to %s, back to %s: %s", quantity(s.To), quantity(s.From), s.Refusal)
		r.mClient.CountAutoscalerStep(rf.Namespace, rf.Name, metrics.AutoscalerStepReverted)
	case autoscale.ActionHold:
		logger.Warningf("Autoscaler: the operator holds the step to %s: %s", quantity(s.To), s.Refusal)
		r.mClient.CountAutoscalerStep(rf.Namespace, rf.Name, metrics.AutoscalerStepHeld)
	}
	if (action == autoscale.ActionStart || action == autoscale.ActionRevert) && r.requeue != nil {
		r.requeue(failoverKey(rf), 0)
	}
}

func quantity(bytes int64) string {
	return resource.NewQuantity(bytes, resource.BinarySI).String()
}

// phaseValue is the value of the autoscaler_step_phase metric. An unknown
// phase starts no step, as Held.
func phaseValue(phase string) int {
	switch phase {
	case "", autoscale.PhaseIdle:
		return 0
	case autoscale.PhaseApplying:
		return 1
	case autoscale.PhaseDone:
		return 2
	case autoscale.PhaseReverted:
		return 3
	}
	return 4
}

// sampleDue treats a sample time in the future as elapsed. After the clock
// went back, the autoscaler must not stop until the clock reaches that time.
func sampleDue(now, last time.Time) bool {
	elapsed := now.Sub(last)
	return elapsed < 0 || elapsed >= autoscale.SampleInterval
}

// readAutoscaleState returns the state and its ConfigMap, or a nil ConfigMap
// when it does not exist.
func (r *RedisFailoverHandler) readAutoscaleState(rf *redisfailoverv1.RedisFailover) (autoscale.State, *corev1.ConfigMap, error) {
	cm, err := r.getAutoscaleConfigMap(rf)
	if err != nil || cm == nil {
		return autoscale.State{}, nil, err
	}
	var state autoscale.State
	if err := json.Unmarshal([]byte(cm.Data[rfservice.AutoscalerStateKey]), &state); err != nil {
		// The next write replaces the content.
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Warningf("Autoscaler state in configmap %s is not valid, the autoscaler starts again: %s", cm.Name, err.Error())
		return autoscale.State{}, cm, nil
	}
	return state, cm, nil
}

// getAutoscaleConfigMap returns nil when the ConfigMap does not exist.
func (r *RedisFailoverHandler) getAutoscaleConfigMap(rf *redisfailoverv1.RedisFailover) (*corev1.ConfigMap, error) {
	name := rfservice.GetAutoscalerStateName(rf)
	cm, err := r.k8sservice.GetConfigMap(rf.Namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(cm, rf) {
		return nil, errForeignState
	}
	return cm, nil
}

func (r *RedisFailoverHandler) writeAutoscaleState(rf *redisfailoverv1.RedisFailover, stored *corev1.ConfigMap, state autoscale.State) error {
	// State has only strings and finite numbers, so Marshal cannot fail.
	data, _ := json.Marshal(state)
	cm := rfservice.GenerateAutoscalerStateConfigMap(rf, r.getLabels(rf), r.createOwnerReferences(rf), string(data))
	if stored == nil {
		return r.k8sservice.CreateConfigMap(rf.Namespace, cm)
	}
	// With the read resource version, the update fails when another writer
	// changed the state after the read.
	cm.ResourceVersion = stored.ResourceVersion
	return r.k8sservice.UpdateConfigMap(rf.Namespace, cm)
}

// stopAutoscaler removes the state, the metrics and the status after a change
// of the mode to Off. On an error, the status stays, so the next reconcile
// tries again.
func (r *RedisFailoverHandler) stopAutoscaler(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus) {
	cm, err := r.getAutoscaleConfigMap(rf)
	if errors.Is(err, errForeignState) {
		// Not a state of this RedisFailover, so there is nothing to delete.
		err = nil
	}
	if err == nil && cm != nil {
		err = r.k8sservice.DeleteConfigMap(rf.Namespace, cm.Name)
		if apierrors.IsNotFound(err) {
			err = nil
		}
	}
	if err != nil {
		rf.Status.Autoscaling = observed
		r.logAutoscale(rf, err)
		return
	}
	if cm != nil || observed != (redisfailoverv1.AutoscalingStatus{}) {
		r.mClient.DeleteAutoscaler(rf.Namespace, rf.Name)
	}
	r.forgetAutoscaler(failoverKey(rf))
	r.autoscaleOff.Store(failoverKey(rf), true)
}

func (r *RedisFailoverHandler) forgetAutoscaler(key string) {
	r.autoscaleReads.Delete(key)
	r.autoscaleMasters.Delete(key)
	r.autoscaleRollout.Delete(key)
	r.autoscaleSamples.Delete(key)
	r.autoscaleLogged.Delete(key)
	r.autoscaleRefusalLogged.Delete(key)
	r.autoscaleOff.Delete(key)
	r.autoscaleLiveRead.Delete(key)
	r.autoscaleClearLogged.Delete(key)
}

// logAutoscale logs an error at Warning when it differs from the last one,
// and at Debug otherwise.
func (r *RedisFailoverHandler) logAutoscale(rf *redisfailoverv1.RedisFailover, err error) {
	key := failoverKey(rf)
	if err == nil {
		r.autoscaleLogged.Delete(key)
		return
	}
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	if last, _ := r.autoscaleLogged.Swap(key, err.Error()); last == err.Error() {
		logger.Debugf("Autoscaler: %s", err.Error())
		return
	}
	logger.Warningf("Autoscaler: %s", err.Error())
}
