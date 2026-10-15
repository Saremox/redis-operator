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
)

// Judges lists each judge.
var Judges = []string{JudgeSize, JudgeTimeout, JudgeRate, JudgeRestart, JudgeHandover, JudgeShrink, JudgeMaxMemory}

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
	// starts are the start times of the steps for the budget.
	starts []time.Time
	// doneAfter is a time before the end of the last done step.
	doneAfter time.Time
	// retryAt is the retryAfter of the last revert. reverts counts the
	// reverts at the generation revertGen.
	retryAt   time.Time
	reverts   int
	revertGen int64
	// highMaxMemory are the pods with a reported maxmemory above the limit.
	highMaxMemory map[string]bool
}

// NewJudge returns the judge of one instance.
func NewJudge() *Judge {
	return &Judge{pods: map[string]baseline{}, highMaxMemory: map[string]bool{}, ended: true}
}

// Observe judges one round. It returns the step results and the violations.
// settling is true while a step is Applying and in the round of its end,
// because the operator sets maxmemory one reconcile after the last resize.
func (j *Judge) Observe(r Round) (events []Event, settling bool) {
	prev := j.prev
	j.prev = &r
	step := r.Status.LastStep
	newStep := prev != nil && step != "" && step != j.step
	if prev == nil {
		j.adopt(r)
	}
	// A step that ended between two rounds still covers the interval.
	active := newStep || isActive(j.phase) || isActive(r.Status.Phase)
	settling = newStep || j.phase == PhaseApplying || r.Status.Phase == PhaseApplying
	switch {
	case step == "" && j.step != "":
		// Mode Off or a lost state ConfigMap starts a state without steps.
		j.reset()
	case newStep:
		events = append(events, j.start(*prev, r)...)
	}
	if prev != nil && step != "" {
		events = append(events, j.follow(*prev, r)...)
	}
	events = append(events, j.checkPods(r, active && prev != nil)...)
	events = append(events, j.checkMaster(r, active && prev != nil)...)
	events = append(events, j.checkMaxMemory(r)...)
	return events, settling
}

func isActive(phase string) bool {
	return phase == PhaseApplying || phase == PhaseHeld
}

// adopt takes the step of the first round, which started before the tester.
func (j *Judge) adopt(r Round) {
	j.step, j.phase = r.Status.LastStep, r.Status.Phase
	j.ended = !isActive(j.phase)
	if at, err := parseTime(j.step); err == nil {
		j.starts = []time.Time{at}
	}
}

func (j *Judge) reset() {
	j.step, j.phase, j.ended, j.timedOut = "", "", true, false
	j.starts, j.doneAfter, j.retryAt, j.reverts = nil, time.Time{}, time.Time{}, 0
}

// start judges the start of a step that is new in r.
func (j *Judge) start(prev, r Round) []Event {
	step := r.Status.LastStep
	from, to := effective(prev), effective(r)
	events := []Event{{Result: ResultStarted, Step: step, Message: fmt.Sprintf("from %s to %s", format(from), format(to))}}
	// A revert shows the limit before the step, and a user change of the
	// bounds in the same interval changes the limit too.
	if prev.Spec == r.Spec && prev.Max == r.Max && r.Status.Phase != PhaseReverted {
		if msg := checkSize(from, to, r.Max); msg != "" {
			events = append(events, Event{Judge: JudgeSize, Step: step, Message: msg})
		}
	}
	if at, err := parseTime(step); err == nil {
		for _, msg := range j.checkRate(at, r.Generation) {
			events = append(events, Event{Judge: JudgeRate, Step: step, Message: msg})
		}
	}
	j.step, j.phase, j.ended, j.timedOut = step, "", false, false
	j.doneAfter, j.retryAt = time.Time{}, time.Time{}
	return events
}

// checkSize returns why a step from from to to breaks the step rule, or "".
// The rule rounds up, so a step can be more than 25 percent of a small limit.
func checkSize(from, to, maxLimit int64) string {
	most := roundUp(from+from*stepPercent/100, stepAlign)
	if maxLimit > 0 {
		most = min(most, maxLimit)
	}
	switch {
	case to <= from:
		return fmt.Sprintf("the step from %s to %s does not raise the limit", format(from), format(to))
	case to > most:
		return fmt.Sprintf("the step from %s to %s is above %s: 25 percent rounded up to 64Mi, at most memoryMax", format(from), format(to), format(most))
	case to%stepAlign != 0 && to != maxLimit:
		return fmt.Sprintf("the step to %s is not a multiple of 64Mi and not memoryMax", format(to))
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
	if phase == PhaseApplying && !j.timedOut {
		if at, err := parseTime(j.step); err == nil && r.Now.Sub(at) > applyTimeout+minute+timeoutMargin {
			j.timedOut = true
			events = append(events, Event{Judge: JudgeTimeout, Step: j.step,
				Message: fmt.Sprintf("the step is Applying for more than %s", applyTimeout+timeoutMargin)})
		}
	}
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
	retry, err := parseTime(r.Status.RetryAfter)
	if err != nil {
		j.retryAt = time.Time{}
		return nil
	}
	j.retryAt = retry
	if want := backOffFor(j.reverts); retry.Add(minute + slack).Before(prev.Now.Add(want)) {
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
		if ok {
			allow := b.allow.or(r.Allow)
			if (active || b.active) && !allow.Recreate {
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
		}
		limit := p.Limit
		if limit == 0 {
			limit = b.limit
		}
		j.pods[p.Name] = baseline{uid: p.UID, restarts: p.Restarts, limit: limit, allow: r.Allow, active: active}
	}
	for name, b := range j.pods {
		if !seen[name] {
			b.allow, b.active = b.allow.or(r.Allow), b.active || active
			j.pods[name] = b
		}
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
// its limit.
func (j *Judge) checkMaxMemory(r Round) []Event {
	var events []Event
	for _, p := range r.Pods {
		high := p.MaxMemory > 0 && p.Limit > 0 && p.MaxMemory > p.Limit
		switch {
		case !high:
			delete(j.highMaxMemory, p.Name)
		case !j.highMaxMemory[p.Name] && !r.Allow.Shrink:
			j.highMaxMemory[p.Name] = true
			events = append(events, Event{Judge: JudgeMaxMemory, Step: j.step,
				Message: fmt.Sprintf("%s has maxmemory %s above its limit %s", p.Name, format(p.MaxMemory), format(p.Limit))})
		}
	}
	return events
}

// effective returns the memory limit of the pods that the status gives.
func effective(r Round) int64 {
	if q, err := resource.ParseQuantity(r.Status.EffectiveMemory); err == nil {
		return q.Value()
	}
	return r.Spec
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
