package autoscale

import (
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// The phases of a step of mode Auto.
const (
	PhaseIdle     = "Idle"
	PhaseApplying = "Applying"
	PhaseDone     = "Done"
	PhaseReverted = "Reverted"
	PhaseHeld     = "Held"
)

// The step rules.
const (
	// Cooldown is the minimum time from the end of a step to the start of
	// the next step.
	Cooldown = 15 * time.Minute
	// UrgentCooldown replaces Cooldown for ReasonUrgent and ReasonOOM,
	// because a store at that memory use refuses writes soon.
	UrgentCooldown = 5 * time.Minute
	// StepBudget is the maximum number of steps in BudgetPeriod. memoryMax
	// limits the total, and the budget limits the speed.
	StepBudget   = 6
	BudgetPeriod = 24 * time.Hour
	// BackOff is the wait after a revert. It doubles at each further revert
	// up to MaxBackOff, because the cause of a refusal, for example a
	// ResourceQuota, often stays.
	BackOff    = time.Hour
	MaxBackOff = 24 * time.Hour
	// ApplyTimeout is three in-place resize timeouts of 5 minutes, one for
	// each pod of a RedisFailover with 3 replicas.
	ApplyTimeout = 15 * time.Minute
)

// The refusals that the operator gives itself.
const (
	// RefusalTimeout is the refusal of a step that did not end in
	// ApplyTimeout.
	RefusalTimeout = "the step did not end in 15 minutes"
	// RefusalClear holds a step that the operator cannot revert, because it
	// cannot remove the resize request of a pod.
	RefusalClear = "the operator cannot remove the resize request of a pod"
	// RefusalStepPod holds a revert that Ensure did not apply, because a pod
	// runs or starts from the template of the step.
	RefusalStepPod = "a pod runs or starts with the limit of the step"
)

// The reasons in the status of mode Auto. A reason with a refusal adds the
// refusal that the state holds, so the text changes only at a transition.
const (
	ReasonHeld     = "the operator holds the step"
	ReasonReverted = "the operator reverted the step"
	ReasonFrozen   = "the state ConfigMap is missing or not valid, so the pods keep the StatefulSet limit until a change of the spec"
	// ReasonResizeRefused shows a refusal outside a step, for example of a
	// change of the spec while the operator cannot read a revision.
	ReasonResizeRefused = "the operator keeps a pod on its revision, because the resize failed"
	// The reasons that a recommended step does not start.
	ReasonNotSettled          = "a step waits until the rollout is complete and the RedisFailover is Healthy"
	ReasonCooldown            = "a step waits for the cooldown after the last step"
	ReasonBudget              = "a step waits, because 6 steps in 24 hours is the maximum"
	ReasonNoInPlaceResize     = "a step needs an in-place resize, which needs Kubernetes 1.33 or later"
	ReasonInPlaceDisabled     = "a step needs redis.inPlaceResize Enabled"
	ReasonNoRevisionAccess    = "a step needs the permission to get controllerrevisions"
	ReasonVPA                 = "a VerticalPodAutoscaler changes the redis pods"
	ReasonRestartOnResize     = "a step needs a redis container without the memory resizePolicy RestartContainer"
	ReasonCustomMaxMemory     = "a step needs a maxmemory that the operator manages, not customConfig"
	ReasonNoManagedMaxMemory  = "a step needs redis.maxMemory"
	ReasonPreconditionUnknown = "a step waits, because a check before the step failed"
)

// NoteSpecBelowEffective is added to the reason after a user lowered the
// spec limit below the effective limit. The pods keep the effective limit.
const NoteSpecBelowEffective = "redis.resources.limits.memory is below effectiveMemory"

// Action tells the handler which transition Plan made.
type Action string

// The actions of Plan.
const (
	ActionNone   Action = ""
	ActionStart  Action = "Start"
	ActionDone   Action = "Done"
	ActionRevert Action = "Revert"
	ActionHold   Action = "Hold"
)

// Observation is what the handler knows at a reconcile.
type Observation struct {
	Now time.Time
	// Spec is the memory limit of redis.resources, and Max is memoryMax.
	Spec, Max int64
	// Generation is the metadata.generation of the RedisFailover.
	Generation int64
	// Settled is true when the RedisFailover is Healthy, all pods are Ready
	// and on the update revision, and the checks before a step passed.
	Settled bool
	// Applied is true when all pods have an applied memory limit of at least
	// To, and the rollout of the latest template is complete. Thus a step is
	// done only when no pod waits for the relabel of the rollout.
	Applied bool
	// AnyApplied is true when a pod has an applied memory limit of at least
	// To, or runs or starts from the template of To. Only the limit counts,
	// not the revision label. The kubelet applies the limit before the
	// rollout moves the label, and a revert would then shrink the pod.
	AnyApplied bool
	// Refusal is the reason of a permanent refusal of a resize, or "".
	Refusal string
}

// EffectiveLimit returns the memory limit of the pods for a state.
func EffectiveLimit(s State, spec, maxLimit int64) int64 {
	if e := Bound(s, spec, maxLimit).Effective; e != 0 {
		return e
	}
	return spec
}

// Bound keeps Effective above the spec limit and at most memoryMax, or sets
// it to 0. A change is a user change of the spec or of memoryMax, and it ends
// a step that is not done.
func Bound(s State, spec, maxLimit int64) State {
	e := min(s.Effective, maxLimit)
	if e <= spec {
		e = 0
	}
	if e != s.Effective {
		s.Effective = e
		if s.Phase == PhaseApplying || s.Phase == PhaseHeld {
			s.Phase, s.Refusal = PhaseIdle, ""
		}
	}
	return s
}

// Resume sets Effective to live, the memory limit of the pods, in the bounds
// of Bound. A change ends a step that is not done, because its template did
// not reach the pods.
func Resume(s State, live, spec, maxLimit int64) State {
	if s.Phase == PhaseReverted {
		// No pod applied the step, and the template of the step can stay
		// only because Ensure did not run after the revert.
		return s
	}
	e := max(spec, min(live, maxLimit))
	if e == spec {
		e = 0
	}
	if e != s.Effective {
		s.Effective, s.SpecLimit = e, spec
		if s.Phase == PhaseApplying || s.Phase == PhaseHeld {
			s.Phase, s.Refusal = PhaseIdle, ""
		}
	}
	return s
}

// Plan returns the next state of the step lifecycle and the transition. A
// step starts only from Recommended, so the dwell of Observe applies.
func Plan(s State, o Observation) (State, Action) {
	now := o.Now.Unix()
	s = Bound(s.rebase(now), o.Spec, o.Max)
	if s.Phase == "" {
		s.Phase = PhaseIdle
	}
	if s.Frozen {
		if o.Generation == s.Generation {
			return s, ActionNone
		}
		// The user saw the frozen limit and changed the spec.
		s.Frozen = false
	}
	if s.Reverts != 0 && o.Generation != s.Generation {
		// A user change can remove the cause of the refusal.
		s.Reverts, s.RetryAt = 0, 0
		if s.Phase == PhaseReverted {
			s.Phase = PhaseIdle
		}
	}
	switch s.Phase {
	case PhaseApplying:
		switch {
		case o.Applied:
			return s.done(now), ActionDone
		case o.Refusal != "" && !o.AnyApplied:
			return s.revert(o, now), ActionRevert
		case o.Refusal != "":
			s.Phase, s.Refusal = PhaseHeld, o.Refusal
			return s, ActionHold
		case now-s.StartedAt >= seconds(ApplyTimeout) && !o.AnyApplied:
			// For example, a pod of the step that no node can hold.
			o.Refusal = RefusalTimeout
			return s.revert(o, now), ActionRevert
		case now-s.StartedAt >= seconds(ApplyTimeout):
			s.Phase, s.Refusal = PhaseHeld, RefusalTimeout
			return s, ActionHold
		}
		return s, ActionNone
	case PhaseReverted:
		if now < s.RetryAt {
			return s, ActionNone
		}
		s.Phase = PhaseIdle
	case PhaseDone:
		if now-s.DoneAt >= seconds(Cooldown) {
			s.Phase = PhaseIdle
		}
	case PhaseIdle:
	default:
		// Held, and also an unknown phase, for example of a newer operator:
		// no step starts.
		if o.Applied {
			return s.done(now), ActionDone
		}
		return s, ActionNone
	}
	return s.start(o, now)
}

func (s State) start(o Observation, now int64) (State, Action) {
	from := EffectiveLimit(s, o.Spec, o.Max)
	to := min(s.Recommended, o.Max)
	if to <= from || s.startWait(o, now) != "" {
		return s, ActionNone
	}
	s.Phase, s.From, s.To, s.Effective, s.SpecLimit = PhaseApplying, from, to, to, o.Spec
	s.Generation, s.StartedAt, s.RetryAt, s.Refusal = o.Generation, now, 0, ""
	copy(s.Steps[:], s.Steps[1:])
	s.Steps[StepBudget-1] = now
	return s, ActionStart
}

// startWait returns why a recommended step waits, or "".
func (s State) startWait(o Observation, now int64) string {
	cooldown := Cooldown
	if s.Reason == ReasonUrgent || s.Reason == ReasonOOM {
		cooldown = UrgentCooldown
	}
	switch {
	case !o.Settled:
		return ReasonNotSettled
	case s.DoneAt != 0 && now-s.DoneAt < seconds(cooldown):
		return ReasonCooldown
	case s.Steps[0] != 0 && now-s.Steps[0] < seconds(BudgetPeriod):
		return ReasonBudget
	}
	return ""
}

// WaitReason returns why the state of Plan starts no step for its
// recommendation, or "". The status shows it in place of the reason of the
// recommendation.
func WaitReason(s State, o Observation) string {
	if s.Frozen || s.Phase != "" && s.Phase != PhaseIdle && s.Phase != PhaseDone || min(s.Recommended, o.Max) <= EffectiveLimit(s, o.Spec, o.Max) {
		return ""
	}
	return s.startWait(o, o.Now.Unix())
}

// done ends a step. The windows and the OOM baseline start again, because
// they measured the memory use with the maxmemory of the earlier limit.
func (s State) done(now int64) State {
	s.Phase, s.Refusal, s.DoneAt = PhaseDone, "", now
	s.Window, s.RunID = Window{}, ""
	s.setHighWindows(0, now)
	s.setRecommended(0, now)
	return s
}

func (s State) revert(o Observation, now int64) State {
	s.Phase, s.Refusal, s.RevertApplied = PhaseReverted, o.Refusal, false
	s.Effective = s.From
	if s.Effective <= o.Spec {
		s.Effective = 0
	}
	s.Reverts++
	s.RetryAt = now + seconds(backOff(s.Reverts))
	// A later change of the generation is a user change.
	s.Generation = o.Generation
	return s
}

func backOff(reverts int32) time.Duration {
	d := BackOff
	for i := int32(1); i < reverts && d < MaxBackOff; i++ {
		d *= 2
	}
	return min(d, MaxBackOff)
}

// rebase moves the step times that are in the future to now, and starts a
// back-off that ends too late again. After the clock went back, a timer would
// otherwise wait for the length of the jump in addition.
func (s State) rebase(now int64) State {
	if wait := seconds(backOff(s.Reverts)); s.RetryAt > now+wait {
		s.RetryAt = now + wait
	}
	s.DoneAt = min(s.DoneAt, now)
	s.StartedAt = min(s.StartedAt, now)
	for i := range s.Steps {
		s.Steps[i] = min(s.Steps[i], now)
	}
	return s
}

// ReportStep adds the step fields of the state to a status of Report. limit
// is the memory limit of the pods.
func ReportStep(st redisfailoverv1.AutoscalingStatus, s State, spec, limit int64) redisfailoverv1.AutoscalingStatus {
	st.Phase = s.Phase
	switch {
	case s.Frozen:
		st.Reason = ReasonFrozen
	case s.Phase == PhaseHeld:
		st.Reason = ReasonHeld + ": " + s.Refusal
	case s.Phase == PhaseReverted:
		st.Reason = ReasonReverted + ": " + s.Refusal
	}
	if limit != spec {
		st.EffectiveMemory = formatQuantity(limit)
		if s.SpecLimit > spec {
			st.Reason += "; " + NoteSpecBelowEffective
		}
	}
	if s.StartedAt != 0 {
		st.LastStep = formatMinute(s.StartedAt)
	}
	if s.Phase == PhaseReverted && s.RetryAt != 0 {
		st.RetryAfter = formatMinute(s.RetryAt)
	}
	return st
}

func seconds(d time.Duration) int64 {
	return int64(d / time.Second)
}
