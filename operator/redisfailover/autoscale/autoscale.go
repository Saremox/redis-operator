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
	// MaxStateAge is the age of the last sample after which a state is
	// discarded. NeedsWrite writes the sample time at least one time in each
	// window, and a sample can come one SampleInterval late. An older state,
	// for example of a mode change to Off and back, shows an earlier memory use.
	MaxStateAge = 2*WindowLength + SampleInterval
	// StateVersion changes when State changes in a way that an older
	// operator cannot read. Version 2 adds the step fields. An operator that
	// reads only version 1 would write the state without them, and the pods
	// would get the spec limit again.
	StateVersion = 2
)

// The reasons in the status. They are constant texts, so that the status changes
// only when the outcome changes.
const (
	// ReasonNoSample tells that the autoscaler has no sample yet.
	ReasonNoSample = "no sample yet"
	// ReasonNotHealthy tells that the RedisFailover is not Healthy.
	ReasonNotHealthy = "the RedisFailover is not Healthy"
	// ReasonNoInfo tells that the sample has no INFO of the master.
	ReasonNoInfo = "no INFO sample of the master"
	// ReasonNotMaster tells that the sampled pod is not the master.
	ReasonNotMaster = "the sampled pod is not the master"
	// ReasonRestarted tells that Redis restarted and the windows start again.
	ReasonRestarted = "Redis restarted, the windows start again"
	// ReasonWarmUp tells that Redis runs for less than 10 minutes.
	ReasonWarmUp = "Redis runs for less than 10 minutes"
	// ReasonLoading tells that Redis loads its data.
	ReasonLoading = "Redis loads its data"
	// ReasonSyncing tells that a sync with the master is in progress.
	ReasonSyncing = "a sync with the master is in progress"
	// ReasonCache tells that the autoscaler makes no decision for a cache.
	ReasonCache = "cache decisions are not available"
	// ReasonLow tells that the memory use is below the threshold.
	ReasonLow = "memory use is below the threshold"
	// ReasonHigh tells that the memory use is high, but not yet for a step.
	ReasonHigh = "memory use is high"
	// ReasonSustained tells that the memory use is high in 3 windows in a
	// row.
	ReasonSustained = "memory use is high in 3 consecutive windows"
	// ReasonUrgent tells that the memory use is at 95 percent or more.
	ReasonUrgent = "memory use is at 95 percent or more"
	// ReasonOOM tells that Redis refused commands with OOM.
	ReasonOOM = "Redis refused commands with OOM"
	// ReasonMaximum tells that the limit is at memoryMax.
	ReasonMaximum = "the limit is at memoryMax"
	// ReasonStepIsActive tells that the limit has the recommended value.
	ReasonStepIsActive = "the limit has the recommended value"
	// ReasonNewerState tells that the state ConfigMap has a newer version.
	ReasonNewerState = "the state ConfigMap has a newer version"
	// ReasonForeignState tells that the state ConfigMap has another owner.
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
	// Limit is the memory limit of the last sample in bytes. An operator
	// that does not know the field ignores it, so StateVersion stays.
	Limit int64 `json:"limit,omitempty"`

	// The step fields. Expire keeps them, because the pods have the
	// Effective limit also when no operator samples. Effective is 0 when the
	// pods have the spec limit, and SpecLimit is the spec limit when
	// Effective was set. The times are Unix times in seconds.
	Effective  int64  `json:"effective,omitempty"`
	SpecLimit  int64  `json:"specLimit,omitempty"`
	Phase      string `json:"phase,omitempty"`
	From       int64  `json:"from,omitempty"`
	To         int64  `json:"to,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	DoneAt     int64  `json:"doneAt,omitempty"`
	RetryAt    int64  `json:"retryAt,omitempty"`
	Refusal    string `json:"refusal,omitempty"`
	// Reverts is the number of reverts since the last user change. It
	// doubles the back-off.
	Reverts int32 `json:"reverts,omitempty"`
	// Steps holds the start times of the last StepBudget steps, the oldest
	// first. An array keeps State comparable with ==.
	Steps [StepBudget]int64 `json:"steps,omitzero"`
	// Frozen marks an Effective that mode Auto took from the StatefulSet
	// after the state was lost. The cause of the lost state is not known, so
	// no step starts until the spec changes at Generation.
	Frozen bool `json:"frozen,omitempty"`
	// RevertApplied marks a Reverted state that Ensure applied. Until then,
	// the StatefulSet can create pods from the template of the step.
	RevertApplied bool `json:"revertApplied,omitempty"`
}

// Expire discards the decision values of a state with a last sample older than
// MaxStateAge. Without a run_id, the next sample sets the OOM baseline again
// and is not a restart.
func Expire(s State, now time.Time) State {
	if s.LastSample == 0 || now.Unix()-s.LastSample <= int64(MaxStateAge/time.Second) {
		return s
	}
	at := now.Unix()
	s.RunID, s.OOMErrors, s.Window = "", 0, Window{}
	setHighWindows(&s, 0, at)
	setRecommended(&s, 0, at)
	s.Reason = ReasonNoSample
	return s
}

// NeedsWrite reports whether s has a change that a decision uses. The sample
// time and the exact peaks of the open window change at most samples, and a
// write for each sample costs one API call. Of the open window, only its start
// and whether its peak is high are decisions. The sample time is written at
// least one time in each window, so that Expire finds only a state that no
// operator samples.
func (s State) NeedsWrite(stored State) bool {
	return s.decisionView() != stored.decisionView() || s.LastSample-stored.LastSample >= int64(WindowLength/time.Second)
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
	// Limit is the memory limit of the pods: the effective limit, which is
	// the limit of redis.resources or a larger limit of a step. Max is
	// memoryMax.
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
	if reason := skipReason(in); reason != "" {
		return s.withReason(reason)
	}
	if observeRunID(&s, info, now) {
		return s.withReason(ReasonRestarted)
	}
	if reason := warmUpReason(info); reason != "" {
		return s.withReason(reason)
	}

	// CONFIG RESETSTAT sets the counter to 0 without a restart. Only a growth
	// is a refused command.
	oom := info.OOMErrors > s.OOMErrors
	s.OOMErrors = info.OOMErrors
	s.Workload = workload(in.Workload, in.Policy, info)
	if s.Workload == redisfailoverv1.AutoscalingWorkloadCache {
		s.Window = Window{}
		setHighWindows(&s, 0, now)
		setRecommended(&s, 0, now)
		return s.withReason(ReasonCache)
	}

	closed := advanceWindow(&s, in.Limit, now)
	r := usedRatio(info, in.Limit)
	s.Window.PeakRatio = max(s.Window.PeakRatio, r)
	s.Window.PeakUsed = max(s.Window.PeakUsed, roundUp(info.UsedMemory, 1<<20))

	evaluate(&s, in, now, oom, r, closed)
	return s
}

// skipReason returns why the input gives no sample of the master, or "".
func skipReason(in Input) string {
	info := in.Info
	switch {
	case !in.Healthy:
		return ReasonNotHealthy
	case info == nil:
		return ReasonNoInfo
	case info.Role != "master":
		return ReasonNotMaster
	}
	return ""
}

// observeRunID starts the windows again when the run_id of Redis changed. It
// reports whether Redis restarted after an earlier sample.
func observeRunID(s *State, info *redis.AutoscalerInfo, now int64) bool {
	if info.RunID == s.RunID {
		return false
	}
	// A restart loses the data or loads it again, so the earlier windows
	// do not show the memory use after the restart. The first sample only
	// sets the baseline of the OOM count.
	restarted := s.RunID != ""
	s.RunID, s.OOMErrors = info.RunID, info.OOMErrors
	s.Window = Window{}
	setHighWindows(s, 0, now)
	setRecommended(s, 0, now)
	return restarted
}

// warmUpReason returns why the memory use of Redis does not count yet, or "".
func warmUpReason(info *redis.AutoscalerInfo) string {
	switch {
	case info.UptimeSeconds < int64(MinUptime/time.Second):
		return ReasonWarmUp
	case info.Loading:
		return ReasonLoading
	case info.SyncInProgress:
		return ReasonSyncing
	}
	return ""
}

// advanceWindow starts the window again after a change of the limit or of the
// clock, and closes the window that ended. It reports whether a window closed.
func advanceWindow(s *State, limit, now int64) bool {
	if s.Limit != limit {
		// The peaks of the earlier limit do not show the use with this one.
		// Without the reset, the window open at a step ends high, and a
		// second step follows at once.
		if limit < s.Limit {
			// The recommendation is a step from the earlier, higher limit.
			setRecommended(s, 0, now)
		}
		s.Limit, s.Window = limit, Window{}
		setHighWindows(s, 0, now)
	}
	if s.Window.Start > now {
		// The clock went back. The window would stay open until the clock
		// reaches its end.
		s.Window = Window{}
	}
	closed := s.Window.Start != 0 && now >= s.Window.Start+int64(WindowLength/time.Second)
	if closed {
		closeWindow(s, now)
	}
	if s.Window.Start == 0 {
		s.Window.Start = now
	}
	return closed
}

// evaluate sets the recommendation and the reason of the sample with the used
// ratio r.
func evaluate(s *State, in Input, now int64, oom bool, r float64, closed bool) {
	switch {
	case oom:
		recommend(s, in.Limit, in.Max, now, ReasonOOM)
	case r >= UrgentRatio:
		recommend(s, in.Limit, in.Max, now, ReasonUrgent)
	case closed && s.HighWindows >= HighWindowsForStep:
		// Only at the end of a window. A change of the limit resets the
		// count, so 3 windows with the changed limit decide on a next step.
		recommend(s, in.Limit, in.Max, now, ReasonSustained)
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
}

// Report returns the status view of the state for the current limit and
// memoryMax. The user can change both after the recommendation. The status
// never shows a value above memoryMax, a value that the limit has, or a step
// from a higher limit.
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
		st.EvaluatedAt = formatMinute(s.EvaluatedAt)
	}
	recommended := s.Recommended
	if s.Limit != 0 && limit < s.Limit {
		// Observe clears a step from a higher limit only at the next sample.
		recommended = 0
	}
	shown := min(recommended, maxLimit)
	switch {
	case shown > limit:
		st.RecommendedMemory = formatQuantity(shown)
	case recommended != 0 && limit >= maxLimit:
		st.Reason = ReasonMaximum
	case recommended != 0:
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
func closeWindow(s *State, now int64) {
	count := s.HighWindows
	if s.Window.PeakRatio >= HighRatio {
		count = min(count+1, HighWindowsForStep)
	} else {
		count = 0
		setRecommended(s, 0, now)
	}
	if now >= s.Window.Start+2*int64(WindowLength/time.Second) {
		count = 0
	}
	setHighWindows(s, count, now)
	s.Window = Window{}
}

// setHighWindows stamps EvaluatedAt only on a change, as setRecommended.
func setHighWindows(s *State, count int32, now int64) {
	if s.HighWindows != count {
		s.HighWindows = count
		s.EvaluatedAt = now
	}
}

func recommend(s *State, limit, maxLimit, now int64, reason string) {
	target := min(roundUp(limit+limit*StepPercent/100, StepAlignment), maxLimit)
	if target <= limit {
		setRecommended(s, 0, now)
		s.Reason = ReasonMaximum
		return
	}
	if s.Recommended != target {
		setRecommended(s, target, now)
		s.Reason = reason
	}
}

func setRecommended(s *State, bytes, now int64) {
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

func formatMinute(unix int64) string {
	return time.Unix(unix, 0).UTC().Truncate(time.Minute).Format(time.RFC3339)
}

func roundUp(n, multiple int64) int64 {
	return (n + multiple - 1) / multiple * multiple
}
