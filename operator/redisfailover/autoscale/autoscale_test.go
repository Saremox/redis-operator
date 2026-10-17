package autoscale

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/redis"
)

const (
	mi = int64(1 << 20)
	gi = 1024 * mi
)

var t0 = time.Unix(1_800_000_000, 0)

// master returns a sample of a settled master with a maxmemory of 1000MiB.
func master(usedMiB int64) *redis.AutoscalerInfo {
	return &redis.AutoscalerInfo{
		UsedMemory:      usedMiB * mi,
		MaxMemory:       1000 * mi,
		MaxMemoryPolicy: "noeviction",
		UptimeSeconds:   3600,
		RunID:           "run1",
		Role:            "master",
	}
}

func input(at time.Duration, info *redis.AutoscalerInfo) Input {
	return Input{
		Now:      t0.Add(at),
		Healthy:  true,
		Info:     info,
		Limit:    gi,
		Max:      4 * gi,
		Workload: redisfailoverv1.AutoscalingWorkloadAuto,
		Policy:   "noeviction",
	}
}

// observeAll feeds one sample per minute, starting at start.
func observeAll(s State, start time.Duration, used ...int64) State {
	for i, u := range used {
		s = Observe(s, input(start+time.Duration(i)*time.Minute, master(u)))
	}
	return s
}

// known is a state with the run_id of master.
func known() State {
	return State{Version: StateVersion, RunID: "run1"}
}

func TestObserveFirstSample(t *testing.T) {
	info := master(990)
	info.OOMErrors = 3
	s := Observe(State{}, input(0, info))
	assert.Equal(t, State{
		Version:     StateVersion,
		Workload:    redisfailoverv1.AutoscalingWorkloadStore,
		Window:      Window{Start: t0.Unix(), PeakRatio: 0.99, PeakUsed: 990 * mi},
		RunID:       "run1",
		OOMErrors:   3,
		Recommended: 1280 * mi,
		Reason:      ReasonUrgent,
		EvaluatedAt: t0.Unix(),
		LastSample:  t0.Unix(),
		Limit:       gi,
	}, s)
	// The OOM count before the first sample is no growth.
	s = Observe(State{}, input(0, func() *redis.AutoscalerInfo { i := master(100); i.OOMErrors = 3; return i }()))
	assert.Equal(t, ReasonLow, s.Reason)
}

func TestObserveGuards(t *testing.T) {
	high := observeAll(known(), 0, 900, 900, 900, 900, 900, 900)
	assert.Equal(t, int32(1), high.HighWindows)
	assert.NotZero(t, high.Window.Start)

	tests := []struct {
		name   string
		in     Input
		reason string
	}{
		{name: "not healthy", in: func() Input { in := input(6*time.Minute, master(990)); in.Healthy = false; return in }(), reason: ReasonNotHealthy},
		{name: "no INFO", in: input(6*time.Minute, nil), reason: ReasonNoInfo},
		{name: "not the master", in: input(6*time.Minute, func() *redis.AutoscalerInfo { i := master(990); i.Role = "slave"; return i }()), reason: ReasonNotMaster},
		{name: "warm-up", in: input(6*time.Minute, func() *redis.AutoscalerInfo { i := master(990); i.UptimeSeconds = 599; return i }()), reason: ReasonWarmUp},
		{name: "loading", in: input(6*time.Minute, func() *redis.AutoscalerInfo { i := master(990); i.Loading = true; return i }()), reason: ReasonLoading},
		{name: "sync", in: input(6*time.Minute, func() *redis.AutoscalerInfo { i := master(990); i.SyncInProgress = true; return i }()), reason: ReasonSyncing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Observe(high, test.in)
			assert.Equal(t, test.reason, got.Reason)
			assert.Equal(t, test.in.Now.Unix(), got.LastSample)
			// The sample is discarded: the window, the count and the
			// recommendation stay.
			want := high
			want.Reason, want.LastSample = test.reason, got.LastSample
			assert.Equal(t, want, got)
		})
	}

	t.Run("run_id change resets", func(t *testing.T) {
		s := observeAll(known(), 0, 990)
		assert.NotZero(t, s.Recommended)
		info := master(990)
		info.RunID, info.OOMErrors = "run2", 7
		got := Observe(s, input(2*time.Minute, info))
		assert.Equal(t, ReasonRestarted, got.Reason)
		assert.Equal(t, "run2", got.RunID)
		assert.Equal(t, int64(7), got.OOMErrors)
		assert.Zero(t, got.HighWindows)
		assert.Zero(t, got.Window)
		assert.Zero(t, got.Recommended)
		// The OOM count of the new run is the baseline, not a growth.
		info.UsedMemory = 100 * mi
		got = Observe(got, input(3*time.Minute, info))
		assert.Equal(t, ReasonLow, Report(got, gi, 4*gi).Reason)
	})
}

func TestObserveWindows(t *testing.T) {
	// Three windows of 5 samples at 90 percent of maxmemory; the third ends at
	// the first sample after 15 minutes.
	s := observeAll(known(), 0, 900, 900, 900, 900, 900)
	assert.Equal(t, ReasonHigh, s.Reason, "the open window is high before it ends")
	assert.Zero(t, s.HighWindows)
	assert.Equal(t, 0.9, s.Window.PeakRatio)
	assert.Equal(t, 900*mi, s.Window.PeakUsed)

	s = observeAll(s, 5*time.Minute, 900)
	assert.Equal(t, int32(1), s.HighWindows)
	assert.Equal(t, ReasonHigh, s.Reason)
	assert.Equal(t, t0.Add(5*time.Minute).Unix(), s.EvaluatedAt)
	s = observeAll(s, 6*time.Minute, 900, 900, 900, 900, 900)
	assert.Equal(t, int32(2), s.HighWindows)
	assert.Zero(t, s.Recommended)
	s = observeAll(s, 11*time.Minute, 900, 900, 900, 900, 900)
	assert.Equal(t, int32(3), s.HighWindows)
	assert.Equal(t, 1280*mi, s.Recommended)
	assert.Equal(t, ReasonSustained, s.Reason)

	st := Report(s, gi, 4*gi)
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:          redisfailoverv1.AutoscalingWorkloadStore,
		RecommendedMemory: "1280Mi",
		Reason:            ReasonSustained,
		EvaluatedAt:       t0.Add(15 * time.Minute).UTC().Format(time.RFC3339),
		HighWindows:       3,
	}, st)

	// The recommendation stays while the windows stay high, and a low
	// window removes it. The sample at 15m is in the fourth window.
	s = observeAll(s, 16*time.Minute, 500, 500, 500, 500, 500)
	assert.Equal(t, int32(3), s.HighWindows, "the count stops at 3")
	assert.Equal(t, 1280*mi, s.Recommended)
	assert.Equal(t, ReasonSustained, s.Reason)
	s = observeAll(s, 21*time.Minute, 500, 500, 500, 500, 500)
	assert.Zero(t, s.HighWindows)
	assert.Zero(t, s.Recommended)
	assert.Equal(t, ReasonLow, s.Reason)
}

func TestObserveOnlyAPeakMakesAWindowHigh(t *testing.T) {
	s := observeAll(known(), 0, 100, 860, 100, 100, 100, 100)
	assert.Equal(t, int32(1), s.HighWindows)
	s = observeAll(s, 6*time.Minute, 840, 840, 840, 840, 840)
	assert.Zero(t, s.HighWindows, "a window below 0.85 resets the count")
}

// A peak of exactly 85 percent is high.
func TestObserveAPeakAtTheThresholdIsHigh(t *testing.T) {
	s := observeAll(known(), 0, 850, 100, 100, 100, 100, 100)
	assert.Equal(t, int32(1), s.HighWindows)
}

func TestObserveAGapEndsTheSequence(t *testing.T) {
	s := observeAll(known(), 0, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900)
	assert.Equal(t, int32(1), s.HighWindows)
	// The next sample comes more than one window after the window start.
	s = observeAll(s, 21*time.Minute, 900)
	assert.Zero(t, s.HighWindows)
	assert.Equal(t, t0.Add(21*time.Minute).Unix(), s.Window.Start)
}

func TestObserveFastPaths(t *testing.T) {
	t.Run("OOM errors", func(t *testing.T) {
		s := observeAll(known(), 0, 500)
		info := master(500)
		info.OOMErrors = 1
		s = Observe(s, input(time.Minute, info))
		assert.Equal(t, ReasonOOM, s.Reason)
		assert.Equal(t, 1280*mi, s.Recommended)
		assert.Equal(t, t0.Add(time.Minute).Unix(), s.EvaluatedAt)
		// No growth: no new recommendation, and the reason stays.
		s = Observe(s, input(2*time.Minute, info))
		assert.Equal(t, ReasonOOM, s.Reason)
		assert.Equal(t, t0.Add(time.Minute).Unix(), s.EvaluatedAt)
	})
	t.Run("95 percent", func(t *testing.T) {
		s := observeAll(known(), 0, 949)
		assert.Zero(t, s.Recommended)
		s = observeAll(s, time.Minute, 950)
		assert.Equal(t, ReasonUrgent, s.Reason)
		assert.Equal(t, 1280*mi, s.Recommended)
	})
	t.Run("without maxmemory the limit is the bound", func(t *testing.T) {
		info := master(0)
		info.MaxMemory, info.UsedMemory = 0, gi*96/100
		s := Observe(known(), input(0, info))
		assert.Equal(t, ReasonUrgent, s.Reason)
		assert.Equal(t, 0.96, UsedRatio(info, gi))
		assert.Zero(t, UsedRatio(info, 0))
	})
}

func TestObserveStep(t *testing.T) {
	tests := []struct {
		name       string
		limit, max int64
		want       int64
		reason     string
	}{
		{name: "25 percent", limit: gi, max: 4 * gi, want: 1280 * mi, reason: ReasonUrgent},
		{name: "rounded up to 64Mi", limit: 100 * mi, max: gi, want: 128 * mi, reason: ReasonUrgent},
		{name: "capped at memoryMax", limit: gi, max: 1100 * mi, want: 1100 * mi, reason: ReasonUrgent},
		{name: "at memoryMax", limit: 2 * gi, max: 2 * gi, reason: ReasonMaximum},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := input(0, master(990))
			in.Limit, in.Max = test.limit, test.max
			s := Observe(known(), in)
			assert.Equal(t, test.want, s.Recommended)
			assert.Equal(t, test.reason, s.Reason)
			assert.Equal(t, test.reason, Report(s, test.limit, test.max).Reason)
		})
	}
}

// A changed limit resets the windows. Thus a step that the user applies does
// not give a second step at the end of the window that was open at the step.
// Here maxmemory follows the limit, as with redis.maxMemory.
func TestObserveAppliedStepGivesNoSecondStep(t *testing.T) {
	s := observeAll(known(), 0, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900)
	require.Equal(t, 1280*mi, s.Recommended)

	applied := func(at time.Duration) Input {
		info := master(900)
		info.MaxMemory = 1250 * mi
		in := input(at, info)
		in.Limit = 1280 * mi
		return in
	}
	s = Observe(s, applied(17*time.Minute))
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:    redisfailoverv1.AutoscalingWorkloadStore,
		Reason:      ReasonStepIsActive,
		EvaluatedAt: t0.Add(17 * time.Minute).UTC().Format(time.RFC3339),
	}, Report(s, 1280*mi, 4*gi))
	for i := 18; i <= 40; i++ {
		s = Observe(s, applied(time.Duration(i)*time.Minute))
		assert.Empty(t, Report(s, 1280*mi, 4*gi).RecommendedMemory, "minute %d", i)
	}
	assert.Zero(t, s.Recommended, "the low windows remove the applied recommendation")
}

// With the use still high after a step, 3 windows with the changed limit
// give the next step.
func TestObserveNextStepNeedsThreeWindows(t *testing.T) {
	s := observeAll(known(), 0, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900)
	require.Equal(t, 1280*mi, s.Recommended)

	in := input(16*time.Minute, master(900))
	in.Limit = 1280 * mi
	for i := 16; i < 31; i++ {
		in.Now = t0.Add(time.Duration(i) * time.Minute)
		s = Observe(s, in)
		assert.Equal(t, 1280*mi, s.Recommended, "minute %d", i)
	}
	in.Now = t0.Add(31 * time.Minute)
	s = Observe(s, in)
	assert.Equal(t, int32(3), s.HighWindows)
	assert.Equal(t, "1600Mi", Report(s, in.Limit, in.Max).RecommendedMemory)
}

func TestObserveWorkload(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		policy   string
		evicted  int64
		workload string
	}{
		{name: "noeviction", spec: "Auto", policy: "noeviction", workload: "Store"},
		{name: "allkeys", spec: "Auto", policy: "allkeys-lru", workload: "Cache"},
		{name: "volatile without evictions", spec: "Auto", policy: "volatile-lru", workload: "Store"},
		{name: "volatile with evictions", spec: "Auto", policy: "volatile-ttl", evicted: 1, workload: "Cache"},
		{name: "explicit store", spec: "Store", policy: "allkeys-lru", workload: "Store"},
		{name: "explicit cache", spec: "Cache", policy: "noeviction", workload: "Cache"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := master(990)
			info.EvictedKeys = test.evicted
			in := input(0, info)
			in.Workload, in.Policy = test.spec, test.policy
			s := Observe(known(), in)
			assert.Equal(t, test.workload, s.Workload)
			if test.workload == "Cache" {
				assert.Equal(t, ReasonCache, s.Reason)
				assert.Zero(t, s.Recommended)
				assert.Zero(t, s.Window)
			} else {
				assert.Equal(t, ReasonUrgent, s.Reason)
			}
		})
	}
}

func TestReport(t *testing.T) {
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: ReasonNoSample}, Report(State{}, gi, 4*gi))
	s := State{Workload: "Store", Recommended: 1280 * mi, Reason: ReasonOOM, EvaluatedAt: t0.Add(59 * time.Second).Unix(), HighWindows: 2}
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:          "Store",
		RecommendedMemory: "1280Mi",
		Reason:            ReasonOOM,
		EvaluatedAt:       t0.UTC().Format(time.RFC3339),
		HighWindows:       2,
	}, Report(s, gi, 4*gi))
}

// The user can lower memoryMax or raise the limit after a recommendation.
// The status never shows a value above memoryMax or at the limit.
func TestReportClampsToTheCurrentSpec(t *testing.T) {
	s := State{Workload: "Store", Recommended: 1280 * mi, Reason: ReasonUrgent}
	assert.Equal(t, "1100Mi", Report(s, gi, 1100*mi).RecommendedMemory, "memoryMax lowered below the recommendation")
	st := Report(s, gi, gi)
	assert.Empty(t, st.RecommendedMemory, "memoryMax lowered to the limit")
	assert.Equal(t, ReasonMaximum, st.Reason)
	st = Report(s, 1280*mi, 4*gi)
	assert.Empty(t, st.RecommendedMemory)
	assert.Equal(t, ReasonStepIsActive, st.Reason)
}

// At the next trigger, the recommendation follows a lowered memoryMax.
func TestObserveRecomputesTheRecommendation(t *testing.T) {
	s := Observe(known(), input(0, master(990)))
	assert.Equal(t, 1280*mi, s.Recommended)
	in := input(time.Minute, master(990))
	in.Max = 1100 * mi
	s = Observe(s, in)
	assert.Equal(t, 1100*mi, s.Recommended)
	assert.Equal(t, "1100Mi", Report(s, in.Limit, in.Max).RecommendedMemory)
}

// With the limit at memoryMax, the reason must not change between the window
// ends, or each change writes the status.
func TestObserveAtTheMaximumIsStable(t *testing.T) {
	sample := func(s State, at time.Duration) State {
		in := input(at, master(900))
		in.Limit, in.Max = 2*gi, 2*gi
		return Observe(s, in)
	}
	s := known()
	var statuses []redisfailoverv1.AutoscalingStatus
	for i := range 31 {
		s = sample(s, time.Duration(i)*time.Minute)
		if i >= 15 {
			statuses = append(statuses, Report(s, 2*gi, 2*gi))
		}
	}
	assert.Equal(t, ReasonMaximum, statuses[0].Reason)
	assert.Empty(t, statuses[0].RecommendedMemory)
	for _, st := range statuses {
		assert.Equal(t, statuses[0], st)
	}
}

// A steady low use also gives a stable status.
func TestObserveLowIsStable(t *testing.T) {
	s := observeAll(known(), 0, 500)
	first := Report(s, gi, 4*gi)
	for i := 1; i <= 20; i++ {
		s = observeAll(s, time.Duration(i)*time.Minute, 500)
		assert.Equal(t, first, Report(s, gi, 4*gi))
	}
}

// CONFIG RESETSTAT sets errorstat_OOM to 0 without a restart.
func TestObserveOOMCounterReset(t *testing.T) {
	s := known()
	s.OOMErrors = 5
	info := master(500)
	s = Observe(s, input(0, info))
	assert.Zero(t, s.Recommended)
	assert.Zero(t, s.OOMErrors)
	info.OOMErrors = 1
	s = Observe(s, input(time.Minute, info))
	assert.Equal(t, ReasonOOM, s.Reason)
}

// The 3-window rule needs a sample in each window. With samples 10 minutes
// apart, for example --sync-interval 600 without pod events, each window end
// is also a gap.
func TestObserveSampleSpacing(t *testing.T) {
	for _, tc := range []struct {
		every time.Duration
		want  int64
	}{{9 * time.Minute, 1280 * mi}, {10 * time.Minute, 0}} {
		s := known()
		for i := range 7 {
			s = observeAll(s, time.Duration(i)*tc.every, 900)
		}
		assert.Equal(t, tc.want, s.Recommended, tc.every.String())
	}
}

func TestStateNeedsWrite(t *testing.T) {
	s := known()
	s.Window = Window{Start: t0.Unix(), PeakRatio: 0.5, PeakUsed: 500 * mi}
	other := s
	other.LastSample++
	other.Window.PeakRatio, other.Window.PeakUsed = 0.84, 840*mi
	assert.False(t, other.NeedsWrite(s), "a growth below the threshold")
	other.Window.PeakRatio = 0.85
	assert.True(t, other.NeedsWrite(s), "the open window becomes high")
	other = s
	other.Window.Start++
	assert.True(t, other.NeedsWrite(s), "a new window")
	other = s
	other.OOMErrors++
	assert.True(t, other.NeedsWrite(s))
}

// A restart or a change to a cache resets highWindows, and the status then
// shows the time of that change.
func TestObserveStampsAResetOfHighWindows(t *testing.T) {
	high := func() State {
		// highWindows is 2 at 10m.
		return observeAll(known(), 0, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900)
	}
	s := high()
	assert.Equal(t, int32(2), s.HighWindows)
	assert.Equal(t, t0.Add(10*time.Minute).Unix(), s.EvaluatedAt)

	restarted := master(900)
	restarted.RunID = "run2"
	got := Observe(s, input(12*time.Minute, restarted))
	assert.Zero(t, got.HighWindows)
	assert.Equal(t, t0.Add(12*time.Minute).UTC().Format(time.RFC3339), Report(got, gi, 4*gi).EvaluatedAt)

	cache := input(12*time.Minute, master(900))
	cache.Workload = redisfailoverv1.AutoscalingWorkloadCache
	got = Observe(high(), cache)
	assert.Zero(t, got.HighWindows)
	assert.Equal(t, t0.Add(12*time.Minute).Unix(), got.EvaluatedAt)
}

func TestExpire(t *testing.T) {
	s := observeAll(known(), 0, 990)
	s.OOMErrors, s.HighWindows = 4, 2
	require.NotZero(t, s.Recommended)

	assert.Equal(t, s, Expire(s, t0.Add(MaxStateAge)), "a state of the bound age continues")
	assert.Equal(t, State{}, Expire(State{}, t0.Add(time.Hour)), "no state")

	got := Expire(s, t0.Add(MaxStateAge+time.Second))
	assert.Equal(t, State{
		Version:     StateVersion,
		Workload:    s.Workload,
		Reason:      ReasonNoSample,
		EvaluatedAt: t0.Add(MaxStateAge + time.Second).Unix(),
		LastSample:  s.LastSample,
		Limit:       s.Limit,
	}, got)
	assert.Empty(t, Report(got, gi, 4*gi).RecommendedMemory)

	// The next sample sets the OOM baseline: no restart and no OOM step.
	info := master(500)
	info.OOMErrors = 9
	got = Observe(got, input(MaxStateAge+time.Minute, info))
	assert.Equal(t, ReasonLow, got.Reason)
	assert.Equal(t, int64(9), got.OOMErrors)
}

func TestStateNeedsWriteRefreshesTheSampleTime(t *testing.T) {
	s := known()
	s.LastSample = t0.Unix()
	later := s
	later.LastSample += int64(WindowLength/time.Second) - 1
	assert.False(t, later.NeedsWrite(s))
	later.LastSample++
	assert.True(t, later.NeedsWrite(s))
}

// A recommendation is a step from the limit of its sample. After the user
// lowers the limit, the status must not show the step from the higher limit.
func TestObserveALoweredLimitClearsTheRecommendation(t *testing.T) {
	s := Observe(known(), input(0, master(990)))
	require.Equal(t, "1280Mi", Report(s, gi, 4*gi).RecommendedMemory)

	in := input(time.Minute, master(100))
	in.Limit = 512 * mi
	s = Observe(s, in)

	assert.Zero(t, s.Recommended)
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:    redisfailoverv1.AutoscalingWorkloadStore,
		Reason:      ReasonLow,
		EvaluatedAt: t0.Add(time.Minute).UTC().Format(time.RFC3339),
	}, Report(s, in.Limit, in.Max))
}

// A reconcile without a sample reports the state of the last sample. The
// status must not show the step from a limit that the user lowered since.
func TestReportHidesAStepFromAHigherLimit(t *testing.T) {
	s := Observe(known(), input(0, master(990)))
	require.Equal(t, gi, s.Limit)

	st := Report(s, 512*mi, 4*gi)

	assert.Empty(t, st.RecommendedMemory)
	assert.Equal(t, ReasonUrgent, st.Reason)
	assert.Equal(t, "1280Mi", Report(s, 1100*mi, 4*gi).RecommendedMemory, "a raised limit keeps the step")
}

// After the clock went back, the open window must end one WindowLength after
// its first sample on the clock that the operator reads now.
func TestObserveAfterTheClockWentBack(t *testing.T) {
	s := observeAll(known(), 0, 900, 900)
	require.Equal(t, t0.Unix(), s.Window.Start)

	s = observeAll(s, -time.Hour, 900, 900, 900, 900, 900, 900)

	assert.Equal(t, t0.Add(-55*time.Minute).Unix(), s.Window.Start)
	assert.Equal(t, int32(1), s.HighWindows)
}
