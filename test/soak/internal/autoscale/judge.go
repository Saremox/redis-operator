// Package autoscale judges the steps of the memory autoscaler in mode Auto
// by the rules that the operator documents. It reads only what the observer
// reads of an instance, never the operator code under test.
package autoscale

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// The step rules of the operator documentation.
const (
	stepPercent  = 25
	stepAlign    = 64 << 20
	applyTimeout = 15 * time.Minute
	// cooldown is the cooldown after an urgent step. The status does not tell
	// an urgent step from a sustained one, so each step gets this bound.
	cooldown     = 5 * time.Minute
	budget       = 6
	budgetPeriod = 24 * time.Hour
	backOff      = time.Hour
	maxBackOff   = 24 * time.Hour
	// minute is the resolution of the times in the status.
	minute = time.Minute
	// slack covers the age of a status read and the clock difference of the
	// tester and the operator.
	slack = 30 * time.Second
	// timeoutMargin covers the reconcile that ends a step after its timeout.
	timeoutMargin = 2 * time.Minute
)

// The phases of a step in status.autoscaling.phase.
const (
	PhaseApplying = "Applying"
	PhaseDone     = "Done"
	PhaseReverted = "Reverted"
	PhaseHeld     = "Held"
)

// The judges, the values of the invariant label of a finding.
const (
	JudgeSize      = "autoscale_step_size"
	JudgeTimeout   = "autoscale_step_timeout"
	JudgeRate      = "autoscale_step_rate"
	JudgeRestart   = "autoscale_restart"
	JudgeHandover  = "autoscale_handover"
	JudgeShrink    = "autoscale_shrink"
	JudgeMaxMemory = "autoscale_maxmemory"
	// JudgeStatus is a status field that the operator never writes: a time
	// or a quantity that does not parse, an unknown phase, or a lastStep that
	// is missing, in the future or before the last one.
	JudgeStatus = "autoscale_status"
)

// Judges lists each judge.
var Judges = []string{JudgeSize, JudgeTimeout, JudgeRate, JudgeRestart, JudgeHandover, JudgeShrink, JudgeMaxMemory, JudgeStatus}

// The step results, the values of the result label.
const (
	ResultStarted  = "started"
	ResultDone     = "done"
	ResultReverted = "reverted"
	ResultHeld     = "held"
)

// Results lists each step result.
var Results = []string{ResultStarted, ResultDone, ResultReverted, ResultHeld}

// Allow is what a user path may do to the redis pods. A step may do none of
// it.
type Allow struct {
	// Recreate also covers a restart of the redis container.
	Recreate, Shrink, Handover bool
	// Pod limits Recreate and Shrink to the pod of that name, for example the
	// pod that a mutation deletes. Its replacement has the same name. "" is
	// each pod.
	Pod string
}

// forPod returns what a may do to the pod name. Handover is about the master
// role, so it stays.
func (a Allow) forPod(name string) Allow {
	if a.Pod != "" && a.Pod != name {
		a.Recreate, a.Shrink = false, false
	}
	a.Pod = ""
	return a
}

// AllowAll is a user path that may change the pods in each way, for example
// a chaos action or a change of the spec by hand.
var AllowAll = Allow{Recreate: true, Shrink: true, Handover: true}

func (a Allow) or(b Allow) Allow {
	return Allow{Recreate: a.Recreate || b.Recreate, Shrink: a.Shrink || b.Shrink, Handover: a.Handover || b.Handover}
}

// Pod is a redis pod in one round.
type Pod struct {
	Name, UID string
	// Restarts is the restart count of the redis container.
	Restarts int32
	// Limit is the applied memory limit of the redis container, and MaxMemory
	// the maxmemory of the server. 0 is not known.
	Limit, MaxMemory int64
}

// Round is what the observer read of an instance in one round.
type Round struct {
	Now        time.Time
	Generation int64
	// Spec is the memory limit of redis.resources, and Max is memoryMax.
	Spec, Max int64
	Status    redisfailoverv1.AutoscalingStatus
	Pods      []Pod
	// Master is the name of the single master, or "".
	Master string
	// Allow is what the user paths of this round may do to the pods.
	Allow Allow
}

// Event is a step result, or a violation of a judge.
type Event struct {
	// Judge is the violated judge, or "" for a step result.
	Judge  string
	Result string
	// Step is the lastStep of the step.
	Step    string
	Message string
}

// baseline is the last state of a pod or of the master. allow and active
// collect the rounds since then, because a change can show one round late.
type baseline struct {
	uid      string
	restarts int32
	limit    int64
	allow    Allow
	active   bool
}

// Judge follows the steps of one instance across the rounds of the observer.
type Judge struct {
	prev   *Round
	pods   map[string]baseline
	master string
	// masterSince collects the rounds since the master was last seen.
	masterSince baseline
	// step is the lastStep of the current step, and phase its last phase.
	// ended is true after the first result of the step.
	step, phase     string
	ended, timedOut bool
	// applyingSince is the first round that saw the phase Applying, zero in
	// another phase.
	applyingSince time.Time
	// starts are the start times of the steps for the budget.
	starts []time.Time
	// doneAfter is a time before the end of the last done step.
	doneAfter time.Time
	// retryAt is the retryAfter of the last revert. reverts counts the
	// reverts at the generation revertGen.
	retryAt time.Time
	// size is the limit before the current step, and the spec limit and
	// memoryMax at its start, while the size of the step waits for the
	// status.
	size      *pendingSize
	reverts   int
	revertGen int64
	// highMaxMemory are the pods with a reported maxmemory above the limit.
	highMaxMemory map[string]bool
	// badStatus is the last reported value of each status field that is not
	// valid. A field is reported again when its value changes.
	badStatus map[string]string
}

// NewJudge returns the judge of one instance.
func NewJudge() *Judge {
	return &Judge{pods: map[string]baseline{}, highMaxMemory: map[string]bool{}, badStatus: map[string]string{}, ended: true}
}

// Observe judges one round. It returns the step results and the violations.
// settling is true while a step is Applying and in the round of its end,
// because the operator sets maxmemory one reconcile after the last resize.
// After the step timeout, settling is false, so that a window that a
// mutation holds can time out.
func (j *Judge) Observe(r Round) (events []Event, settling bool) {
	prev := j.prev
	j.prev = &r
	step := r.Status.LastStep
	newStep := prev != nil && j.isNewStep(r)
	if prev == nil {
		events = append(events, j.adopt(r)...)
	}
	// A step that ended between two rounds still covers the interval.
	active := newStep || isActive(j.phase) || isActive(r.Status.Phase)
	settling = newStep || j.phase == PhaseApplying || r.Status.Phase == PhaseApplying
	events = append(events, j.checkStatus(r)...)
	switch {
	case step == "" && j.step != "":
		// Mode Off or a lost state ConfigMap starts a state without steps.
		j.reset()
	case newStep:
		events = append(events, j.start(*prev, r)...)
	}
	// A step of a lost state has no lastStep, so the phase alone tells its
	// end.
	if prev != nil {
		events = append(events, j.follow(*prev, r)...)
	}
	events = append(events, j.checkPendingSize(r)...)
	events = append(events, j.checkTimeout(r)...)
	events = append(events, j.checkPods(r, active && prev != nil)...)
	events = append(events, j.checkMaster(r, active && prev != nil)...)
	events = append(events, j.checkMaxMemory(r, prev)...)
	return events, settling && !j.timedOut
}

// isNewStep reports whether r shows a step after the current one. lastStep
// has a resolution of a minute. Applying after the end of a step is a further
// step, also in the same minute.
func (j *Judge) isNewStep(r Round) bool {
	step := r.Status.LastStep
	return step != "" && (step != j.step || r.Status.Phase == PhaseApplying && isEnd(j.phase))
}

func isEnd(phase string) bool {
	return phase == PhaseDone || phase == PhaseReverted || phase == PhaseHeld
}

func isActive(phase string) bool {
	return phase == PhaseApplying || phase == PhaseHeld
}

// adopt takes the step of the first round, which started before the tester.
// Only a step in Applying has its first result still to come. The step
// started at lastStep, so its end and its revert came later: lastStep is the
// earliest end for the cooldown, and lastStep plus one back-off the earliest
// retry.
func (j *Judge) adopt(r Round) []Event {
	j.step, j.phase = r.Status.LastStep, r.Status.Phase
	j.ended = j.phase != PhaseApplying
	at, err := parseTime(j.step)
	if err == nil {
		j.starts = []time.Time{at}
	}
	switch j.phase {
	case PhaseDone:
		if err == nil {
			j.doneAfter = at
		}
	case PhaseReverted:
		j.reverts, j.revertGen = 1, r.Generation
		retry, rerr := parseTime(r.Status.RetryAfter)
		if rerr == nil {
			j.retryAt = retry
			return nil
		}
		if err == nil {
			j.retryAt = at.Add(backOff - slack)
		}
		return []Event{{Judge: JudgeRate, Step: j.step,
			Message: fmt.Sprintf("retryAfter %q of the Reverted step is not a time", r.Status.RetryAfter)}}
	}
	return nil
}

// pendingSize is a step whose size the judge did not check yet.
type pendingSize struct {
	from, spec, max int64
}

func (j *Judge) reset() {
	j.size = nil
	j.step, j.phase, j.ended, j.timedOut = "", "", true, false
	j.starts, j.doneAfter, j.retryAt, j.reverts = nil, time.Time{}, time.Time{}, 0
}

// start judges a step that the previous round did not show.
func (j *Judge) start(prev, r Round) []Event {
	step := r.Status.LastStep
	from, fromOK := effective(prev)
	events := []Event{{Result: ResultStarted, Step: step, Message: "from " + format(from)}}
	if at, err := parseTime(step); err == nil {
		if last, err := parseTime(j.step); err == nil && at.Before(last) {
			events = append(events, Event{Judge: JudgeStatus, Step: step, Message: fmt.Sprintf("lastStep %s is before the last step %s", step, j.step)})
		}
	}
	// A user change of the bounds in the same interval changes the limit
	// too. checkStatus reports an effectiveMemory that is not valid, and a
	// spec without a limit has no autoscaling.
	j.size = nil
	if prev.Spec == r.Spec && prev.Max == r.Max && fromOK && from > 0 {
		j.size = &pendingSize{from: from, spec: r.Spec, max: r.Max}
	}
	if at, err := parseTime(step); err == nil {
		for _, msg := range j.checkRate(at, r.Generation) {
			events = append(events, Event{Judge: JudgeRate, Step: step, Message: msg})
		}
	}
	j.step, j.phase, j.ended, j.timedOut, j.applyingSince = step, "", false, false, time.Time{}
	j.doneAfter, j.retryAt = time.Time{}, time.Time{}
	return events
}

// checkPendingSize checks the size of the current step when the status shows
// its limit. The operator writes the step to the state first, and the
// status shows the limit of the pods, which follows one reconcile later.
// Thus the first round of a step can show the limit before the step. The
// size is known in Applying when the limit moved, and at Done and Held,
// which keep the limit of the step. A revert shows the limit before the step,
// and a user change of the bounds changes the limit, so these give no check.
func (j *Judge) checkPendingSize(r Round) []Event {
	p := j.size
	if p == nil {
		return nil
	}
	to, ok := effective(r)
	switch phase := r.Status.Phase; {
	case r.Spec != p.spec || r.Max != p.max || phase == PhaseReverted:
		j.size = nil
		return nil
	case !ok, phase == PhaseApplying && to == p.from:
		return nil
	case phase != PhaseApplying && phase != PhaseDone && phase != PhaseHeld:
		return nil
	}
	j.size = nil
	if msg := checkSize(p.from, to, p.max); msg != "" {
		return []Event{{Judge: JudgeSize, Step: j.step, Message: msg}}
	}
	return nil
}

// checkSize returns why a step from the limit from to the limit to breaks the
// step rule, or "".
// The rule gives one target: 25 percent more, rounded up to 64Mi, and
// memoryMax at most. Thus a step can be more than 25 percent of a small
// limit. A target at or below from gives no step.
func checkSize(from, to, maxLimit int64) string {
	want := roundUp(from+from*stepPercent/100, stepAlign)
	if maxLimit > 0 {
		want = min(want, maxLimit)
	}
	switch {
	case want <= from:
		return fmt.Sprintf("a step from %s to %s, but the limit is at memoryMax", format(from), format(to))
	case to != want:
		return fmt.Sprintf("the step from %s to %s, want %s: 25 percent rounded up to 64Mi, at most memoryMax", format(from), format(to), format(want))
	}
	return ""
}

// checkRate returns why a step that started in the minute at breaks the
// cooldown, the budget or the back-off. The start is at most a minute after
// at, so each check uses the latest possible start.
func (j *Judge) checkRate(at time.Time, generation int64) []string {
	var msgs []string
	if !j.doneAfter.IsZero() && at.Add(minute+slack).Before(j.doneAfter.Add(cooldown)) {
		msgs = append(msgs, fmt.Sprintf("the step started in the minute %s, but the last step ended after %s: the cooldown is %s",
			at.Format(time.RFC3339), j.doneAfter.UTC().Format(time.RFC3339), cooldown))
	}
	recent := []time.Time{at}
	for _, s := range j.starts {
		if at.Sub(s) < budgetPeriod {
			recent = append(recent, s)
		}
	}
	j.starts = recent
	n := 0
	for _, s := range recent {
		if at.Sub(s) <= budgetPeriod-minute {
			n++
		}
	}
	if n > budget {
		msgs = append(msgs, fmt.Sprintf("%d steps started in 24 hours, the budget is %d", n, budget))
	}
	// A change of the generation is a user change, and it ends the back-off.
	if !j.retryAt.IsZero() && generation == j.revertGen && !at.Add(minute).After(j.retryAt) {
		msgs = append(msgs, fmt.Sprintf("the step started in the minute %s, before retryAfter %s", at.Format(time.RFC3339), j.retryAt.Format(time.RFC3339)))
	}
	return msgs
}

// follow judges the phase of the current step. A change of the phase
// happened after the previous round.
func (j *Judge) follow(prev, r Round) []Event {
	var events []Event
	phase := r.Status.Phase
	if phase == j.phase {
		return events
	}
	if result := resultOf(phase); result != "" && !j.ended {
		j.ended = true
		events = append(events, Event{Result: result, Step: j.step})
	}
	switch phase {
	case PhaseDone:
		j.doneAfter = prev.Now
	case PhaseReverted:
		events = append(events, j.revert(prev, r)...)
	}
	j.phase = phase
	return events
}

// checkTimeout judges the time of the phase Applying. It counts from
// lastStep, and at most from the first round in Applying, so that each step
// window has a bound.
func (j *Judge) checkTimeout(r Round) []Event {
	if r.Status.Phase != PhaseApplying {
		j.applyingSince = time.Time{}
		return nil
	}
	if j.applyingSince.IsZero() {
		j.applyingSince = r.Now
		if r.Status.LastStep == "" {
			// No lastStep tells a step from the last one.
			j.timedOut = false
		}
	}
	// A lastStep in the future does not move the bound of the window.
	deadline := j.applyingSince.Add(applyTimeout + timeoutMargin)
	if at, err := parseTime(r.Status.LastStep); err == nil {
		if d := at.Add(applyTimeout + minute + timeoutMargin); d.Before(deadline.Add(minute)) {
			deadline = d
		} else {
			deadline = deadline.Add(minute)
		}
	}
	if j.timedOut || !r.Now.After(deadline) {
		return nil
	}
	j.timedOut = true
	return []Event{{Judge: JudgeTimeout, Step: r.Status.LastStep,
		Message: fmt.Sprintf("the step is Applying for more than %s", applyTimeout+timeoutMargin)}}
}

func resultOf(phase string) string {
	switch phase {
	case PhaseDone:
		return ResultDone
	case PhaseReverted:
		return ResultReverted
	case PhaseHeld:
		return ResultHeld
	}
	return ""
}

// revert judges the back-off of a revert. RetryAt is the time of the revert
// plus the back-off, and retryAfter shows it in full minutes.
func (j *Judge) revert(prev, r Round) []Event {
	if r.Generation != j.revertGen {
		j.reverts = 0
	}
	j.reverts++
	j.revertGen = r.Generation
	want := backOffFor(j.reverts)
	retry, err := parseTime(r.Status.RetryAfter)
	if err != nil {
		// The revert is after the previous round, so the back-off ends after
		// this time.
		j.retryAt = prev.Now.Add(want - slack)
		return []Event{{Judge: JudgeRate, Step: j.step,
			Message: fmt.Sprintf("revert %d: retryAfter %q is not a time", j.reverts, r.Status.RetryAfter)}}
	}
	j.retryAt = retry
	if retry.Add(minute + slack).Before(prev.Now.Add(want)) {
		return []Event{{Judge: JudgeRate, Step: j.step,
			Message: fmt.Sprintf("revert %d: retryAfter %s is less than %s after the revert", j.reverts, r.Status.RetryAfter, want)}}
	}
	return nil
}

func backOffFor(reverts int) time.Duration {
	d := backOff
	for i := 1; i < reverts && d < maxBackOff; i++ {
		d *= 2
	}
	return min(d, maxBackOff)
}

// checkPods judges the redis pods against their baselines. A step must not
// restart or recreate a pod, and only a user path may lower a limit.
func (j *Judge) checkPods(r Round, active bool) []Event {
	var events []Event
	seen := map[string]bool{}
	for _, p := range r.Pods {
		seen[p.Name] = true
		b, ok := j.pods[p.Name]
		mine := r.Allow.forPod(p.Name)
		if ok {
			events = append(events, j.checkPod(p, b, b.allow.or(mine), active || b.active)...)
		}
		limit := p.Limit
		if limit == 0 {
			limit = b.limit
		}
		j.pods[p.Name] = baseline{uid: p.UID, restarts: p.Restarts, limit: limit, allow: mine, active: active}
	}
	for name, b := range j.pods {
		if !seen[name] {
			b.allow, b.active = b.allow.or(r.Allow.forPod(name)), b.active || active
			j.pods[name] = b
		}
	}
	return events
}

// checkPod judges a pod against its baseline b, with what allow permits.
// active is true when a step covers the round or the baseline.
func (j *Judge) checkPod(p Pod, b baseline, allow Allow, active bool) []Event {
	var events []Event
	if active && !allow.Recreate {
		switch {
		case p.UID != b.uid:
			events = append(events, Event{Judge: JudgeRestart, Step: j.step, Message: p.Name + " was recreated during a step"})
		case p.Restarts > b.restarts:
			events = append(events, Event{Judge: JudgeRestart, Step: j.step, Message: fmt.Sprintf("the redis container of %s restarted during a step", p.Name)})
		}
	}
	if p.Limit != 0 && p.Limit < b.limit && !allow.Shrink {
		events = append(events, Event{Judge: JudgeShrink, Step: j.step,
			Message: fmt.Sprintf("the limit of %s fell from %s to %s without a user path", p.Name, format(b.limit), format(p.Limit))})
	}
	return events
}

// checkMaster judges a change of the master. The master does not fail over
// in a step.
func (j *Judge) checkMaster(r Round, active bool) []Event {
	b := j.masterSince
	if r.Master == "" {
		j.masterSince = baseline{allow: b.allow.or(r.Allow), active: b.active || active}
		return nil
	}
	var events []Event
	if j.master != "" && r.Master != j.master && (active || b.active) && !b.allow.or(r.Allow).Handover {
		events = append(events, Event{Judge: JudgeHandover, Step: j.step, Message: fmt.Sprintf("the master moved from %s to %s during a step", j.master, r.Master)})
	}
	j.master, j.masterSince = r.Master, baseline{allow: r.Allow, active: active}
	return events
}

// checkMaxMemory judges each pod once until its maxmemory is again at most
// its limit. A user path that may shrink a pod excuses its round and the next
// round, because the operator lowers maxmemory one reconcile later.
func (j *Judge) checkMaxMemory(r Round, prev *Round) []Event {
	var events []Event
	for _, p := range r.Pods {
		excused := r.Allow.forPod(p.Name).Shrink || prev != nil && prev.Allow.forPod(p.Name).Shrink
		high := p.MaxMemory > 0 && p.Limit > 0 && p.MaxMemory > p.Limit
		switch {
		case !high:
			delete(j.highMaxMemory, p.Name)
		case !j.highMaxMemory[p.Name] && !excused:
			j.highMaxMemory[p.Name] = true
			events = append(events, Event{Judge: JudgeMaxMemory, Step: j.step,
				Message: fmt.Sprintf("%s has maxmemory %s above its limit %s", p.Name, format(p.MaxMemory), format(p.Limit))})
		}
	}
	return events
}

// effective returns the memory limit of the pods that the status gives. ok
// is false for an effectiveMemory that is not a positive quantity, and the
// limit is then the spec limit.
func effective(r Round) (limit int64, ok bool) {
	if r.Status.EffectiveMemory == "" {
		return r.Spec, true
	}
	if q, err := resource.ParseQuantity(r.Status.EffectiveMemory); err == nil && q.Value() > 0 {
		return q.Value(), true
	}
	return r.Spec, false
}

// checkStatus reports each status field that the operator never writes. The
// other judges then use a safe default: an unknown phase is no step in
// progress, a time that does not parse gives no rate check, and the timeout
// counts from the first round in Applying.
func (j *Judge) checkStatus(r Round) []Event {
	st := r.Status
	bad := map[string]string{}
	switch st.Phase {
	case "", "Idle", PhaseApplying, PhaseDone, PhaseReverted, PhaseHeld:
	default:
		bad["phase"] = fmt.Sprintf("phase %q is unknown", st.Phase)
	}
	if at, err := parseTime(st.LastStep); st.LastStep != "" && err != nil {
		bad["lastStep"] = fmt.Sprintf("lastStep %q is not a time", st.LastStep)
	} else if err == nil && at.After(r.Now.Add(minute+slack)) {
		bad["lastStep"] = fmt.Sprintf("lastStep %s is after the round at %s", st.LastStep, r.Now.UTC().Format(time.RFC3339))
	} else if st.LastStep == "" && (st.Phase == PhaseApplying || st.Phase == PhaseReverted) {
		// Only the start of a step gives these phases, and it sets lastStep.
		// A held step of a lost state has no lastStep.
		bad["lastStep"] = "no lastStep in phase " + st.Phase
	}
	if _, ok := effective(r); !ok {
		bad["effectiveMemory"] = fmt.Sprintf("effectiveMemory %q is not a positive quantity", st.EffectiveMemory)
	}
	var events []Event
	for _, field := range []string{"phase", "lastStep", "effectiveMemory"} {
		msg, isBad := bad[field]
		switch {
		case !isBad:
			delete(j.badStatus, field)
		case j.badStatus[field] != msg:
			j.badStatus[field] = msg
			events = append(events, Event{Judge: JudgeStatus, Step: st.LastStep, Message: msg})
		}
	}
	return events
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

func roundUp(n, multiple int64) int64 {
	return (n + multiple - 1) / multiple * multiple
}

func format(b int64) string {
	return resource.NewQuantity(b, resource.BinarySI).String()
}
