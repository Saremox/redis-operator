package redisfailover

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
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
	// lost tells that a state without a version replaces a lost or invalid
	// state.
	lost bool
	// userLowered tells that the user lowered the spec limit while the
	// template had the earlier spec limit.
	userLowered bool
	// spec is the memory limit of redis.resources, and effective the memory
	// limit of the pods.
	spec, effective int64
}

// readAutoscale reads the state and resolves the effective memory limit.
//
// The live limit is a floor: the limit of the StatefulSet template, or of the
// largest redis pod when the StatefulSet is missing. Only a lower spec limit
// than the one of the last Ensure, when the template had that limit, or the
// revert of the step of the template, gives a lower limit. A state below the
// floor is lost, for example after a restore of an older ConfigMap, because
// its limit would shrink the pods.
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
	}
	live, written, err := r.liveRedisMemoryLimit(rf, spec)
	if err != nil {
		return read, err
	}
	// A limit above the last spec limit is a step, and a lower spec limit
	// does not change it.
	read.userLowered = spec < written && live <= written
	if read.blocked == "" && read.state.Version != 0 {
		if err := r.holdUnappliedRevert(rf, &read, spec, maxLimit); err != nil {
			return read, err
		}
		// No operator sampled an expired state for MaxStateAge. Its Effective
		// can be a leftover of an earlier mode Auto, or of a step whose
		// template never reached the pods. A leftover would resize the pods
		// without a decision, so the template wins.
		if read.state.Effective != 0 && r.autoscaleStateExpired(rf, read) {
			read.state = autoscale.Resume(read.state, live, spec, maxLimit)
			read.resumed = true
		}
		read.effective = autoscale.EffectiveLimit(read.state, spec, maxLimit)
	}
	floor := max(spec, min(live, maxLimit))
	if read.effective < floor && !read.userLowered && !ownRevert(read.state, live) {
		if read.blocked == "" {
			read.state, read.resumed = autoscale.State{}, false
		}
		read.effective = floor
	}
	read.lost = stateLost(read, rf.Status.Autoscaling)
	return read, nil
}

// ownRevert reports whether s reverts the step of the template limit live,
// and Ensure did not apply that revert.
func ownRevert(s autoscale.State, live int64) bool {
	return s.Phase == autoscale.PhaseReverted && !s.RevertApplied && s.To == live
}

// stateLost reports whether a state without a version replaces a lost,
// invalid or outdated state. A ConfigMap, a phase or an effective limit in
// the status shows that the autoscaler ran before.
func stateLost(read autoscaleRead, status redisfailoverv1.AutoscalingStatus) bool {
	return read.blocked == "" && read.state.Version == 0 && (read.cm != nil || status.Phase != "" || status.EffectiveMemory != "")
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
// live StatefulSet, and the spec limit of the last Ensure, or 0. Without the
// StatefulSet, for example after a delete with --cascade=orphan, the largest
// limit of the redis pods counts, or spec without pods.
func (r *RedisFailoverHandler) liveRedisMemoryLimit(rf *redisfailoverv1.RedisFailover, spec int64) (int64, int64, error) {
	ss, err := r.k8sservice.GetStatefulSet(rf.Namespace, rfservice.GetRedisName(rf))
	if apierrors.IsNotFound(err) {
		pods, err := r.k8sservice.ListPods(rf.Namespace)
		if err != nil {
			return 0, 0, err
		}
		live := spec
		for i := range pods.Items {
			if pod := &pods.Items[i]; rfservice.IsRedisPod(rf, pod) {
				live = max(live, rfservice.AppliedRedisMemoryLimit(pod))
			}
		}
		return live, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	written, err := resource.ParseQuantity(ss.Annotations[rfservice.SpecMemoryLimitAnnotation])
	if err != nil {
		return rfservice.TemplateRedisMemoryLimit(ss), 0, nil
	}
	return rfservice.TemplateRedisMemoryLimit(ss), written.Value(), nil
}

// autoscalerLimits returns the memory limits of this reconcile in each mode
// with autoscaling. A revision to the effective limit can come from a step,
// also after a mode change or the loss of the state. The user path would
// recreate a pod that refuses it.
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
	switch s := read.state; {
	case stepUnfinished(s) && s.To == read.spec:
		// A spec limit at the target of the step does not change the
		// template. The revision of the step stays a step.
		limits.Spec, limits.Effective = s.From, s.To
	case read.blocked != "" || read.lost || s.Frozen:
		// Without a known state, each raise to the limit of the StatefulSet
		// can be a step, also at the spec limit, and each larger pod can be
		// a pod of a step. A freeze ends at the next change of the spec.
		limits.Effective = read.effective
		if !read.userLowered {
			limits.StepMax = rf.Spec.Redis.Autoscaling.MemoryMax.Value()
		}
	case read.effective != read.spec:
		limits.Spec, limits.Effective = read.spec, read.effective
	}
	// The last step keeps the pods of its template in each phase and mode,
	// also after the back-off of a revert. A memoryMax below its limit, a
	// spec limit at or above it (Bound), and mode Off end it.
	if s := read.state; s.To > read.spec && s.To <= rf.Spec.Redis.Autoscaling.MemoryMax.Value() {
		limits.Step = s.To
	}
	return limits
}

// autoscaleStepping reports whether the autoscaler makes steps in this
// reconcile. Mode Auto makes steps. Mode Recommend completes a step of an
// earlier mode Auto.
func (r *RedisFailoverHandler) autoscaleStepping(rf *redisfailoverv1.RedisFailover) bool {
	switch {
	case !rf.AutoscalingEnabled():
		return false
	case rf.Spec.Redis.Autoscaling.Mode == redisfailoverv1.AutoscalingModeAuto:
		return true
	}
	v, _ := r.autoscaleReads.Load(failoverKey(rf))
	read, _ := v.(autoscaleRead)
	// A change of the spec turns a frozen limit into a held step, which ends
	// at once when each pod runs with the limit.
	return stepUnfinished(read.state) || read.state.Frozen && read.state.Generation != rf.Generation
}

// stepUnfinished reports whether the template of a step can be ahead of
// the pods.
func stepUnfinished(s autoscale.State) bool {
	return s.Phase == autoscale.PhaseApplying || s.Phase == autoscale.PhaseHeld
}

// autoscale runs the memory autoscaler after the checks and sets
// rf.Status.Autoscaling. The Recommend mode only reports, so an error is
// logged and never fails the reconcile or makes the RedisFailover NotHealthy.
func (r *RedisFailoverHandler) autoscale(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus) {
	read, fromHandle, master, run := r.autoscaleInput(rf, observed)
	if !run {
		return
	}
	stored, cm := read.state, read.cm
	limit := read.effective
	maxLimit := rf.Spec.Redis.Autoscaling.MemoryMax.Value()
	now := r.now()
	state, last, version, write := r.continueAutoscaleState(rf, observed, read, fromHandle, now)
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
		var err error
		if state, err = r.sampleAutoscaler(rf, master, in, state, &last, version); err != nil {
			errs = append(errs, err)
		}
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
	r.autoscaleStepWritten(rf, state.Phase, next, action)
	r.reportAutoscale(rf, read, next, wait, last.ratio)
	r.logAutoscale(rf, errors.Join(errs...))
}

// autoscaleInput returns the state that Handle read, or reads it. It also
// returns whether Handle read the state, and the IP of the master of the
// checks. It returns run false when the autoscaler does not run in this
// reconcile.
func (r *RedisFailoverHandler) autoscaleInput(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus) (read autoscaleRead, fromHandle bool, master string, run bool) {
	key := failoverKey(rf)
	v, ok := r.autoscaleReads.LoadAndDelete(key)
	if !rf.AutoscalingEnabled() {
		// Without an earlier status, Handle removed the state. This keeps the
		// API calls of the checks of an instance without autoscaling.
		if observed != (redisfailoverv1.AutoscalingStatus{}) {
			r.stopAutoscaler(rf, observed)
		}
		return autoscaleRead{}, false, "", false
	}
	r.autoscaleOff.Delete(key)

	m, _ := r.autoscaleMasters.LoadAndDelete(key)
	master, _ = m.(string)
	read, _ = v.(autoscaleRead)
	if !ok {
		// CheckAndHeal ran without Handle.
		var err error
		if read, err = r.readAutoscale(rf); err != nil {
			rf.Status.Autoscaling = observed
			r.logAutoscale(rf, err)
			return read, ok, master, false
		}
	}
	// The autoscaler cannot use a state of another owner or of a newer
	// operator, and it does not overwrite it. No metric value is known.
	if read.blocked != "" {
		rf.Status.Autoscaling = autoscale.ReportStep(redisfailoverv1.AutoscalingStatus{Reason: read.blocked}, autoscale.State{}, read.spec, read.effective)
		r.mClient.DeleteAutoscaler(rf.Namespace, rf.Name)
		r.logAutoscale(rf, read.err)
		return read, ok, master, false
	}
	return read, ok, master, true
}

// continueAutoscaleState returns the state that this reconcile continues, the
// last sample in memory, the resourceVersion of the state ConfigMap, and
// whether the state needs a write. fromHandle tells that Handle read the
// state before Ensure.
func (r *RedisFailoverHandler) continueAutoscaleState(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus, read autoscaleRead, fromHandle bool, now time.Time) (autoscale.State, autoscaleSample, string, bool) {
	maxLimit := rf.Spec.Redis.Autoscaling.MemoryMax.Value()
	last, version := r.lastAutoscaleSample(rf, read)
	state := read.state
	// A lost state can have the version of the sample, for example no
	// ConfigMap after a delete.
	lost := read.lost || stateLost(read, observed)
	if last.sampled && last.version == version && !read.resumed && !lost {
		state = last.state
	}
	unbound := state
	state = freezeAutoscaleState(rf, read, state, lost)
	state = autoscale.Bound(state, read.spec, maxLimit)
	if fromHandle && read.state.Phase == autoscale.PhaseReverted && !read.state.RevertApplied && state.Phase == autoscale.PhaseReverted {
		// Ensure of this reconcile applied the limit before the step.
		state.RevertApplied = true
	}
	// The next reconcile gives the pods the limit of the stored state, so a
	// change of the limit cannot wait for the next sample.
	write := state != unbound || read.resumed
	// A leftover state, for example of a mode change to Off that the status
	// did not show, must not give a recommendation of an earlier time.
	state = autoscale.Expire(state, now)
	return state, last, version, write
}

// lastAutoscaleSample returns the last sample in memory, and the
// resourceVersion of the state ConfigMap or "".
func (r *RedisFailoverHandler) lastAutoscaleSample(rf *redisfailoverv1.RedisFailover, read autoscaleRead) (autoscaleSample, string) {
	sample, _ := r.autoscaleSamples.Load(failoverKey(rf))
	last, found := sample.(autoscaleSample)
	if !found {
		last.ratio = read.state.Window.PeakRatio
	}
	// While the writes fail, each sample must continue from the last sample
	// and not from the stored state, or the status changes at each reconcile.
	// A write of another operator changes the resourceVersion, and then the
	// ConfigMap wins. The sample times cannot tell this, because the clock
	// can go back.
	version := ""
	if read.cm != nil {
		version = read.cm.ResourceVersion
	}
	return last, version
}

// freezeAutoscaleState freezes a state without a version at the limit of
// readAutoscale, and ends a freeze after a change of the spec.
func freezeAutoscaleState(rf *redisfailoverv1.RedisFailover, read autoscaleRead, state autoscale.State, lost bool) autoscale.State {
	limit := read.effective
	if state.Version == 0 && (limit != read.spec || lost) {
		// The write of the state keeps the limit that readAutoscale froze, and
		// no step starts from it. A lost state also lost its budget, its
		// back-off and its step.
		state.Version, state.Effective, state.SpecLimit = autoscale.StateVersion, limit, read.spec
		state.Frozen, state.Generation = true, rf.Generation
	}
	if state.Frozen && state.Generation != rf.Generation {
		// A change of the spec ends the freeze in each mode. Plan runs only
		// in mode Auto.
		state = autoscale.Unfreeze(state)
	}
	return state
}

// sampleAutoscaler adds a sample of the master at master to state, and keeps
// it in last and in memory. The error tells why the sample has no INFO.
func (r *RedisFailoverHandler) sampleAutoscaler(rf *redisfailoverv1.RedisFailover, master string, in autoscale.Input, state autoscale.State, last *autoscaleSample, version string) (autoscale.State, error) {
	var err error
	if in.Healthy && master != "" {
		info, infoErr := r.rfChecker.GetAutoscalerInfo(rf, master)
		if infoErr != nil {
			err = fmt.Errorf("no autoscaler sample of %s: %w", master, infoErr)
		}
		in.Info = info
	}
	state = autoscale.Observe(state, in)
	last.at, last.state, last.version, last.sampled = in.Now, state, version, true
	if in.Info != nil {
		last.ratio = autoscale.UsedRatio(in.Info, in.Limit)
	}
	r.autoscaleSamples.Store(failoverKey(rf), *last)
	return state, err
}

// reportAutoscale sets rf.Status.Autoscaling and the metrics of the state
// next. wait is the reason that a recommended step does not start, and ratio
// the used ratio of the last sample.
func (r *RedisFailoverHandler) reportAutoscale(rf *redisfailoverv1.RedisFailover, read autoscaleRead, next autoscale.State, wait string, ratio float64) {
	limit := read.effective
	maxLimit := rf.Spec.Redis.Autoscaling.MemoryMax.Value()
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
	r.mClient.SetAutoscaler(rf.Namespace, rf.Name, recommended, ratio, next.HighWindows, limit)
	r.mClient.SetAutoscalerPhase(rf.Namespace, rf.Name, phaseValue(shown.Phase))
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
