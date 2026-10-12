// Package autoscale holds the state and the rules of the memory autoscaler.
// It has no Kubernetes or Redis calls, so the rules can be tested as pure
// functions.
package autoscale

import (
	"math"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/redis"
)

// The rule constants. A change of WindowLength or HighWindowsForStep changes
// the time from a sustained high use to a recommendation.
const (
	// SampleInterval is the minimum time between two samples. Pod events
	// start reconciles in bursts, and each sample costs one INFO call.
	SampleInterval = time.Minute
	// WindowLength is the length of one evaluation window.
	WindowLength = 5 * time.Minute
	// MinUptime keeps samples of a restarted Redis out. Its memory use is
	// low until the clients write the data again.
	MinUptime = 10 * time.Minute
	// HighRatio is the used_memory / maxmemory peak that makes a window high.
	HighRatio = 0.85
	// UrgentRatio gives a recommendation at once. Above it, a store refuses
	// writes soon.
	UrgentRatio = 0.95
	// HighWindowsForStep is the number of consecutive high windows for a
	// recommendation.
	HighWindowsForStep = 3
	// StepPercent is the increase of the memory limit in one step.
	StepPercent = 25
	// StepAlignment rounds the recommended limit up, so that small limits get
	// a useful step.
	StepAlignment = 64 << 20
	// StateVersion changes when State changes in a way that an older
	// operator cannot read.
	StateVersion = 1
)

// The reasons in the status. They are constant texts, so that the status changes
// only when the outcome changes.
const (
	ReasonNoSample     = "no sample yet"
	ReasonNotHealthy   = "the RedisFailover is not Healthy"
	ReasonNoInfo       = "no INFO sample of the master"
	ReasonNotMaster    = "the sampled pod is not the master"
	ReasonRestarted    = "Redis restarted, the windows start again"
	ReasonWarmUp       = "Redis runs for less than 10 minutes"
	ReasonLoading      = "Redis loads its data"
	ReasonSyncing      = "a sync with the master is in progress"
	ReasonCache        = "cache decisions are not available"
	ReasonLow          = "memory use is below the threshold"
	ReasonHigh         = "memory use is high"
	ReasonSustained    = "memory use is high in 3 consecutive windows"
	ReasonUrgent       = "memory use is at 95 percent or more"
	ReasonOOM          = "Redis refused commands with OOM"
	ReasonMaximum      = "the limit is at memoryMax"
	ReasonStepIsActive = "the limit has the recommended value"
	ReasonNewerState   = "the state ConfigMap has a newer version"
	ReasonForeignState = "the state ConfigMap has another owner"
)

// Window is the peak memory use in one evaluation window.
type Window struct {
	// Start is a Unix time in seconds. 0 means no window.
	Start     int64   `json:"start"`
	PeakRatio float64 `json:"peakRatio"`
	// PeakUsed is rounded up to MiB, so that a small growth of used_memory
	// does not write the state.
	PeakUsed int64 `json:"peakUsed"`
}

// State is the content of the state ConfigMap. It holds only scalar fields,
// so that NeedsWrite can compare it with ==.
type State struct {
	Version     int    `json:"version"`
	Workload    string `json:"workload,omitempty"`
	Window      Window `json:"window"`
	HighWindows int32  `json:"highWindows"`
	RunID       string `json:"runID,omitempty"`
	OOMErrors   int64  `json:"oomErrors"`
	// Recommended is the recommended memory limit in bytes, or 0.
	Recommended int64  `json:"recommended,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// EvaluatedAt and LastSample are Unix times in seconds.
	EvaluatedAt int64 `json:"evaluatedAt,omitempty"`
	LastSample  int64 `json:"lastSample,omitempty"`
}

// NeedsWrite reports whether s has a change that a decision uses. The sample
// time and the exact peaks of the open window change at most samples, and a
// write for each sample costs one API call. Of the open window, only its start
// and whether its peak is high are decisions.
func (s State) NeedsWrite(stored State) bool {
	return s.decisionView() != stored.decisionView()
}

func (s State) decisionView() State {
	high := s.Window.PeakRatio >= HighRatio
	s.LastSample, s.Window.PeakRatio, s.Window.PeakUsed = 0, 0, 0
	if high {
		s.Window.PeakRatio = HighRatio
	}
	return s
}

// Input is what the handler knows at a sample.
type Input struct {
	Now     time.Time
	Healthy bool
	// Info is nil when the master did not answer INFO, or no master is known.
	Info *redis.AutoscalerInfo
	// Limit is the memory limit of redis.resources, and Max is memoryMax.
	Limit, Max int64
	// Workload and Policy come from the spec.
	Workload, Policy string
}

// Observe adds one sample to the state and returns the next state.
func Observe(s State, in Input) State {
	s.Version = StateVersion
	now := in.Now.Unix()
	s.LastSample = now
	info := in.Info
	switch {
	case !in.Healthy:
		return s.withReason(ReasonNotHealthy)
	case info == nil:
		return s.withReason(ReasonNoInfo)
	case info.Role != "master":
		return s.withReason(ReasonNotMaster)
	}
	if info.RunID != s.RunID {
		// A restart loses the data or loads it again, so the earlier windows
		// do not show the memory use after the restart. The first sample only
		// sets the baseline of the OOM count.
		restarted := s.RunID != ""
		s.RunID, s.OOMErrors = info.RunID, info.OOMErrors
		s.HighWindows, s.Window = 0, Window{}
		s.setRecommended(0, now)
		if restarted {
			return s.withReason(ReasonRestarted)
		}
	}
	switch {
	case info.UptimeSeconds < int64(MinUptime/time.Second):
		return s.withReason(ReasonWarmUp)
	case info.Loading:
		return s.withReason(ReasonLoading)
	case info.SyncInProgress:
		return s.withReason(ReasonSyncing)
	}

	// CONFIG RESETSTAT sets the counter to 0 without a restart. Only a growth
	// is a refused command.
	oom := info.OOMErrors > s.OOMErrors
	s.OOMErrors = info.OOMErrors
	s.Workload = workload(in.Workload, in.Policy, info)
	if s.Workload == redisfailoverv1.AutoscalingWorkloadCache {
		s.HighWindows, s.Window = 0, Window{}
		s.setRecommended(0, now)
		return s.withReason(ReasonCache)
	}

	closed := s.Window.Start != 0 && now >= s.Window.Start+int64(WindowLength/time.Second)
	if closed {
		s.closeWindow(now)
	}
	if s.Window.Start == 0 {
		s.Window.Start = now
	}
	r := usedRatio(info, in.Limit)
	s.Window.PeakRatio = max(s.Window.PeakRatio, r)
	s.Window.PeakUsed = max(s.Window.PeakUsed, roundUp(info.UsedMemory, 1<<20))

	switch {
	case oom:
		s.recommend(in.Limit, in.Max, now, ReasonOOM)
	case r >= UrgentRatio:
		s.recommend(in.Limit, in.Max, now, ReasonUrgent)
	case closed && s.HighWindows >= HighWindowsForStep:
		// Only at the end of a window, so that the windows after a step
		// show the effect of the step first.
		s.recommend(in.Limit, in.Max, now, ReasonSustained)
	case s.Recommended != 0:
		// Keep the recommendation and its reason until a low window.
	case s.HighWindows == 0 && s.Window.PeakRatio < HighRatio:
		s.Reason = ReasonLow
	case in.Limit >= in.Max:
		// The same reason as at the end of a window, so that the status
		// does not change between the windows.
		s.Reason = ReasonMaximum
	default:
		s.Reason = ReasonHigh
	}
	return s
}

// Report returns the status view of the state for the current limit and
// memoryMax. The user can change both after the recommendation. The status
// never shows a value above memoryMax, or a value that the limit has.
func Report(s State, limit, maxLimit int64) redisfailoverv1.AutoscalingStatus {
	st := redisfailoverv1.AutoscalingStatus{
		Workload:    s.Workload,
		Reason:      s.Reason,
		HighWindows: s.HighWindows,
	}
	if st.Reason == "" {
		st.Reason = ReasonNoSample
	}
	if s.EvaluatedAt != 0 {
		st.EvaluatedAt = time.Unix(s.EvaluatedAt, 0).UTC().Truncate(time.Minute).Format(time.RFC3339)
	}
	shown := min(s.Recommended, maxLimit)
	switch {
	case shown > limit:
		st.RecommendedMemory = formatQuantity(shown)
	case s.Recommended != 0 && limit >= maxLimit:
		st.Reason = ReasonMaximum
	case s.Recommended != 0:
		st.Reason = ReasonStepIsActive
	}
	return st
}

// UsedRatio returns the metric value of a sample.
func UsedRatio(info *redis.AutoscalerInfo, limit int64) float64 {
	return usedRatio(info, limit)
}

// usedRatio compares used_memory with maxmemory. Without maxmemory, Redis has
// no own bound, so the memory limit is the bound.
func usedRatio(info *redis.AutoscalerInfo, limit int64) float64 {
	bound := info.MaxMemory
	if bound == 0 {
		bound = limit
	}
	if bound <= 0 {
		return 0
	}
	// Three decimals keep the state stable between samples.
	return math.Round(float64(info.UsedMemory)/float64(bound)*1000) / 1000
}

// workload derives Store or Cache. A volatile-* policy that evicted a key
// behaves like a cache: the clients accept the loss of keys.
func workload(spec, policy string, info *redis.AutoscalerInfo) string {
	switch {
	case spec == redisfailoverv1.AutoscalingWorkloadStore || spec == redisfailoverv1.AutoscalingWorkloadCache:
		return spec
	case strings.HasPrefix(policy, "allkeys-"):
		return redisfailoverv1.AutoscalingWorkloadCache
	case strings.HasPrefix(policy, "volatile-") && info.EvictedKeys > 0:
		return redisfailoverv1.AutoscalingWorkloadCache
	}
	return redisfailoverv1.AutoscalingWorkloadStore
}

// closeWindow counts the window that ended. A window without a sample, for
// example while the RedisFailover was not Healthy, ends the sequence of
// consecutive high windows. The count stops at HighWindowsForStep, so that
// the status does not change at each window while the use stays high.
func (s *State) closeWindow(now int64) {
	count := s.HighWindows
	if s.Window.PeakRatio >= HighRatio {
		count = min(count+1, HighWindowsForStep)
	} else {
		count = 0
		s.setRecommended(0, now)
	}
	if now >= s.Window.Start+2*int64(WindowLength/time.Second) {
		count = 0
	}
	if count != s.HighWindows {
		s.HighWindows = count
		s.EvaluatedAt = now
	}
	s.Window = Window{}
}

func (s *State) recommend(limit, maxLimit, now int64, reason string) {
	target := min(roundUp(limit+limit*StepPercent/100, StepAlignment), maxLimit)
	if target <= limit {
		s.setRecommended(0, now)
		s.Reason = ReasonMaximum
		return
	}
	if s.Recommended != target {
		s.setRecommended(target, now)
		s.Reason = reason
	}
}

func (s *State) setRecommended(bytes, now int64) {
	if s.Recommended != bytes {
		s.Recommended = bytes
		s.EvaluatedAt = now
	}
}

func (s State) withReason(reason string) State {
	s.Reason = reason
	return s
}

func formatQuantity(bytes int64) string {
	return resource.NewQuantity(bytes, resource.BinarySI).String()
}

func roundUp(n, multiple int64) int64 {
	return (n + multiple - 1) / multiple * multiple
}
