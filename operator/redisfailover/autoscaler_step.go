package redisfailover

import (
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
)

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
func (r *RedisFailoverHandler) autoscaleStepWritten(rf *redisfailoverv1.RedisFailover, from string, s autoscale.State, action autoscale.Action) {
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	switch action {
	case autoscale.ActionNone:
		return
	case autoscale.ActionStart:
		logger.Infof("Autoscaler: a step of the memory limit from %s to %s starts", quantity(s.From), quantity(s.To))
	case autoscale.ActionDone:
		logger.Infof("Autoscaler: the step to %s is done", quantity(s.To))
		// A held step counted as held, and a held frozen limit is no step of
		// the operator.
		if from == autoscale.PhaseApplying {
			r.mClient.CountAutoscalerStep(rf.Namespace, rf.Name, metrics.AutoscalerStepDone)
		}
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
