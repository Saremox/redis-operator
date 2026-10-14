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

func TestPlanTimeout(t *testing.T) {
	s, action := Plan(applying(t), obs(ApplyTimeout-time.Second))
	require.Equal(t, ActionNone, action)
	require.Equal(t, PhaseApplying, s.Phase)

	s, action = Plan(s, obs(ApplyTimeout))
	assert.Equal(t, ActionHold, action)
	assert.Equal(t, PhaseHeld, s.Phase)
	assert.Equal(t, RefusalTimeout, s.Refusal)
	assert.Equal(t, 1280*mi, s.Effective)
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
	s, _ := Plan(applying(t), obs(ApplyTimeout))
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
		s, action = Plan(s, obs(back+ApplyTimeout))
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
		{name: "spec raised to To", spec: 1280 * mi, max: 4 * gi, phase: PhaseIdle},
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
