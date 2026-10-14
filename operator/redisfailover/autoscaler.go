package redisfailover

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
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
	// spec is the memory limit of redis.resources, and effective the memory
	// limit of the pods.
	spec, effective int64
}

// readAutoscale reads the state and resolves the effective memory limit. In
// mode Auto, a state without a limit freezes it at the limit of the live
// StatefulSet, because the spec limit can shrink the pods.
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
		read.effective = autoscale.EffectiveLimit(read.state, spec, maxLimit)
		return read, nil
	}
	if rf.Spec.Redis.Autoscaling.Mode != redisfailoverv1.AutoscalingModeAuto {
		return read, nil
	}
	ss, err := r.k8sservice.GetStatefulSet(rf.Namespace, rfservice.GetRedisName(rf))
	if apierrors.IsNotFound(err) {
		return read, nil
	}
	if err != nil {
		return read, err
	}
	read.effective = max(spec, min(rfservice.TemplateRedisMemoryLimit(ss), maxLimit))
	return read, nil
}

// autoscalerLimits returns the memory limits of this reconcile in mode Auto
// when the effective limit differs from the spec limit. Only then a revision
// can be a step of the autoscaler. In the other modes, a frozen effective
// limit makes no step, so each revision takes the user path.
func (r *RedisFailoverHandler) autoscalerLimits(rf *redisfailoverv1.RedisFailover) rfservice.AutoscalerLimits {
	if !rf.AutoscalingEnabled() || rf.Spec.Redis.Autoscaling.Mode != redisfailoverv1.AutoscalingModeAuto {
		return rfservice.AutoscalerLimits{}
	}
	v, _ := r.autoscaleReads.Load(failoverKey(rf))
	read, ok := v.(autoscaleRead)
	if !ok {
		return rfservice.AutoscalerLimits{}
	}
	if read.effective == read.spec {
		return rfservice.AutoscalerLimits{}
	}
	return rfservice.AutoscalerLimits{Spec: read.spec, Effective: read.effective}
}

// autoscaleRollout is what UpdateRedisesPods found for the memory autoscaler
// in the current reconcile.
type autoscaleRollout struct {
	// settled is true when the rollout waits on nothing, the StatefulSet
	// controller observed the latest template, and each pod is ready on the
	// update revision.
	settled bool
	// applied is true when each pod runs with the effective memory limit or
	// a larger one, and anyApplied when one pod does.
	applied, anyApplied bool
	// refusal is the permanent refusal of a resize to an autoscaler revision.
	refusal *resizeRefusal
}

// resizeRefusal is a permanent refusal of the in-place resize of pod.
type resizeRefusal struct {
	pod     string
	class   rfservice.ResizeClass
	message string
}

// recordAutoscaleRollout completes the autoscaleRollout of mode Auto. It
// reads the StatefulSet and its pods, so the other modes do not call it.
func (r *RedisFailoverHandler) recordAutoscaleRollout(rf *redisfailoverv1.RedisFailover, waits bool) error {
	if !rf.AutoscalingEnabled() || rf.Spec.Redis.Autoscaling.Mode != redisfailoverv1.AutoscalingModeAuto {
		return nil
	}
	key := failoverKey(rf)
	v, _ := r.autoscaleRollout.Load(key)
	rollout, _ := v.(autoscaleRollout)
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
	complete := len(pods.Items) >= int(rf.Spec.Redis.Replicas)
	rollout.settled = complete && !waits && rollout.refusal == nil && ss.Status.ObservedGeneration >= ss.Generation
	rollout.applied = complete
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Labels[appsv1.ControllerRevisionHashLabelKey] != ss.Status.UpdateRevision || rfservice.ResizeRequested(pod) || !util.PodIsReady(pod) {
			rollout.settled = false
		}
		if rfservice.AppliedRedisMemoryLimit(pod) >= limit {
			rollout.anyApplied = true
		} else {
			rollout.applied = false
		}
	}
	r.autoscaleRollout.Store(key, rollout)
	return nil
}

// autoscale runs the memory autoscaler after the checks and sets
// rf.Status.Autoscaling. The Recommend mode only reports, so an error is
// logged and never fails the reconcile or makes the RedisFailover NotHealthy.
func (r *RedisFailoverHandler) autoscale(rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.AutoscalingStatus) {
	key := failoverKey(rf)
	v, ok := r.autoscaleReads.LoadAndDelete(key)
	if !rf.AutoscalingEnabled() {
		// Without an earlier status, there is no state to remove. This keeps
		// the API calls of an instance without autoscaling unchanged.
		if observed != (redisfailoverv1.AutoscalingStatus{}) {
			r.stopAutoscaler(rf, observed)
		}
		return
	}

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
	if last.sampled && last.version == version {
		state = last.state
	}
	unbound := state
	if state.Version == 0 && limit != read.spec {
		// The write of the state keeps the limit that readAutoscale froze.
		state.Version, state.Effective, state.SpecLimit = autoscale.StateVersion, limit, read.spec
	}
	state = autoscale.Bound(state, read.spec, maxLimit)
	// The next reconcile gives the pods the limit of the stored state, so a
	// change of the limit cannot wait for the next sample.
	limitChanged := state != unbound
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
		if cm == nil || state.NeedsWrite(stored) {
			// A write changes the resourceVersion, so the next reconcile
			// reads this state from the ConfigMap.
			errs = append(errs, r.writeAutoscaleState(rf, cm, state))
		}
	} else if limitChanged {
		errs = append(errs, r.writeAutoscaleState(rf, cm, state))
	}

	rf.Status.Autoscaling = autoscale.ReportStep(autoscale.Report(state, limit, maxLimit), state, read.spec, limit)
	var recommended int64
	if rf.Status.Autoscaling.RecommendedMemory != "" {
		recommended = min(state.Recommended, maxLimit)
	}
	r.mClient.SetAutoscaler(rf.Namespace, rf.Name, recommended, last.ratio, state.HighWindows, limit)
	r.logAutoscale(rf, errors.Join(errs...))
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
	r.mClient.DeleteAutoscaler(rf.Namespace, rf.Name)
	r.forgetAutoscaler(failoverKey(rf))
}

func (r *RedisFailoverHandler) forgetAutoscaler(key string) {
	r.autoscaleReads.Delete(key)
	r.autoscaleMasters.Delete(key)
	r.autoscaleRollout.Delete(key)
	r.autoscaleSamples.Delete(key)
	r.autoscaleLogged.Delete(key)
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
