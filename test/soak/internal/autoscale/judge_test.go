package autoscale

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

const mi = 1 << 20

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// at returns a round at minute m of the test, with three pods of the limit
// 128Mi and rfr-x-0 as the master.
func at(m float64) Round {
	r := Round{Now: t0.Add(time.Duration(m * float64(time.Minute))), Generation: 1, Spec: 128 * mi, Max: 1024 * mi, Master: "rfr-x-0"}
	for _, name := range []string{"rfr-x-0", "rfr-x-1", "rfr-x-2"} {
		r.Pods = append(r.Pods, Pod{Name: name, UID: name + "-uid", Limit: 128 * mi, MaxMemory: 45 * mi})
	}
	return r
}

// minuteOf returns the lastStep of a step that started in minute m.
func minuteOf(m int) string {
	return t0.Add(time.Duration(m) * time.Minute).Format(time.RFC3339)
}

// stepped sets the status of a step that started in minute start.
func stepped(r Round, start int, phase, effective string) Round {
	r.Status = redisfailoverv1.AutoscalingStatus{LastStep: minuteOf(start), Phase: phase, EffectiveMemory: effective}
	return r
}

func withLimit(r Round, limit int64) Round {
	for i := range r.Pods {
		r.Pods[i].Limit = limit
	}
	return r
}

func observe(j *Judge, rounds ...Round) []Event {
	var events []Event
	for _, r := range rounds {
		e, _ := j.Observe(r)
		events = append(events, e...)
	}
	return events
}

// summary lists the results and the judges of the events.
func summary(events []Event) []string {
	var out []string
	for _, e := range events {
		if e.Judge != "" {
			out = append(out, e.Judge)
		} else {
			out = append(out, e.Result)
		}
	}
	return out
}

func TestStep(t *testing.T) {
	j := NewJudge()
	idle := at(0)
	if e, settling := j.Observe(idle); len(e) != 0 || settling {
		t.Fatalf("first round: %v, settling %t", summary(e), settling)
	}
	applying := stepped(at(1), 1, PhaseApplying, "192Mi")
	e, settling := j.Observe(applying)
	if got := summary(e); !slices.Equal(got, []string{ResultStarted}) || !settling {
		t.Fatalf("start: %v, settling %t", got, settling)
	}
	if e[0].Message != "from 128Mi" {
		t.Errorf("message %q", e[0].Message)
	}
	done := withLimit(stepped(at(2), 1, PhaseDone, "192Mi"), 192*mi)
	// The round of the end still settles: maxmemory follows one reconcile
	// later.
	e, settling = j.Observe(done)
	if got := summary(e); !slices.Equal(got, []string{ResultDone}) || !settling {
		t.Fatalf("done: %v, settling %t", got, settling)
	}
	// The step ends one time.
	e, settling = j.Observe(withLimit(stepped(at(3), 1, PhaseDone, "192Mi"), 192*mi))
	if len(e) != 0 || settling {
		t.Errorf("after the end: %v, settling %t", summary(e), settling)
	}
}

// A step that started and ended between two rounds still opens a window.
func TestStepBetweenRounds(t *testing.T) {
	j := NewJudge()
	observe(j, at(0))
	e, settling := j.Observe(withLimit(stepped(at(1), 1, PhaseDone, "192Mi"), 192*mi))
	if got := summary(e); !slices.Equal(got, []string{ResultStarted, ResultDone}) || !settling {
		t.Errorf("%v, settling %t", got, settling)
	}
}

// The tester adopts the step of its first round, and counts no result.
func TestAdopt(t *testing.T) {
	j := NewJudge()
	e := observe(j, stepped(at(0), 0, PhaseApplying, "192Mi"), withLimit(stepped(at(1), 0, PhaseDone, "192Mi"), 192*mi))
	if got := summary(e); !slices.Equal(got, []string{ResultDone}) {
		t.Errorf("%v", got)
	}
}

func TestCheckSize(t *testing.T) {
	for _, c := range []struct {
		name          string
		from, to, max int64
		bad           bool
	}{
		{"25 percent rounded up", 128 * mi, 192 * mi, 512 * mi, false},
		{"a small limit gets a step of 64Mi", 64 * mi, 128 * mi, 512 * mi, false},
		{"memoryMax", 128 * mi, 150 * mi, 150 * mi, false},
		{"more than one step", 128 * mi, 256 * mi, 512 * mi, true},
		{"above memoryMax", 128 * mi, 192 * mi, 150 * mi, true},
		{"not aligned", 128 * mi, 130 * mi, 512 * mi, true},
		{"no raise", 192 * mi, 192 * mi, 512 * mi, true},
		{"a lower limit", 192 * mi, 128 * mi, 512 * mi, true},
		// The rule gives one target.
		{"512Mi", 512 * mi, 640 * mi, 1024 * mi, false},
		{"less than the target", 512 * mi, 576 * mi, 1024 * mi, true},
		{"aligned, but more than the target", 512 * mi, 704 * mi, 1024 * mi, true},
		{"below memoryMax", 128 * mi, 150 * mi, 256 * mi, true},
		{"a step at memoryMax", 150 * mi, 192 * mi, 150 * mi, true},
	} {
		if got := checkSize(c.from, c.to, c.max); (got != "") != c.bad {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

func TestStepSizeJudge(t *testing.T) {
	j := NewJudge()
	e := observe(j, at(0), stepped(at(1), 1, PhaseApplying, "256Mi"))
	if got := summary(e); !slices.Equal(got, []string{ResultStarted, JudgeSize}) {
		t.Errorf("%v", got)
	}
	// A user change of the spec in the same interval also changes the limit.
	j = NewJudge()
	r := stepped(at(1), 1, PhaseApplying, "256Mi")
	r.Spec = 160 * mi
	if got := summary(observe(j, at(0), r)); !slices.Equal(got, []string{ResultStarted}) {
		t.Errorf("after a spec change: %v", got)
	}
}

// target is the step of the operator from limit, below memoryMax.
func target(limit int64) int64 {
	return roundUp(limit+limit/4, 64*mi)
}

// A step that stays Applying keeps no window open after its timeout, so
// that a mutation that holds the window ends.
// A step with a lastStep that does not parse, or without lastStep, times out
// 17 minutes after the first round that saw it Applying.
func TestTimeoutWithoutLastStep(t *testing.T) {
	for _, lastStep := range []string{"not a time", ""} {
		j := NewJudge()
		observe(j, stepped(at(0), 0, PhaseDone, "192Mi"))
		applying := func(m float64) Round {
			r := stepped(at(m), 0, PhaseApplying, "256Mi")
			r.Status.LastStep = lastStep
			return r
		}
		var events []Event
		for _, m := range []float64{1, 10, 17.5} {
			e, settling := j.Observe(applying(m))
			events = append(events, e...)
			if !settling {
				t.Errorf("%q, minute %v: no window", lastStep, m)
			}
		}
		for _, m := range []float64{18.5, 40} {
			e, settling := j.Observe(applying(m))
			events = append(events, e...)
			if settling {
				t.Errorf("%q, minute %v: a window after the timeout", lastStep, m)
			}
		}
		timeouts := 0
		for _, e := range events {
			if e.Judge == JudgeTimeout {
				timeouts++
			}
		}
		if timeouts != 1 {
			t.Errorf("%q: %v, want one timeout", lastStep, summary(events))
		}
		// The phase Applying starts again after another phase.
		observe(j, stepped(at(41), 0, PhaseDone, "256Mi"))
		if _, settling := j.Observe(applying(42)); !settling {
			t.Errorf("%q: no window for the next Applying", lastStep)
		}
	}
}

func TestTimeoutEndsSettling(t *testing.T) {
	j := NewJudge()
	observe(j, at(0))
	for _, m := range []float64{1, 10, 18} {
		if _, settling := j.Observe(stepped(at(m), 1, PhaseApplying, "192Mi")); !settling {
			t.Errorf("minute %v: no window", m)
		}
	}
	for _, m := range []float64{20, 40} {
		if _, settling := j.Observe(stepped(at(m), 1, PhaseApplying, "192Mi")); settling {
			t.Errorf("minute %v: a window after the timeout", m)
		}
	}
	// The next step settles again.
	if _, settling := j.Observe(stepped(at(41), 41, PhaseApplying, "256Mi")); !settling {
		t.Error("the next step has no window")
	}
}

// The operator writes the step to the state first, and the status shows the
// limit of the step one reconcile later. Thus the first round of a step can
// show the earlier effectiveMemory, and the size check waits until the
// status shows the step limit, or the step ends.
func TestStepSizeAfterTheFirstRound(t *testing.T) {
	applying := func(m float64, effective string) Round {
		return stepped(at(m), 1, PhaseApplying, effective)
	}
	ended := func(m float64, phase, effective string, limit int64) Round {
		return withLimit(stepped(at(m), 1, phase, effective), limit)
	}
	for _, c := range []struct {
		name   string
		rounds []Round
		want   []string
	}{
		{"the limit shows in the second round", []Round{at(0), applying(1, ""), applying(1.1, "192Mi"), ended(2, PhaseDone, "192Mi", 192*mi)},
			[]string{ResultStarted, ResultDone}},
		{"the earlier limit in the first round", []Round{withLimit(stepped(at(0), 0, PhaseDone, "192Mi"), 192*mi),
			withLimit(stepped(at(30), 30, PhaseApplying, "192Mi"), 192*mi), withLimit(stepped(at(30.1), 30, PhaseApplying, "256Mi"), 192*mi),
			withLimit(stepped(at(31), 30, PhaseDone, "256Mi"), 256*mi)},
			[]string{ResultStarted, ResultDone}},
		{"a wrong limit in the second round", []Round{at(0), applying(1, ""), applying(1.1, "160Mi"), ended(2, PhaseDone, "160Mi", 160*mi)},
			[]string{ResultStarted, JudgeSize, ResultDone}},
		{"a step to 576Mi from 512Mi", []Round{withLimit(spec(at(0), 512*mi), 512*mi), withLimit(spec(applying(1, ""), 512*mi), 512*mi),
			withLimit(spec(applying(1.1, "576Mi"), 512*mi), 512*mi)},
			[]string{ResultStarted, JudgeSize}},
		// A fast step ends before the status shows Applying with its limit.
		{"Done in the first round", []Round{at(0), ended(1, PhaseDone, "192Mi", 192*mi)}, []string{ResultStarted, ResultDone}},
		{"Done with a wrong limit", []Round{at(0), applying(1, ""), ended(2, PhaseDone, "160Mi", 160*mi)},
			[]string{ResultStarted, ResultDone, JudgeSize}},
		{"Done without a raise", []Round{at(0), applying(1, ""), ended(2, PhaseDone, "", 128*mi)},
			[]string{ResultStarted, ResultDone, JudgeSize}},
		// A held step keeps the limit of the step.
		{"Held", []Round{at(0), applying(1, ""), ended(2, PhaseHeld, "192Mi", 192*mi)}, []string{ResultStarted, ResultHeld}},
		// A revert shows the limit before the step: no size to check.
		{"Reverted", []Round{at(0), applying(1, ""), stepped(at(2), 1, PhaseReverted, "")}, []string{ResultStarted, ResultReverted}},
		// A user change of the bounds changes the limit, so the size of the
		// step is not known.
		{"a spec change in the step", []Round{at(0), applying(1, ""), spec(applying(1.1, "256Mi"), 160*mi)}, []string{ResultStarted}},
		{"a memoryMax change in the step", []Round{at(0), applying(1, ""), noMax(applying(1.1, "160Mi"))}, []string{ResultStarted}},
	} {
		j := NewJudge()
		var got []string
		for _, r := range c.rounds {
			e, _ := j.Observe(r)
			for _, x := range summary(e) {
				if x != JudgeRate {
					got = append(got, x)
				}
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTimeout(t *testing.T) {
	j := NewJudge()
	e := observe(j, at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), stepped(at(15), 1, PhaseApplying, "192Mi"))
	if got := summary(e); !slices.Equal(got, []string{ResultStarted}) {
		t.Fatalf("in time: %v", got)
	}
	e = observe(j, stepped(at(19), 1, PhaseApplying, "192Mi"), stepped(at(20), 1, PhaseApplying, "192Mi"), stepped(at(21), 1, PhaseHeld, "192Mi"))
	if got := summary(e); !slices.Equal(got, []string{JudgeTimeout, ResultHeld}) {
		t.Errorf("late: %v", got)
	}
}

// done runs a step that starts in minute start and is done after minute
// end.
func done(j *Judge, start, end int, from, to int64) []Event {
	r := withLimit(at(float64(start)-0.5), from)
	r.Status = j.prev.Status
	applying := withLimit(stepped(at(float64(start)+0.5), start, PhaseApplying, format(to)), from)
	finished := withLimit(stepped(at(float64(end)+0.5), start, PhaseDone, format(to)), to)
	return observe(j, r, applying, finished)
}

func TestCooldown(t *testing.T) {
	for _, c := range []struct {
		next int
		bad  bool
	}{{next: 2, bad: true}, {next: 3, bad: false}, {next: 6, bad: false}} {
		j := NewJudge()
		observe(j, at(0))
		done(j, 1, 2, 128*mi, 192*mi)
		e := done(j, 2+c.next, 3+c.next, 192*mi, 256*mi)
		// The slack of 30 seconds accepts 3 minutes.
		if got := slices.Contains(summary(e), JudgeRate); got != c.bad {
			t.Errorf("next step %d minutes after the end: %v", c.next, summary(e))
		}
	}
}

func TestBudget(t *testing.T) {
	j := NewJudge()
	observe(j, at(0))
	limit := int64(128 * mi)
	var e []Event
	for i := range 7 {
		e = done(j, 10+20*i, 11+20*i, limit, target(limit))
		limit = target(limit)
	}
	if got := summary(e); !slices.Contains(got, JudgeRate) || !strings.Contains(e[1].Message, "7 steps") {
		t.Errorf("seventh step: %v %v", got, e)
	}

	// Mode Off or a lost state starts the budget again.
	j = NewJudge()
	observe(j, at(0))
	limit = 128 * mi
	for i := range 7 {
		if i == 3 {
			observe(j, withLimit(at(float64(5+20*i)), limit))
		}
		e = append(e[:0], done(j, 10+20*i, 11+20*i, limit, target(limit))...)
		if slices.Contains(summary(e), JudgeRate) {
			t.Errorf("step %d after a reset: %v", i, e)
		}
		limit = target(limit)
	}
}

// revert runs a step that starts in minute start and is reverted after minute
// end with retryAfter.
func revert(j *Judge, start, end int, retry time.Duration, generation int64) []Event {
	before := at(float64(start) - 0.5)
	before.Status, before.Generation = j.prev.Status, generation
	applying := stepped(at(float64(start)+0.5), start, PhaseApplying, "192Mi")
	reverted := stepped(at(float64(end)+0.5), start, PhaseReverted, "")
	applying.Generation, reverted.Generation = generation, generation
	reverted.Status.RetryAfter = t0.Add(time.Duration(end)*time.Minute + retry).Format(time.RFC3339)
	return observe(j, before, applying, reverted)
}

func TestBackOff(t *testing.T) {
	j := NewJudge()
	observe(j, at(0))
	if got := summary(revert(j, 1, 2, time.Hour, 1)); !slices.Equal(got, []string{ResultStarted, ResultReverted}) {
		t.Fatalf("first revert: %v", got)
	}
	// The second revert at the same generation doubles the back-off.
	if got := summary(revert(j, 63, 64, time.Hour, 1)); !slices.Equal(got, []string{ResultStarted, ResultReverted, JudgeRate}) {
		t.Errorf("second revert with 1h: %v", got)
	}
	// A start before retryAfter.
	if got := summary(revert(j, 70, 71, 4*time.Hour, 1)); !slices.Contains(got, JudgeRate) {
		t.Errorf("start before retryAfter: %v", got)
	}

	// A change of the generation ends the wait and the count of reverts.
	j = NewJudge()
	observe(j, at(0))
	revert(j, 1, 2, time.Hour, 1)
	if got := summary(revert(j, 10, 11, time.Hour, 2)); !slices.Equal(got, []string{ResultStarted, ResultReverted}) {
		t.Errorf("after a user change: %v", got)
	}
}

// Each status field that the operator never writes is a finding of
// autoscale_status, or of the judge that needs it. The other judges then use
// a safe default, and no window stays open without a bound.
func TestBadStatus(t *testing.T) {
	withStatus := func(r Round, phase, lastStep, effective string) Round {
		r.Status = redisfailoverv1.AutoscalingStatus{Phase: phase, LastStep: lastStep, EffectiveMemory: effective}
		return r
	}
	reverted := func(retryAfter string) Round {
		r := stepped(at(2), 1, PhaseReverted, "")
		r.Status.RetryAfter = retryAfter
		return r
	}
	for _, c := range []struct {
		name   string
		rounds []Round
		want   []string
	}{
		{"unknown phase", []Round{at(0), withStatus(at(1), "Resizing", minuteOf(1), "192Mi")},
			[]string{JudgeStatus, ResultStarted}},
		{"lastStep is no time", []Round{at(0), withStatus(at(1), PhaseApplying, "soon", "192Mi")},
			[]string{JudgeStatus, ResultStarted}},
		{"lastStep in the future", []Round{at(0), withStatus(at(1), PhaseApplying, minuteOf(30), "192Mi")},
			[]string{JudgeStatus, ResultStarted}},
		{"no lastStep in Applying", []Round{at(0), withStatus(at(1), PhaseApplying, "", "192Mi")},
			[]string{JudgeStatus}},
		{"no lastStep in Reverted", []Round{at(0), withStatus(at(1), PhaseReverted, "", "")},
			[]string{JudgeStatus, JudgeRate}},
		// A lost state gives a held step without lastStep.
		{"no lastStep in Held", []Round{at(0), withStatus(at(1), PhaseHeld, "", "192Mi")}, nil},
		{"lastStep before the last step", []Round{at(0), withLimit(stepped(at(10), 9, PhaseDone, "192Mi"), 192*mi),
			withLimit(stepped(at(11), 5, PhaseDone, "256Mi"), 256*mi)},
			[]string{ResultStarted, ResultDone, ResultStarted, JudgeStatus, ResultDone}},
		// The size check needs both limits.
		{"effectiveMemory is no quantity", []Round{at(0), withStatus(at(1), PhaseApplying, minuteOf(1), "lots")},
			[]string{JudgeStatus, ResultStarted}},
		{"effectiveMemory is negative", []Round{at(0), withStatus(at(1), PhaseApplying, minuteOf(1), "-64Mi")},
			[]string{JudgeStatus, ResultStarted}},
		{"effectiveMemory is 0", []Round{at(0), withStatus(at(1), PhaseApplying, minuteOf(1), "0")},
			[]string{JudgeStatus, ResultStarted}},
		{"no retryAfter", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), reverted("")},
			[]string{ResultStarted, ResultReverted, JudgeRate}},
		{"retryAfter is no time", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), reverted("later")},
			[]string{ResultStarted, ResultReverted, JudgeRate}},
		// The back-off still counts from the revert that the tester saw.
		{"a step in the back-off without retryAfter", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), reverted(""),
			stepped(at(10), 10, PhaseApplying, "192Mi")},
			[]string{ResultStarted, ResultReverted, JudgeRate, ResultStarted, JudgeRate}},
		// Mode Off and a lost state remove the status.
		{"no status", []Round{stepped(at(0), 0, PhaseDone, "192Mi"), at(1)}, nil},
		// A spec without a limit has no autoscaling: no size check.
		{"no spec limit", []Round{spec(at(0), 0), spec(stepped(at(1), 1, PhaseApplying, "192Mi"), 0)},
			[]string{ResultStarted}},
		// Without memoryMax, the target has no cap.
		{"no memoryMax", []Round{noMax(at(0)), noMax(stepped(at(1), 1, PhaseApplying, "192Mi"))},
			[]string{ResultStarted}},
	} {
		j := NewJudge()
		events := observe(j, c.rounds...)
		if got := summary(events); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		// The same status 40 minutes later holds no window.
		last := c.rounds[len(c.rounds)-1]
		last.Now = last.Now.Add(40 * time.Minute)
		if _, settling := j.Observe(last); settling {
			t.Errorf("%s: a window after 40 minutes", c.name)
		}
	}
}

// A field that stays wrong counts one time, and again after it changes.
func TestBadStatusOnce(t *testing.T) {
	j := NewJudge()
	r := func(m float64, phase string) Round {
		r := at(m)
		r.Status.Phase = phase
		return r
	}
	got := summary(observe(j, r(0, "Foo"), r(1, "Foo"), r(2, "Bar"), r(3, ""), r(4, "Bar")))
	if !slices.Equal(got, []string{JudgeStatus, JudgeStatus, JudgeStatus}) {
		t.Errorf("%v", got)
	}
}

func spec(r Round, limit int64) Round {
	r.Spec = limit
	return r
}

func noMax(r Round) Round {
	r.Max = 0
	return r
}

// The state machine of the judge: the tester adopts each phase, with and
// without lastStep, and the next round shows each phase, with the same
// lastStep, a later one, or none.
func TestStateMachine(t *testing.T) {
	phases := []string{"", "Idle", PhaseApplying, PhaseDone, PhaseReverted, PhaseHeld, "Foo"}
	results := map[string]bool{ResultDone: true, ResultReverted: true, ResultHeld: true}
	for _, adoptPhase := range phases {
		for _, adoptStep := range []string{minuteOf(0), ""} {
			for _, next := range phases {
				for _, nextStep := range []string{"same", "later", "none"} {
					name := fmt.Sprintf("adopt %q %q, next %q %s", adoptPhase, adoptStep, next, nextStep)
					round := func(m float64, phase, lastStep string) Round {
						r := withLimit(at(m), 192*mi)
						r.Status = redisfailoverv1.AutoscalingStatus{Phase: phase, LastStep: lastStep, EffectiveMemory: "192Mi",
							RetryAfter: minuteOf(120)}
						return r
					}
					step := map[string]string{"same": adoptStep, "later": minuteOf(3), "none": ""}[nextStep]
					j := NewJudge()
					observe(j, round(0, adoptPhase, adoptStep))
					second := observe(j, round(4, next, step))
					// The step of the second round started there when its
					// lastStep changed. Else the adopted step continues, and
					// only an adopted Applying has its result still to come.
					// lastStep has a resolution of a minute, so Applying after
					// the end of a step is a further step in the same minute.
					started := step != "" && (step != adoptStep || next == PhaseApplying && isEnd(adoptPhase))
					wantResult := results[resultOf(next)] && (started || adoptPhase == PhaseApplying && step == adoptStep)
					if got := countResults(second, results); got != map[bool]int{true: 1, false: 0}[wantResult] {
						t.Errorf("%s: %d results in %v", name, got, summary(second))
					}
					if got := slices.Contains(summary(second), ResultStarted); got != started {
						t.Errorf("%s: started %t in %v", name, got, summary(second))
					}
					if j.phase != next {
						t.Errorf("%s: cached phase %q", name, j.phase)
					}
					e, settling := j.Observe(round(4.5, next, step))
					if countResults(e, results) != 0 || settling != (next == PhaseApplying) {
						t.Errorf("%s: %v in the third round, settling %t", name, summary(e), settling)
					}
					// A restart and a handover are violations only while a
					// step is in progress.
					r := round(5, next, step)
					r.Pods[1].UID, r.Master = "new", "rfr-x-1"
					active := isActive(next)
					e, _ = j.Observe(r)
					if got := slices.Contains(summary(e), JudgeRestart); got != active {
						t.Errorf("%s: restart finding %t", name, got)
					}
					if got := slices.Contains(summary(e), JudgeHandover); got != active {
						t.Errorf("%s: handover finding %t", name, got)
					}
				}
			}
		}
	}
}

// A further step in the same minute as the last one counts as a step, with
// its checks, and its end counts again.
func TestStepInTheSameMinute(t *testing.T) {
	for _, c := range []struct {
		end  string
		want []string
	}{
		{PhaseDone, []string{ResultStarted, JudgeRate, ResultDone}},
		{PhaseReverted, []string{ResultStarted, JudgeRate, ResultDone}},
		{PhaseHeld, []string{ResultStarted, ResultDone}},
	} {
		j := NewJudge()
		first := withLimit(stepped(at(0.2), 0, PhaseApplying, "192Mi"), 128*mi)
		ended := withLimit(stepped(at(0.4), 0, c.end, "192Mi"), 192*mi)
		if c.end == PhaseReverted {
			ended = withLimit(stepped(at(0.4), 0, c.end, ""), 128*mi)
			ended.Status.RetryAfter = minuteOf(60)
		}
		observe(j, at(0), first, ended)
		// The step from the limit of the end round.
		again := withLimit(stepped(at(0.6), 0, PhaseApplying, ""), effectiveOf(ended))
		again.Status.EffectiveMemory = format(target(effectiveOf(ended)))
		done := withLimit(stepped(at(0.8), 0, PhaseDone, again.Status.EffectiveMemory), target(effectiveOf(ended)))
		if got := summary(observe(j, again, done)); !slices.Equal(got, c.want) {
			t.Errorf("after %s: %v, want %v", c.end, got, c.want)
		}
	}
}

func effectiveOf(r Round) int64 {
	l, _ := effective(r)
	return l
}

func countResults(events []Event, results map[string]bool) int {
	n := 0
	for _, e := range events {
		if e.Judge == "" && results[e.Result] {
			n++
		}
	}
	return n
}

// The tester adopts the rate state that the status allows.
func TestAdoptRateState(t *testing.T) {
	adopted := func(phase, retryAfter string) Round {
		r := stepped(at(0), 0, phase, "192Mi")
		r.Status.RetryAfter = retryAfter
		return withLimit(r, 192*mi)
	}
	next := func(m float64, generation int64) Round {
		r := withLimit(stepped(at(m), int(m), PhaseApplying, "256Mi"), 192*mi)
		r.Generation = generation
		return r
	}
	for _, c := range []struct {
		name   string
		rounds []Round
		want   []string
	}{
		{"Done: the cooldown from lastStep", []Round{adopted(PhaseDone, ""), next(2, 1)}, []string{ResultStarted, JudgeRate}},
		{"Done: after the cooldown", []Round{adopted(PhaseDone, ""), next(10, 1)}, []string{ResultStarted}},
		{"Reverted: a step before retryAfter", []Round{adopted(PhaseReverted, minuteOf(60)), next(30, 1)}, []string{ResultStarted, JudgeRate}},
		{"Reverted: a step after retryAfter", []Round{adopted(PhaseReverted, minuteOf(60)), next(70, 1)}, []string{ResultStarted}},
		{"Reverted: a user change ends the back-off", []Round{adopted(PhaseReverted, minuteOf(60)), next(30, 2)}, []string{ResultStarted}},
		// The back-off is at least 1 hour after lastStep.
		{"Reverted: no retryAfter", []Round{adopted(PhaseReverted, ""), next(30, 1)}, []string{JudgeRate, ResultStarted, JudgeRate}},
		{"Reverted: retryAfter is no time", []Round{adopted(PhaseReverted, "soon"), next(70, 1)}, []string{JudgeRate, ResultStarted}},
	} {
		if got := summary(observe(NewJudge(), c.rounds...)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBackOffFor(t *testing.T) {
	for reverts, want := range map[int]time.Duration{1: time.Hour, 2: 2 * time.Hour, 5: 16 * time.Hour, 6: 24 * time.Hour, 9: 24 * time.Hour} {
		if got := backOffFor(reverts); got != want {
			t.Errorf("%d reverts: %s, want %s", reverts, got, want)
		}
	}
}

func TestRestart(t *testing.T) {
	recreated := func(r Round) Round {
		r.Pods[1].UID = "new"
		return r
	}
	restarted := func(r Round) Round {
		r.Pods[2].Restarts = 1
		return r
	}
	for _, c := range []struct {
		name   string
		rounds []Round
		want   []string
	}{
		{"recreate in a step", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), recreated(stepped(at(2), 1, PhaseApplying, "192Mi"))},
			[]string{ResultStarted, JudgeRestart}},
		{"restart in a step", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), restarted(stepped(at(2), 1, PhaseApplying, "192Mi"))},
			[]string{ResultStarted, JudgeRestart}},
		// The restart shows one round after the end of the step.
		{"restart after the end", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), stepped(at(2), 1, PhaseDone, "192Mi"),
			restarted(stepped(at(3), 1, PhaseDone, "192Mi"))}, []string{ResultStarted, ResultDone, JudgeRestart}},
		{"restart outside a step", []Round{at(0), at(1), restarted(at(2))}, nil},
		{"a user path in a step", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), recreated(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true}))},
			[]string{ResultStarted}},
		// The user path ran in the round before the change showed.
		{"a user path one round before", []Round{at(0), allow(stepped(at(1), 1, PhaseApplying, "192Mi"), Allow{Recreate: true}), recreated(stepped(at(2), 1, PhaseApplying, "192Mi"))},
			[]string{ResultStarted}},
		// A kill excuses only the killed pod. Its replacement has its name.
		{"the killed pod", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"),
			recreated(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Pod: "rfr-x-1"}))},
			[]string{ResultStarted}},
		{"another pod than the killed pod", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"),
			recreated(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Pod: "rfr-x-2"}))},
			[]string{ResultStarted, JudgeRestart}},
		{"another pod than the killed pod, absent for a while", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"),
			without(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Pod: "rfr-x-2"}), 1),
			recreated(allow(stepped(at(3), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Pod: "rfr-x-2"}))},
			[]string{ResultStarted, JudgeRestart}},
		{"the killed pod, absent for a while", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"),
			without(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Pod: "rfr-x-1"}), 1),
			recreated(stepped(at(3), 1, PhaseApplying, "192Mi"))},
			[]string{ResultStarted}},
		{"a pod that is absent for a while", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), without(stepped(at(2), 1, PhaseDone, "192Mi"), 1),
			recreated(stepped(at(3), 1, PhaseDone, "192Mi"))}, []string{ResultStarted, ResultDone, JudgeRestart}},
	} {
		if got := summary(observe(NewJudge(), c.rounds...)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func allow(r Round, a Allow) Round {
	r.Allow = a
	return r
}

func without(r Round, pod int) Round {
	r.Pods = slices.Delete(r.Pods, pod, pod+1)
	return r
}

func TestHandover(t *testing.T) {
	moved := func(r Round) Round {
		r.Master = "rfr-x-1"
		return r
	}
	for _, c := range []struct {
		name   string
		rounds []Round
		want   []string
	}{
		{"in a step", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), moved(stepped(at(2), 1, PhaseApplying, "192Mi"))},
			[]string{ResultStarted, JudgeHandover}},
		{"without a master for a round", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), noMaster(stepped(at(2), 1, PhaseApplying, "192Mi")),
			moved(stepped(at(3), 1, PhaseDone, "192Mi"))}, []string{ResultStarted, ResultDone, JudgeHandover}},
		{"kill_master in a step", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), moved(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Handover: true}))},
			[]string{ResultStarted}},
		// The handover moves the master role to another pod, but only the
		// killed master may be recreated.
		{"kill_master and the recreate of a replica", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"),
			replicaRecreated(moved(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Handover: true, Pod: "rfr-x-0"})))},
			[]string{ResultStarted, JudgeRestart}},
		{"kill_master and its recreate", []Round{at(0), stepped(at(1), 1, PhaseApplying, "192Mi"),
			masterRecreated(moved(allow(stepped(at(2), 1, PhaseApplying, "192Mi"), Allow{Recreate: true, Handover: true, Pod: "rfr-x-0"})))},
			[]string{ResultStarted}},
		{"outside a step", []Round{at(0), moved(at(1))}, nil},
	} {
		if got := summary(observe(NewJudge(), c.rounds...)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func replicaRecreated(r Round) Round {
	r.Pods[2].UID = "new"
	return r
}

func masterRecreated(r Round) Round {
	r.Pods[0].UID = "new"
	return r
}

func noMaster(r Round) Round {
	r.Master = ""
	return r
}

func TestShrink(t *testing.T) {
	for _, c := range []struct {
		name   string
		rounds []Round
		want   []string
	}{
		{"without a user path", []Round{withLimit(at(0), 192*mi), at(1)}, []string{JudgeShrink, JudgeShrink, JudgeShrink}},
		{"mode Off", []Round{withLimit(at(0), 192*mi), allow(withLimit(at(1), 192*mi), Allow{Shrink: true}), at(2)}, nil},
		{"a raise", []Round{at(0), withLimit(at(1), 192*mi)}, nil},
		// An unknown limit keeps the last known one.
		{"an unknown limit", []Round{withLimit(at(0), 192*mi), withLimit(at(1), 0), withLimit(at(2), 192*mi)}, nil},
		{"a shrink after an unknown limit", []Round{withLimit(at(0), 192*mi), withLimit(at(1), 0), at(2)},
			[]string{JudgeShrink, JudgeShrink, JudgeShrink}},
		{"a recreated pod with a lower limit", []Round{withLimit(at(0), 192*mi), allow(recreatedAll(at(1)), Allow{Recreate: true})},
			[]string{JudgeShrink, JudgeShrink, JudgeShrink}},
	} {
		if got := summary(observe(NewJudge(), c.rounds...)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func recreatedAll(r Round) Round {
	for i := range r.Pods {
		r.Pods[i].UID += "-new"
	}
	return r
}

func TestMaxMemory(t *testing.T) {
	high := func(r Round) Round {
		r.Pods[0].MaxMemory = 129 * mi
		return r
	}
	j := NewJudge()
	if got := summary(observe(j, at(0), high(at(1)), high(at(2)))); !slices.Equal(got, []string{JudgeMaxMemory}) {
		t.Errorf("one report: %v", got)
	}
	if got := summary(observe(j, at(3), high(at(4)))); !slices.Equal(got, []string{JudgeMaxMemory}) {
		t.Errorf("a report after the pod held again: %v", got)
	}
	// A change by hand lowers the limit one round before maxmemory follows.
	j = NewJudge()
	lowered := allow(high(at(6)), AllowAll)
	if got := summary(observe(j, at(5), lowered, high(at(7)))); len(got) != 0 {
		t.Errorf("the round after a user path: %v", got)
	}
	if got := summary(observe(j, high(at(8)))); !slices.Equal(got, []string{JudgeMaxMemory}) {
		t.Errorf("two rounds after a user path: %v", got)
	}
	// An unknown maxmemory or limit is no violation.
	r := at(5)
	r.Pods[0].Limit = 0
	if got := summary(observe(NewJudge(), high(r))); len(got) != 0 {
		t.Errorf("unknown limit: %v", got)
	}
}
