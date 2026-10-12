package autoscale

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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

// After the user applies a step, the count stays until the next window ends,
// so the windows after the step decide on the next step.
func TestObserveNextStepWaitsForAWindow(t *testing.T) {
	s := observeAll(known(), 0, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900, 900)
	assert.Equal(t, 1280*mi, s.Recommended)

	in := input(16*time.Minute, master(900))
	in.Limit = 1280 * mi
	s = Observe(s, in)
	assert.Equal(t, 1280*mi, s.Recommended)
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:    redisfailoverv1.AutoscalingWorkloadStore,
		Reason:      ReasonStepIsActive,
		EvaluatedAt: t0.Add(15 * time.Minute).UTC().Format(time.RFC3339),
		HighWindows: 3,
	}, Report(s, in.Limit, in.Max))

	for i := 17; i <= 21; i++ {
		in.Now = t0.Add(time.Duration(i) * time.Minute)
		s = Observe(s, in)
	}
	assert.Equal(t, int32(3), s.HighWindows)
	assert.Equal(t, 1600*mi, s.Recommended)
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
	for i := 0; i <= 30; i++ {
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
		for i := 0; i <= 6; i++ {
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
