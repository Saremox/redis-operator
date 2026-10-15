package autoscale

import (
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
	if e[0].Message != "from 128Mi to 192Mi" {
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
		e = done(j, 10+20*i, 11+20*i, limit, limit+64*mi)
		limit += 64 * mi
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
		e = append(e[:0], done(j, 10+20*i, 11+20*i, limit, limit+64*mi)...)
		if slices.Contains(summary(e), JudgeRate) {
			t.Errorf("step %d after a reset: %v", i, e)
		}
		limit += 64 * mi
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

// A revert without retryAfter starts no back-off check.
func TestRevertWithoutRetryAfter(t *testing.T) {
	j := NewJudge()
	e := observe(j, at(0), stepped(at(1), 1, PhaseApplying, "192Mi"), stepped(at(2), 1, PhaseReverted, ""))
	if got := summary(e); !slices.Equal(got, []string{ResultStarted, ResultReverted}) || !j.retryAt.IsZero() {
		t.Fatalf("%v, retryAt %s", got, j.retryAt)
	}
	if got := summary(observe(j, stepped(at(3), 3, PhaseApplying, "192Mi"))); !slices.Equal(got, []string{ResultStarted}) {
		t.Errorf("the next step: %v", got)
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
		{"outside a step", []Round{at(0), moved(at(1))}, nil},
	} {
		if got := summary(observe(NewJudge(), c.rounds...)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
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
	// An unknown maxmemory or limit is no violation.
	r := at(5)
	r.Pods[0].Limit = 0
	if got := summary(observe(NewJudge(), high(r))); len(got) != 0 {
		t.Errorf("unknown limit: %v", got)
	}
}
