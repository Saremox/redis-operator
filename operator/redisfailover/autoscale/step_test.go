package autoscale

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// obs is a settled instance with a spec limit of 1Gi and a memoryMax of
// 4Gi at t0+at.
func obs(at time.Duration) Observation {
	return Observation{Now: t0.Add(at), Spec: gi, Max: 4 * gi, Generation: 1, Settled: true}
}

// recommending is an idle state with a sustained recommendation of 1280Mi.
func recommending() State {
	s := known()
	s.Reason, s.Recommended, s.Limit = ReasonSustained, 1280*mi, gi
	return s
}

// applying is a step from 1Gi to 1280Mi that started at t0.
func applying(t *testing.T) State {
	t.Helper()
	s, action := Plan(recommending(), obs(0))
	require.Equal(t, ActionStart, action)
	return s
}

func at(d time.Duration) int64 {
	return t0.Add(d).Unix()
}

func TestPlanStartsAStep(t *testing.T) {
	s, action := Plan(recommending(), obs(0))

	assert.Equal(t, ActionStart, action)
	want := recommending()
	want.Phase, want.From, want.To, want.Effective, want.SpecLimit = PhaseApplying, gi, 1280*mi, 1280*mi, gi
	want.Generation, want.StartedAt = 1, t0.Unix()
	want.Steps[StepBudget-1] = t0.Unix()
	assert.Equal(t, want, s)
	assert.Equal(t, 1280*mi, EffectiveLimit(s, gi, 4*gi))
}

func TestPlanStartsNoStep(t *testing.T) {
	tests := []struct {
		name   string
		state  func() State
		obs    func() Observation
		phase  string
		reason string
	}{
		{name: "not settled", state: recommending, obs: func() Observation { o := obs(0); o.Settled = false; return o }},
		{name: "no recommendation", state: known, obs: func() Observation { return obs(0) }},
		{name: "the limit has the recommendation", state: recommending, obs: func() Observation { o := obs(0); o.Spec = 1280 * mi; return o }},
		{name: "at memoryMax", state: recommending, obs: func() Observation { o := obs(0); o.Max = gi; return o }},
		{name: "held", state: func() State { s := recommending(); s.Phase, s.Effective = PhaseHeld, 1280*mi; return s }, obs: func() Observation { return obs(0) }, phase: PhaseHeld},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, action := Plan(test.state(), test.obs())
			assert.Equal(t, ActionNone, action)
			assert.Equal(t, cmpOr(test.phase, PhaseIdle), s.Phase)
		})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// memoryMax caps the step, as it caps the recommendation.
func TestPlanCapsTheStepAtMemoryMax(t *testing.T) {
	o := obs(0)
	o.Max = 1100 * mi
	s, action := Plan(recommending(), o)
	assert.Equal(t, ActionStart, action)
	assert.Equal(t, 1100*mi, s.To)
}

// The windows measured the maxmemory of the earlier limit, and the OOM count
// can have grown during the step. Both start again at Done.
func TestPlanDoneResetsTheWindows(t *testing.T) {
	s := applying(t)
	s.Window = Window{Start: at(time.Minute), PeakRatio: 0.9}
	s.HighWindows, s.OOMErrors = 2, 3
	o := obs(4 * time.Minute)
	o.Applied = true

	s, action := Plan(s, o)

	assert.Equal(t, ActionDone, action)
	assert.Equal(t, PhaseDone, s.Phase)
	assert.Equal(t, at(4*time.Minute), s.DoneAt)
	assert.Equal(t, 1280*mi, s.Effective)
	assert.Zero(t, s.Window)
	assert.Zero(t, s.HighWindows)
	assert.Zero(t, s.Recommended)
	assert.Empty(t, s.RunID, "the next sample sets the OOM baseline")
	assert.Equal(t, at(4*time.Minute), s.EvaluatedAt)

	info := master(500)
	info.OOMErrors = 9
	got := Observe(s, input(5*time.Minute, info))
	assert.Equal(t, int64(9), got.OOMErrors)
	assert.Zero(t, got.Recommended, "no OOM step from the count before Done")
}

func TestPlanRefusal(t *testing.T) {
	o := obs(2 * time.Minute)
	o.Refusal = "quota"

	t.Run("no pod applied the size: revert", func(t *testing.T) {
		s, action := Plan(applying(t), o)
		assert.Equal(t, ActionRevert, action)
		assert.Equal(t, PhaseReverted, s.Phase)
		assert.Zero(t, s.Effective, "back at the spec limit")
		assert.Equal(t, "quota", s.Refusal)
		assert.Equal(t, int32(1), s.Reverts)
		assert.Equal(t, at(2*time.Minute+time.Hour), s.RetryAt)
	})

	t.Run("revert to a limit above the spec", func(t *testing.T) {
		s := applying(t)
		s.From = 1100 * mi
		s, _ = Plan(s, o)
		assert.Equal(t, 1100*mi, s.Effective)
	})

	t.Run("a pod applied the size: hold", func(t *testing.T) {
		o := o
		o.AnyApplied = true
		s, action := Plan(applying(t), o)
		assert.Equal(t, ActionHold, action)
		assert.Equal(t, PhaseHeld, s.Phase)
		assert.Equal(t, 1280*mi, s.Effective)
		assert.Equal(t, "quota", s.Refusal)
		assert.Zero(t, s.Reverts)
	})
}

// appliedObs is obs(at) with a pod that applied the step.
func appliedObs(at time.Duration) Observation {
	o := obs(at)
	o.AnyApplied = true
	return o
}

// A step that does not end in 15 minutes is held when a pod applied it, and
// reverted otherwise.
func TestPlanTimeout(t *testing.T) {
	s, action := Plan(applying(t), appliedObs(ApplyTimeout-time.Second))
	require.Equal(t, ActionNone, action)
	require.Equal(t, PhaseApplying, s.Phase)

	held, action := Plan(s, appliedObs(ApplyTimeout))
	assert.Equal(t, ActionHold, action)
	assert.Equal(t, PhaseHeld, held.Phase)
	assert.Equal(t, RefusalTimeout, held.Refusal)
	assert.Equal(t, 1280*mi, held.Effective)

	reverted, action := Plan(s, obs(ApplyTimeout))
	assert.Equal(t, ActionRevert, action)
	assert.Equal(t, PhaseReverted, reverted.Phase)
	assert.Equal(t, RefusalTimeout, reverted.Refusal)
	assert.Zero(t, reverted.Effective)
	assert.Equal(t, int32(1), reverted.Reverts)
}

// A phase that this operator does not know, for example of a newer operator,
// starts no step, as Held.
func TestPlanUnknownPhaseStartsNoStep(t *testing.T) {
	s := recommending()
	s.Phase = "Shrinking"

	got, action := Plan(s, obs(time.Hour))

	assert.Equal(t, ActionNone, action)
	assert.Equal(t, "Shrinking", got.Phase)
	assert.Zero(t, got.Effective)

	o := obs(time.Hour)
	o.Applied = true
	got, action = Plan(s, o)
	assert.Equal(t, ActionDone, action)
	assert.Equal(t, PhaseDone, got.Phase)
}

func TestPlanHeldEndsWhenAllPodsApplied(t *testing.T) {
	s, _ := Plan(applying(t), appliedObs(ApplyTimeout))
	require.Equal(t, PhaseHeld, s.Phase)
	o := obs(time.Hour)
	o.Applied = true

	s, action := Plan(s, o)

	assert.Equal(t, ActionDone, action)
	assert.Equal(t, PhaseDone, s.Phase)
	assert.Empty(t, s.Refusal)
}

// done is the state after a step from 1Gi to 1280Mi that ended at t0, with a
// next recommendation of reason.
func done(t *testing.T, reason string) State {
	t.Helper()
	s := applying(t)
	s.StartedAt, s.Steps[StepBudget-1] = at(-time.Minute), at(-time.Minute)
	o := obs(0)
	o.Applied = true
	s, _ = Plan(s, o)
	require.Equal(t, PhaseDone, s.Phase)
	s.Reason, s.Recommended = reason, 1600*mi
	return s
}

func TestPlanCooldown(t *testing.T) {
	tests := []struct {
		reason   string
		cooldown time.Duration
	}{
		{reason: ReasonSustained, cooldown: Cooldown},
		{reason: ReasonUrgent, cooldown: UrgentCooldown},
		{reason: ReasonOOM, cooldown: UrgentCooldown},
	}
	for _, test := range tests {
		t.Run(test.reason, func(t *testing.T) {
			s, action := Plan(done(t, test.reason), obs(test.cooldown-time.Second))
			assert.Equal(t, ActionNone, action)
			assert.Equal(t, PhaseDone, s.Phase)

			s, action = Plan(s, obs(test.cooldown))
			assert.Equal(t, ActionStart, action)
			assert.Equal(t, 1280*mi, s.From)
			assert.Equal(t, 1600*mi, s.To)
		})
	}
}

func TestPlanDoneBecomesIdle(t *testing.T) {
	o := obs(Cooldown)
	o.Settled = false
	s, action := Plan(done(t, ReasonSustained), o)
	assert.Equal(t, ActionNone, action)
	assert.Equal(t, PhaseIdle, s.Phase)
}

func TestPlanBackOff(t *testing.T) {
	refused := func(s State, now time.Duration) State {
		t.Helper()
		o := obs(now)
		o.Refusal = "quota"
		s, action := Plan(s, o)
		require.Equal(t, ActionRevert, action)
		return s
	}
	s := refused(applying(t), 0)
	waits := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	now := time.Duration(0)
	for i, wait := range waits {
		require.Equal(t, int32(i+1), s.Reverts)
		assert.Equal(t, at(now+wait), s.RetryAt, "revert %d", i+1)

		var action Action
		s, action = Plan(s, obs(now+wait-time.Second))
		require.Equal(t, ActionNone, action)
		require.Equal(t, PhaseReverted, s.Phase)

		now += wait
		// Only the budget limits the steps: one step each day.
		s.Steps = [StepBudget]int64{}
		s, action = Plan(s, obs(now))
		require.Equal(t, ActionStart, action, "retry %d", i+1)
		s = refused(s, now)
	}
	assert.Equal(t, 24*time.Hour, backOff(100))
}

// A user change can remove the cause of a refusal, so it ends the back-off.
func TestPlanUserChangeResetsTheBackOff(t *testing.T) {
	o := obs(time.Minute)
	o.Refusal = "quota"
	s, _ := Plan(applying(t), o)
	s.Reverts = 3

	same, action := Plan(s, obs(2*time.Minute))
	require.Equal(t, ActionNone, action)
	require.Equal(t, PhaseReverted, same.Phase)

	changed := obs(2 * time.Minute)
	changed.Generation = 2
	s, action = Plan(s, changed)

	assert.Equal(t, ActionStart, action)
	assert.Zero(t, s.Reverts)
	assert.Zero(t, s.RetryAt)
	assert.Equal(t, int64(2), s.Generation)
}

func TestPlanBudget(t *testing.T) {
	s := recommending()
	for i := range StepBudget {
		s.Steps[i] = at(time.Duration(i) * time.Hour)
	}
	s.DoneAt = s.Steps[StepBudget-1]

	_, action := Plan(s, obs(BudgetPeriod-time.Second))
	assert.Equal(t, ActionNone, action)

	s, action = Plan(s, obs(BudgetPeriod))
	assert.Equal(t, ActionStart, action)
	assert.Equal(t, at(time.Hour), s.Steps[0], "the oldest step leaves the budget")
	assert.Equal(t, at(BudgetPeriod), s.Steps[StepBudget-1])
}

// After the clock went back, each timer starts again at the time of the
// clock. It must not wait for the length of the jump in addition.
func TestPlanAfterTheClockWentBack(t *testing.T) {
	back := -2 * time.Hour

	t.Run("applying", func(t *testing.T) {
		s, action := Plan(applying(t), obs(back))
		require.Equal(t, ActionNone, action)
		assert.Equal(t, at(back), s.StartedAt)
		assert.Equal(t, at(back), s.Steps[StepBudget-1])
		_, action = Plan(s, appliedObs(back+ApplyTimeout))
		assert.Equal(t, ActionHold, action)
	})

	t.Run("cooldown", func(t *testing.T) {
		s, action := Plan(done(t, ReasonSustained), obs(back))
		require.Equal(t, ActionNone, action)
		assert.Equal(t, at(back), s.DoneAt)
		_, action = Plan(s, obs(back+Cooldown))
		assert.Equal(t, ActionStart, action)
	})

	t.Run("back-off", func(t *testing.T) {
		o := obs(0)
		o.Refusal = "quota"
		s, _ := Plan(applying(t), o)
		s, action := Plan(s, obs(back))
		require.Equal(t, ActionNone, action)
		assert.Equal(t, at(back+BackOff), s.RetryAt)
		_, action = Plan(s, obs(back+BackOff))
		assert.Equal(t, ActionStart, action)
	})
}

func TestPlanUserChanges(t *testing.T) {
	tests := []struct {
		name      string
		spec, max int64
		phase     string
		effective int64
	}{
		{name: "spec raised above To", spec: 1500 * mi, max: 4 * gi, phase: PhaseIdle},
		{name: "spec raised to To", spec: 1280 * mi, max: 4 * gi, phase: PhaseApplying},
		{name: "spec raised below To", spec: 1100 * mi, max: 4 * gi, phase: PhaseApplying, effective: 1280 * mi},
		{name: "spec lowered", spec: 512 * mi, max: 4 * gi, phase: PhaseApplying, effective: 1280 * mi},
		{name: "memoryMax lowered below To", spec: gi, max: 1100 * mi, phase: PhaseIdle, effective: 1100 * mi},
		{name: "memoryMax lowered to the spec", spec: gi, max: gi, phase: PhaseIdle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			o := obs(time.Minute)
			o.Spec, o.Max, o.Settled = test.spec, test.max, false
			s, action := Plan(applying(t), o)
			assert.Equal(t, ActionNone, action)
			assert.Equal(t, test.phase, s.Phase)
			assert.Equal(t, test.effective, s.Effective)
			assert.Equal(t, max(test.spec, test.effective), EffectiveLimit(s, test.spec, test.max))
		})
	}
}

// A spec limit at the target of the step does not change the template. The
// step continues, and a refusal or the timeout holds it, because a revert
// cannot lower the template.
func TestPlanKeepsAStepWithTheSpecAtTo(t *testing.T) {
	pinned := func(t *testing.T) State {
		t.Helper()
		o := obs(time.Minute)
		o.Spec, o.Settled = 1280*mi, false
		s, action := Plan(applying(t), o)
		require.Equal(t, ActionNone, action)
		require.Equal(t, PhaseApplying, s.Phase)
		return s
	}
	t.Run("refusal", func(t *testing.T) {
		o := obs(2 * time.Minute)
		o.Spec, o.Refusal = 1280*mi, "quota"
		s, action := Plan(pinned(t), o)
		assert.Equal(t, ActionHold, action)
		assert.Equal(t, PhaseHeld, s.Phase)
		assert.Equal(t, "quota", s.Refusal)
		assert.Zero(t, s.Effective)
		assert.Zero(t, s.Reverts)
	})
	t.Run("timeout", func(t *testing.T) {
		o := obs(ApplyTimeout)
		o.Spec = 1280 * mi
		s, action := Plan(pinned(t), o)
		assert.Equal(t, ActionHold, action)
		assert.Equal(t, RefusalTimeout, s.Refusal)
	})
	t.Run("done", func(t *testing.T) {
		o := obs(2 * time.Minute)
		o.Spec, o.Applied = 1280*mi, true
		s, action := Plan(pinned(t), o)
		assert.Equal(t, ActionDone, action)
		assert.Equal(t, PhaseDone, s.Phase)
	})
	t.Run("held step", func(t *testing.T) {
		s := applying(t)
		s.Phase, s.Refusal = PhaseHeld, "infeasible"
		o := obs(time.Hour)
		o.Spec = 1280 * mi
		s, action := Plan(s, o)
		assert.Equal(t, ActionNone, action)
		assert.Equal(t, PhaseHeld, s.Phase)
		assert.Equal(t, "infeasible", s.Refusal)
		assert.Equal(t, s, Resume(s, 1280*mi, 1280*mi, 4*gi))

		// An expired state from before the change of the spec.
		stored := applying(t)
		stored.Phase = PhaseHeld
		resumed := Resume(stored, 1280*mi, 1280*mi, 4*gi)
		assert.Equal(t, PhaseHeld, resumed.Phase)
		assert.Zero(t, resumed.Effective)
	})
	t.Run("a later change of the spec ends it", func(t *testing.T) {
		for _, spec := range []int64{1344 * mi, 1100 * mi} {
			o := obs(2 * time.Minute)
			o.Spec, o.Settled = spec, false
			s, _ := Plan(pinned(t), o)
			assert.Equal(t, PhaseIdle, s.Phase, spec)
			assert.Equal(t, PhaseIdle, Resume(pinned(t), spec, spec, 4*gi).Phase, spec)
		}
	})
}

// A change of the spec turns a frozen limit above the spec limit into a held
// step to it. The step ends as Done when each pod runs with the limit, and a
// spec limit above it ends the step.
func TestUnfreeze(t *testing.T) {
	frozen := State{Version: StateVersion, Phase: PhaseIdle, Frozen: true, Generation: 1, Effective: 1280 * mi, SpecLimit: gi}
	for _, test := range []struct {
		spec, effective int64
		phase           string
	}{
		{spec: gi, effective: 1280 * mi, phase: PhaseHeld},
		{spec: gi + 64*mi, effective: 1280 * mi, phase: PhaseHeld},
		{spec: 1280 * mi, phase: PhaseHeld},
		{spec: 1344 * mi, phase: PhaseIdle},
	} {
		o := obs(time.Minute)
		o.Generation, o.Spec, o.Settled = 2, test.spec, false
		s, action := Plan(frozen, o)
		assert.Equal(t, ActionNone, action)
		assert.False(t, s.Frozen)
		assert.Equal(t, test.phase, s.Phase, test.spec)
		assert.Equal(t, test.effective, s.Effective, test.spec)
		if test.phase == PhaseHeld {
			assert.Equal(t, RefusalLostState, s.Refusal)
			assert.Equal(t, gi, s.From)
			assert.Equal(t, 1280*mi, s.To)

			o.Applied = true
			s, action = Plan(s, o)
			assert.Equal(t, ActionDone, action)
			assert.Equal(t, PhaseDone, s.Phase)
		}
	}

	// Without the generation of a change, the freeze stays.
	o := obs(time.Minute)
	o.Generation = 1
	s, _ := Plan(frozen, o)
	assert.True(t, s.Frozen)

	atSpec := frozen
	atSpec.Effective = 0
	assert.Equal(t, PhaseIdle, Unfreeze(atSpec).Phase)
}

// A spec limit at or above the limit of a step ends the step, unless the
// step is not done. A later lower spec limit then gives the pods of the step
// the user path.
func TestBoundEndsTheStepAtTheSpecLimit(t *testing.T) {
	for _, test := range []struct {
		phase string
		spec  int64
		to    int64
	}{
		{PhaseDone, 1280 * mi, 0},
		{PhaseDone, 1536 * mi, 0},
		{PhaseIdle, 1280 * mi, 0},
		{PhaseReverted, 1280 * mi, 0},
		{PhaseDone, gi, 1280 * mi},
		{PhaseApplying, 1280 * mi, 1280 * mi},
		{PhaseHeld, 1280 * mi, 1280 * mi},
	} {
		s := State{Phase: test.phase, From: gi, To: 1280 * mi, Effective: 1280 * mi}
		assert.Equal(t, test.to, Bound(s, test.spec, 4*gi).To, "%s at %d", test.phase, test.spec)
	}
}

func TestBoundKeepsTheStepOfAnotherPhase(t *testing.T) {
	s := State{Phase: PhaseDone, Effective: 1280 * mi}
	assert.Equal(t, State{Phase: PhaseDone}, Bound(s, 1280*mi, 4*gi))
	assert.Equal(t, gi, EffectiveLimit(State{}, gi, 4*gi))
}

func TestReportStep(t *testing.T) {
	st := redisfailoverv1.AutoscalingStatus{Reason: ReasonLow}
	assert.Equal(t, st, ReportStep(st, State{}, gi, gi))

	s := applying(t)
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Reason:          ReasonLow,
		EffectiveMemory: "1280Mi",
		Phase:           PhaseApplying,
		LastStep:        t0.UTC().Format(time.RFC3339),
	}, ReportStep(st, s, gi, 1280*mi))

	assert.Equal(t, ReasonLow+"; "+NoteSpecBelowEffective, ReportStep(st, s, 512*mi, 1280*mi).Reason, "the user lowered the spec")

	o := obs(90 * time.Second)
	o.Refusal = "quota"
	s, _ = Plan(s, o)
	got := ReportStep(st, s, gi, gi)
	assert.Equal(t, PhaseReverted, got.Phase)
	assert.Empty(t, got.EffectiveMemory)
	assert.Equal(t, t0.Add(time.Hour+time.Minute).UTC().Format(time.RFC3339), got.RetryAfter)
}

// The pods have the Effective limit also when no operator samples, so Expire
// must keep the step.
func TestExpireKeepsTheStep(t *testing.T) {
	s := applying(t)
	s.LastSample = t0.Unix()

	got := Expire(s, t0.Add(time.Hour))

	require.Equal(t, ReasonNoSample, got.Reason)
	assert.Equal(t, s.Effective, got.Effective)
	assert.Equal(t, s.Phase, got.Phase)
	assert.Equal(t, s.From, got.From)
	assert.Equal(t, s.To, got.To)
	assert.Equal(t, s.StartedAt, got.StartedAt)
	assert.Equal(t, s.Steps, got.Steps)
}

func TestStateVersion1IsUpgradedOnTheNextWrite(t *testing.T) {
	var stored State
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"workload":"Store","window":{"start":0,"peakRatio":0,"peakUsed":0},"highWindows":0,"runID":"run1","oomErrors":0,"reason":"memory use is below the threshold","limit":1073741824}`), &stored))
	assert.Equal(t, gi, EffectiveLimit(stored, gi, 4*gi))

	got := Observe(stored, input(0, master(100)))

	assert.Equal(t, 2, got.Version)
	assert.True(t, got.NeedsWrite(stored))
}

func TestStateRoundTrip(t *testing.T) {
	s := applying(t)
	data, err := json.Marshal(s)
	require.NoError(t, err)
	var got State
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, s, got)

	data, err = json.Marshal(known())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "steps", "a state without steps")

	moved := s
	moved.Steps[0] = 1
	assert.True(t, moved.NeedsWrite(s), "a change of the steps")
}

// A frozen limit starts no step until the spec changes.
func TestPlanFrozen(t *testing.T) {
	s := recommending()
	s.Effective, s.Frozen, s.Generation = 1100*mi, true, 1

	got, action := Plan(s, obs(time.Hour))
	assert.Equal(t, ActionNone, action)
	assert.True(t, got.Frozen)
	assert.Equal(t, ReasonFrozen, ReportStep(redisfailoverv1.AutoscalingStatus{Reason: ReasonSustained}, got, gi, 1100*mi).Reason)
	assert.Empty(t, WaitReason(got, obs(time.Hour)), "the frozen reason has priority")

	// The change of the spec holds the frozen limit as a step until each pod
	// runs with it. The next step waits for the cooldown after that.
	o := obs(time.Hour)
	o.Generation = 2
	got, action = Plan(s, o)
	assert.Equal(t, ActionNone, action)
	assert.False(t, got.Frozen)
	assert.Equal(t, PhaseHeld, got.Phase)
	o.Applied = true
	got, action = Plan(got, o)
	require.Equal(t, ActionDone, action)
	got.Recommended, got.Reason = 1408*mi, ReasonSustained
	got, action = Plan(got, obs(time.Hour+Cooldown))
	assert.Equal(t, ActionStart, action)
	assert.Equal(t, 1100*mi, got.From)
}

func TestWaitReason(t *testing.T) {
	notSettled := obs(0)
	notSettled.Settled = false
	budget := recommending()
	for i := range StepBudget {
		budget.Steps[i] = at(time.Duration(i) * time.Minute)
	}
	reverted := recommending()
	reverted.Phase = PhaseReverted
	applied := recommending()
	applied.Effective = 1280 * mi
	tests := map[string]struct {
		s    State
		o    Observation
		want string
	}{
		"no recommendation":            {known(), notSettled, ""},
		"limit has the recommendation": {applied, notSettled, ""},
		"not settled":                  {recommending(), notSettled, ReasonNotSettled},
		"cooldown":                     {done(t, ReasonSustained), obs(time.Minute), ReasonCooldown},
		"budget":                       {budget, obs(time.Hour), ReasonBudget},
		"step can start":               {recommending(), obs(0), ""},
		"reverted":                     {reverted, notSettled, ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.want, WaitReason(test.s, test.o))
		})
	}
}

func TestReportStepRefusal(t *testing.T) {
	st := redisfailoverv1.AutoscalingStatus{Reason: ReasonLow}
	for phase, reason := range map[string]string{PhaseHeld: ReasonHeld, PhaseReverted: ReasonReverted} {
		s := State{Phase: phase, Refusal: "pod rfr-test-1: resize infeasible"}
		assert.Equal(t, reason+": pod rfr-test-1: resize infeasible", ReportStep(st, s, gi, gi).Reason, phase)
	}
}

func TestResume(t *testing.T) {
	s := applying(t)
	tests := map[string]struct {
		live      int64
		effective int64
		phase     string
	}{
		"template has the step":          {1280 * mi, 1280 * mi, PhaseApplying},
		"template before the step":       {gi, 0, PhaseIdle},
		"template between":               {1100 * mi, 1100 * mi, PhaseIdle},
		"template above memoryMax":       {8 * gi, 4 * gi, PhaseIdle},
		"template below the spec":        {512 * mi, 0, PhaseIdle},
		"template larger than Effective": {1536 * mi, 1536 * mi, PhaseIdle},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := Resume(s, test.live, gi, 4*gi)
			assert.Equal(t, test.effective, got.Effective)
			assert.Equal(t, test.phase, got.Phase)
		})
	}

	reverted := s
	reverted.Phase, reverted.Effective = PhaseReverted, 0
	assert.Equal(t, reverted, Resume(reverted, 1280*mi, gi, 4*gi), "a revert stays")

	held := s
	held.Phase, held.Refusal = PhaseHeld, "quota"
	got := Resume(held, gi, gi, 4*gi)
	assert.Equal(t, PhaseIdle, got.Phase)
	assert.Empty(t, got.Refusal)
	doneStep := State{Phase: PhaseDone, Effective: 1280 * mi}
	assert.Equal(t, PhaseDone, Resume(doneStep, gi, gi, 4*gi).Phase, "a done step keeps its phase")
}
