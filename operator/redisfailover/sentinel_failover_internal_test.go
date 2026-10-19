package redisfailover

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

// levelLogger records the Info, Warning and Debug lines with the level in front.
type levelLogger struct {
	log.DummyLogger
	lines *[]string
}

func (l levelLogger) With(string, any) log.Logger      { return l }
func (l levelLogger) WithField(string, any) log.Logger { return l }
func (l levelLogger) Infof(format string, args ...any) {
	*l.lines = append(*l.lines, "info: "+fmt.Sprintf(format, args...))
}

func (l levelLogger) Warningf(format string, args ...any) {
	*l.lines = append(*l.lines, "warning: "+fmt.Sprintf(format, args...))
}

func (l levelLogger) Debugf(format string, args ...any) {
	*l.lines = append(*l.lines, "debug: "+fmt.Sprintf(format, args...))
}

const (
	sfMaster    = "10.0.0.2"
	sfOldMaster = "10.0.0.1"
)

var sfSentinels = []string{"10.0.1.1", "10.0.1.2", "10.0.1.3"}

// failingOver is the failover state of a Sentinel that fails over the old
// master.
func failingOver(state string, timeout time.Duration) []redis.SentinelFailover {
	return failingOverMaster(sfOldMaster+":6379", state, timeout)
}

func failingOverMaster(master, state string, timeout time.Duration) []redis.SentinelFailover {
	return []redis.SentinelFailover{{InProgress: true, State: state, Timeout: timeout, Master: master}}
}

// sfReconcile is the state of the Sentinels in one reconcile.
type sfReconcile struct {
	// wrong holds the Sentinels that monitor the old master.
	wrong []string
	// mismatch holds the Sentinels that know a wrong number of Sentinels.
	mismatch []string
	// failover holds the replies of GetSentinelFailover of each Sentinel, in
	// order. The last reply repeats. A Sentinel without replies has no failover.
	failover map[string][]redis.SentinelFailover
	// readErr holds the error of GetSentinelFailover of each Sentinel.
	readErr map[string]error
}

// sfResult is what a reconcile did.
type sfResult struct {
	err       error
	monitored []string
	reset     []string
	reads     int
}

// sentinelFailoverTest runs CheckAndHeal after a Sentinel failover. The Redis
// checks pass.
type sentinelFailoverTest struct {
	rf       *redisfailoverv1.RedisFailover
	check    *mRFService.RedisFailoverCheck
	heal     *mRFService.RedisFailoverHeal
	handler  *RedisFailoverHandler
	clock    time.Time
	lines    []string
	requeues []time.Duration
}

func newSentinelFailoverTest(bootstrap bool) *sentinelFailoverTest {
	ft := &sentinelFailoverTest{
		rf:    newCustomConfigTestRF(),
		clock: time.Unix(1000, 0),
	}
	ft.rf.Spec.Sentinel.Enabled = new(true)
	if bootstrap {
		ft.rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: sfMaster, Port: "6379", AllowSentinels: true}
	}
	mk := noAppliedSecret(&mK8SService.Services{})
	mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Maybe().Return(&corev1.PodList{}, nil)
	mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe().Return()
	ft.check = &mRFService.RedisFailoverCheck{}
	ft.heal = &mRFService.RedisFailoverHeal{}
	ft.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, ft.check, ft.heal, mk, metrics.Dummy, levelLogger{lines: &ft.lines})
	ft.handler.now = func() time.Time { return ft.clock }
	ft.handler.requeue = func(key string, after time.Duration) {
		if key == "testns/test" {
			ft.requeues = append(ft.requeues, after)
		}
	}
	return ft
}

func (ft *sentinelFailoverTest) run(s sfReconcile) sfResult {
	ft.check.ExpectedCalls, ft.check.Calls = nil, nil
	ft.heal.ExpectedCalls, ft.heal.Calls = nil, nil
	ft.lines, ft.requeues = nil, nil
	c, h, rf := noUnknownSentinelAddrs(ft.check), ft.heal, ft.rf
	h.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	h.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	c.On("IsRedisRunning", rf).Maybe().Return(true)
	c.On("IsSentinelRunning", rf).Maybe().Return(true)
	c.On("IsSentinelRunningQuorum", rf).Maybe().Return(true)
	c.On("GetNumberMasters", rf).Maybe().Return(1, nil)
	c.On("GetMasterIP", rf).Maybe().Return(sfMaster, nil)
	c.On("GetSentinelMasterIPs", rf).Maybe().Return(nil, nil)
	c.On("CheckAllSlavesFromMaster", sfMaster, rf).Maybe().Return(nil)
	c.On("GetRedisesIPs", rf).Maybe().Return([]string{sfMaster}, nil)
	h.On("SetRedisCustomConfig", sfMaster, rf).Maybe().Return(nil)
	h.On("SetExternalMasterOnAll", sfMaster, "6379", rf).Maybe().Return(nil)
	c.On("GetStatefulSetUpdateRevision", rf).Maybe().Return("1", nil)
	c.On("GetRedisesSlavesPods", rf).Maybe().Return([]string{}, nil)
	c.On("GetRedisesMasterPod", rf).Maybe().Return("rfr-test-1", nil)
	c.On("GetRedisRevisionHash", "rfr-test-1", rf).Maybe().Return("1", nil)
	c.On("CheckRedisSlavesReady", mock.Anything, rf).Maybe().Return(true, nil)
	c.On("GetSentinelsIPs", rf).Maybe().Return(sfSentinels, nil)
	for _, sip := range sfSentinels {
		var wrong, mismatch error
		if slices.Contains(s.wrong, sip) {
			wrong = fmt.Errorf("sentinel monitoring %s:0 instead %s:0", sfOldMaster, sfMaster)
		}
		if slices.Contains(s.mismatch, sip) {
			mismatch = errors.New("sentinels in memory mismatch")
		}
		c.On("CheckSentinelMonitor", sip, sfMaster, mock.Anything).Maybe().Return(wrong)
		replies := s.failover[sip]
		if len(replies) == 0 {
			replies = []redis.SentinelFailover{{}}
		}
		for i, reply := range replies {
			call := c.On("GetSentinelFailover", sip).Maybe().Return(reply, s.readErr[sip])
			if i < len(replies)-1 {
				call.Once()
			}
		}
		h.On("NewSentinelMonitor", sip, sfMaster, rf).Maybe().Return(nil)
		h.On("NewSentinelMonitorWithPort", sip, sfMaster, "6379", rf).Maybe().Return(nil)
		c.On("CheckSentinelNumberInMemory", sip, rf).Maybe().Return(mismatch)
		c.On("CheckSentinelSlavesNumberInMemory", sip, rf).Maybe().Return(nil)
		h.On("RestoreSentinel", sip).Maybe().Return(nil)
		h.On("SetSentinelCustomConfig", sip, rf).Maybe().Return(nil)
	}
	res := sfResult{err: ft.handler.CheckAndHeal(rf)}
	for _, call := range h.Calls {
		switch call.Method {
		case "NewSentinelMonitor", "NewSentinelMonitorWithPort":
			res.monitored = append(res.monitored, call.Arguments.String(0))
		case "RestoreSentinel":
			res.reset = append(res.reset, call.Arguments.String(0))
		}
	}
	for _, call := range c.Calls {
		if call.Method == "GetSentinelFailover" {
			res.reads++
		}
	}
	return res
}

// reconcile runs a reconcile that returns no error.
func (ft *sentinelFailoverTest) reconcile(t *testing.T, s sfReconcile) sfResult {
	t.Helper()
	res := ft.run(s)
	require.NoError(t, res.err)
	return res
}

func (ft *sentinelFailoverTest) assertHold(t *testing.T) {
	t.Helper()
	assert.Equal(t, redisfailoverv1.NotHealthyState, ft.rf.Status.State)
	assert.Equal(t, sentinelFailoverMsg, ft.rf.Status.Message)
}

const (
	sfFixLine  = "warning: Fixing sentinel not monitoring expected master: sentinel monitoring 10.0.0.1:0 instead 10.0.0.2:0"
	sfWaitLine = "Sentinel 10.0.1.3 fails over, so the operator sends no SENTINEL MONITOR or RESET until the failover ends"
)

// TestSentinelFailoverInProgressHoldsTheNewMonitor covers the evidence of the
// soak test: SENTINEL REMOVE and MONITOR during the failover made the
// Sentinels return to the old master. Only the Sentinel that fails over has
// the flag, so the two Sentinels before it in the list get no new monitor too.
func TestSentinelFailoverInProgressHoldsTheNewMonitor(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(fmt.Sprintf("bootstrap=%v", bootstrap), func(t *testing.T) {
			ft := newSentinelFailoverTest(bootstrap)
			inFailover := sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
				sfSentinels[2]: failingOver("wait_promotion", 10*time.Second),
			}}

			res := ft.reconcile(t, inFailover)
			assert.Empty(t, res.monitored)
			assert.Equal(t, 3, res.reads)
			assert.Equal(t, []string{sfFixLine, "info: " + sfWaitLine, sfFixLine, sfFixLine}, ft.lines)
			assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
			ft.assertHold(t)
			// SENTINEL SET does not end a failover.
			ft.heal.AssertCalled(t, "SetSentinelCustomConfig", sfSentinels[2], ft.rf)

			// The next reconcile in the same failover logs at Debug.
			ft.clock = ft.clock.Add(3 * time.Second)
			res = ft.reconcile(t, inFailover)
			assert.Empty(t, res.monitored)
			assert.Contains(t, ft.lines, "debug: "+sfWaitLine)
			assert.Equal(t, []time.Duration{7 * time.Second}, ft.requeues)
			ft.assertHold(t)

			// After the failover, a Sentinel that still monitors the old master
			// gets a new monitor, as before.
			ft.clock = ft.clock.Add(time.Second)
			res = ft.reconcile(t, sfReconcile{wrong: sfSentinels[:1]})
			assert.Equal(t, sfSentinels[:1], res.monitored)
			assert.Empty(t, ft.requeues)
			assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
			_, waiting := ft.handler.sentinelFailovers.Load("testns/test")
			assert.False(t, waiting)
		})
	}
}

// TestSentinelFailoverInProgressHoldsTheReset covers a Sentinel that fails
// over and monitors the master that the operator read. SENTINEL RESET also
// ends its failover.
func TestSentinelFailoverInProgressHoldsTheReset(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	res := ft.reconcile(t, sfReconcile{mismatch: sfSentinels[2:], failover: map[string][]redis.SentinelFailover{
		sfSentinels[2]: failingOver("wait_promotion", 10*time.Second),
	}})
	assert.Empty(t, res.reset)
	assert.Equal(t, 3, res.reads)
	ft.assertHold(t)
	assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
}

// TestSentinelHealWithoutFailover covers the normal heals: they read the
// failover state only before a change, and a Sentinel without a failover gets
// the change at once.
func TestSentinelHealWithoutFailover(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(fmt.Sprintf("bootstrap=%v", bootstrap), func(t *testing.T) {
			ft := newSentinelFailoverTest(bootstrap)
			res := ft.reconcile(t, sfReconcile{})
			assert.Zero(t, res.reads)
			assert.Empty(t, res.monitored)

			// Each change reads the state of its Sentinel again.
			res = ft.reconcile(t, sfReconcile{wrong: sfSentinels[:2]})
			assert.Equal(t, sfSentinels[:2], res.monitored)
			assert.Equal(t, 5, res.reads)
			assert.Empty(t, ft.requeues)
			assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
		})
	}
	t.Run("reset", func(t *testing.T) {
		ft := newSentinelFailoverTest(false)
		res := ft.reconcile(t, sfReconcile{mismatch: sfSentinels[2:]})
		assert.Equal(t, sfSentinels[2:], res.reset)
		assert.Equal(t, 4, res.reads)
		assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
	})
}

// TestSentinelFailoverStartsAfterTheFirstRead covers a failover that starts
// after the first read. The read before the next change finds it.
func TestSentinelFailoverStartsAfterTheFirstRead(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	res := ft.reconcile(t, sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
		sfSentinels[2]: {{}, {InProgress: true, State: "wait_start", Timeout: 10 * time.Second}},
	}})
	assert.Equal(t, sfSentinels[:2], res.monitored)
	assert.Equal(t, 6, res.reads)
	ft.assertHold(t)
}

// TestSentinelFailoverStartsBeforeTheFirstChange covers a failover that
// starts on the first target after the read of all the Sentinels. The read of
// the target just before the change finds it.
func TestSentinelFailoverStartsBeforeTheFirstChange(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%v", mismatch), func(t *testing.T) {
			ft := newSentinelFailoverTest(false)
			state := sfReconcile{wrong: sfSentinels[:1], failover: map[string][]redis.SentinelFailover{
				sfSentinels[0]: {{}, {InProgress: true, State: "wait_start", Timeout: 10 * time.Second}},
			}}
			if mismatch {
				state.wrong, state.mismatch = nil, sfSentinels[:1]
			}
			res := ft.reconcile(t, state)
			assert.Empty(t, res.monitored)
			assert.Empty(t, res.reset)
			assert.Equal(t, 4, res.reads)
			ft.assertHold(t)
		})
	}
}

// TestSentinelFailoverMissingMaster covers a Sentinel without the master,
// for example after SENTINEL REMOVE. It has no failover, so it gets the heal
// at once.
func TestSentinelFailoverMissingMaster(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	res := ft.reconcile(t, sfReconcile{wrong: sfSentinels[2:], readErr: map[string]error{
		sfSentinels[2]: errors.New("ERR No such master with that name"),
	}})
	assert.Equal(t, sfSentinels[2:], res.monitored)
	assert.Empty(t, ft.requeues)
	assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
}

// TestSentinelFailoverReadError covers a Sentinel that answers the read with
// another error, for example an ACL error. That Sentinel can fail over, so it
// gets no change until one failover-timeout after the first error.
func TestSentinelFailoverReadError(t *testing.T) {
	for _, readErr := range []error{
		errors.New("NOPERM this user has no permissions to run the 'sentinel|master' command"),
		errors.New("malformed SENTINEL MASTER reply"),
	} {
		t.Run(readErr.Error(), func(t *testing.T) {
			ft := newSentinelFailoverTest(false)
			state := sfReconcile{wrong: sfSentinels[1:], readErr: map[string]error{sfSentinels[2]: readErr}}
			held := "warning: Sentinel 10.0.1.3 did not give its failover state, so the operator does not change it for 10s: " + readErr.Error()

			waiting := "waiting to read the failover state of sentinel 10.0.1.3"
			res := ft.reconcile(t, state)
			assert.Equal(t, sfSentinels[1:2], res.monitored)
			assert.Contains(t, ft.lines, held)
			assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
			assert.Equal(t, redisfailoverv1.NotHealthyState, ft.rf.Status.State)
			assert.Equal(t, waiting, ft.rf.Status.Message)

			// The Warning is logged one time.
			ft.clock = ft.clock.Add(6 * time.Second)
			res = ft.reconcile(t, state)
			assert.Equal(t, sfSentinels[1:2], res.monitored)
			assert.NotContains(t, ft.lines, held)
			assert.Equal(t, []time.Duration{4 * time.Second}, ft.requeues)
			assert.Equal(t, waiting, ft.rf.Status.Message)

			ft.clock = ft.clock.Add(4 * time.Second)
			res = ft.reconcile(t, state)
			assert.Equal(t, sfSentinels[1:], res.monitored)
			assert.Contains(t, ft.lines, "warning: Sentinel 10.0.1.3 did not give its failover state for 10s, so the operator heals it: "+readErr.Error())
			assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
			assert.Empty(t, ft.rf.Status.Message)

			// A read without an error clears the time of the first error.
			ft.clock = ft.clock.Add(time.Second)
			ft.reconcile(t, sfReconcile{wrong: sfSentinels[2:]})
			_, stored := ft.handler.sentinelReadErrors.Load("testns/test")
			assert.False(t, stored)
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		ft := newSentinelFailoverTest(false)
		res := ft.run(sfReconcile{wrong: sfSentinels, readErr: map[string]error{
			sfSentinels[2]: errors.New("dial tcp 10.0.1.3:26379: i/o timeout"),
		}})
		require.EqualError(t, res.err, "sentinel(s) 10.0.1.3 unreachable")
		assert.Equal(t, sfSentinels[:2], res.monitored)
	})
}

// TestSentinelFailoverReadErrorWithoutAChange checks that a reconcile without
// a change clears the time of the first read error.
func TestSentinelFailoverReadErrorWithoutAChange(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	ft.reconcile(t, sfReconcile{wrong: sfSentinels[2:], readErr: map[string]error{sfSentinels[2]: errors.New("NOPERM")}})
	ft.reconcile(t, sfReconcile{})
	_, stored := ft.handler.sentinelReadErrors.Load("testns/test")
	assert.False(t, stored)
}

// TestSentinelFailoverOfAnotherMaster covers two failovers of one Sentinel
// without a reconcile between them. The second failover starts from the
// promoted replica of the first one, so it gets its own wait.
func TestSentinelFailoverOfAnotherMaster(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	ft.reconcile(t, sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
		sfSentinels[2]: failingOverMaster("10.0.0.1:6379", "wait_promotion", 10*time.Second),
	}})

	ft.clock = ft.clock.Add(10 * time.Second)
	res := ft.reconcile(t, sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
		sfSentinels[2]: failingOverMaster("10.0.0.2:6379", "reconf_slaves", 10*time.Second),
	}})
	assert.Empty(t, res.monitored)
	assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
	ft.assertHold(t)
}

// TestSentinelFailoverWaitEndsAfterOneTimeout follows one failover through
// its states on a 60 s clock. The master address stays the same until the
// failover ends, so the wait ends one failover-timeout after the first read.
func TestSentinelFailoverWaitEndsAfterOneTimeout(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	for elapsed := time.Duration(0); elapsed <= time.Minute; elapsed += 2 * time.Second {
		state := "wait_promotion"
		if elapsed >= 4*time.Second {
			state = "reconf_slaves"
		}
		ft.clock = time.Unix(1000, 0).Add(elapsed)
		res := ft.reconcile(t, sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
			sfSentinels[2]: failingOver(state, 10*time.Second),
		}})
		if elapsed < 10*time.Second {
			assert.Empty(t, res.monitored, elapsed)
		} else {
			assert.Equal(t, sfSentinels, res.monitored, elapsed)
		}
	}
}

// TestSentinelFailoverCandidateDoesNotExtendTheWait covers a leader that does
// not end its failover and a candidate that keeps the flag. The read of the
// candidate before its change holds the candidate only and starts no new
// wait, so the other Sentinels wait one failover-timeout only.
func TestSentinelFailoverCandidateDoesNotExtendTheWait(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	stuck := sfReconcile{wrong: sfSentinels[1:], failover: map[string][]redis.SentinelFailover{
		sfSentinels[0]: failingOver("reconf_slaves", 10*time.Second),
		sfSentinels[2]: failingOver("wait_start", 10*time.Second),
	}}
	assert.Empty(t, ft.reconcile(t, stuck).monitored)
	for elapsed := 10 * time.Second; elapsed <= time.Minute; elapsed += 5 * time.Second {
		ft.clock = time.Unix(1000, 0).Add(elapsed)
		assert.Equal(t, sfSentinels[1:2], ft.reconcile(t, stuck).monitored, elapsed)
		ft.assertHold(t)
		v, _ := ft.handler.sentinelFailovers.Load("testns/test")
		assert.Equal(t, sentinelFailover{sentinel: sfSentinels[0], master: sfOldMaster + ":6379", state: 4, since: time.Unix(1000, 0)}, v, elapsed)
	}

	// A candidate loses the flag at the end of the election.
	delete(stuck.failover, sfSentinels[2])
	assert.Equal(t, sfSentinels[1:], ft.reconcile(t, stuck).monitored)
	assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
}

// TestSentinelFailoverCandidateHoldsOnlyItself covers a candidate before the
// other targets in the list. Its hold does not hold the next Sentinel.
func TestSentinelFailoverCandidateHoldsOnlyItself(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	stuck := sfReconcile{wrong: sfSentinels[:2], failover: map[string][]redis.SentinelFailover{
		sfSentinels[0]: failingOver("wait_start", 10*time.Second),
		sfSentinels[2]: failingOver("reconf_slaves", 10*time.Second),
	}}
	assert.Empty(t, ft.reconcile(t, stuck).monitored)
	ft.clock = ft.clock.Add(10 * time.Second)
	assert.Equal(t, sfSentinels[1:2], ft.reconcile(t, stuck).monitored)
}

// TestSentinelFailoverAfterAnExpiredWait covers a new failover that starts
// between the read of all the Sentinels and the read of the target, after the
// wait of an earlier failover ended. The target gets no change, and the next
// read of all the Sentinels starts a full wait.
func TestSentinelFailoverAfterAnExpiredWait(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%v", mismatch), func(t *testing.T) {
			ft := newSentinelFailoverTest(false)
			ft.reconcile(t, sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
				sfSentinels[2]: failingOverMaster("10.0.0.1:6379", "reconf_slaves", 10*time.Second),
			}})

			second := failingOverMaster("10.0.0.2:6379", "wait_start", 10*time.Second)[0]
			ft.clock = ft.clock.Add(12 * time.Second)
			state := sfReconcile{wrong: sfSentinels[2:], failover: map[string][]redis.SentinelFailover{
				sfSentinels[2]: {{}, second},
			}}
			if mismatch {
				state.wrong, state.mismatch = nil, sfSentinels[2:]
			}
			res := ft.reconcile(t, state)
			assert.Empty(t, res.monitored)
			assert.Empty(t, res.reset)
			ft.assertHold(t)
			assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)

			ft.clock = ft.clock.Add(time.Second)
			state.failover[sfSentinels[2]] = []redis.SentinelFailover{second}
			res = ft.reconcile(t, state)
			assert.Empty(t, res.monitored)
			assert.Empty(t, res.reset)
			assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
			v, _ := ft.handler.sentinelFailovers.Load("testns/test")
			stored, _ := v.(sentinelFailover)
			assert.Equal(t, ft.clock, stored.since)
		})
	}
}

// TestStuckSentinelFailoverEndsTheWait checks that the wait ends after the
// failover-timeout, so a failover that does not end does not stop the heal.
func TestStuckSentinelFailoverEndsTheWait(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	inFailover := sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
		sfSentinels[2]: failingOver("reconf_slaves", 10*time.Second),
	}}
	assert.Empty(t, ft.reconcile(t, inFailover).monitored)

	ft.clock = ft.clock.Add(10 * time.Second)
	assert.Equal(t, sfSentinels, ft.reconcile(t, inFailover).monitored)
	assert.Contains(t, ft.lines, "warning: Sentinel 10.0.1.3 fails over for 10s, longer than its failover-timeout 10s, so the operator heals the Sentinels")
	assert.Empty(t, ft.requeues)
	assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
}

// TestSentinelFailoverWaitUsesTheDefaultTimeout covers a Sentinel that gives
// no failover-timeout.
func TestSentinelFailoverWaitUsesTheDefaultTimeout(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	ft.reconcile(t, sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
		sfSentinels[2]: failingOver("wait_promotion", 0),
	}})
	assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
}

// TestSentinelFailoverLeader checks that the wait names the Sentinel with the
// highest failover-state. A candidate that loses the election also has the
// flag for a short time.
func TestSentinelFailoverLeader(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	ft.reconcile(t, sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
		sfSentinels[0]: failingOver("wait_start", 10*time.Second),
		sfSentinels[2]: failingOver("wait_promotion", 10*time.Second),
	}})
	assert.Contains(t, ft.lines, "info: "+sfWaitLine)
}

// TestSentinelFailoverWaitStartsAgain checks that a new failover gets the
// full wait: after a reconcile without a change, after a failover of another
// Sentinel, and after an earlier failover-state.
func TestSentinelFailoverWaitStartsAgain(t *testing.T) {
	inFailover := func(sip, state string) sfReconcile {
		return sfReconcile{wrong: sfSentinels, failover: map[string][]redis.SentinelFailover{
			sip: failingOver(state, 10*time.Second),
		}}
	}
	tests := []struct {
		name   string
		second sfReconcile
		// between runs a reconcile without a change between the failovers.
		between bool
	}{
		{name: "a reconcile without a change", second: inFailover(sfSentinels[2], "reconf_slaves"), between: true},
		{name: "another Sentinel", second: inFailover(sfSentinels[0], "reconf_slaves")},
		{name: "an earlier failover-state", second: inFailover(sfSentinels[2], "wait_promotion")},
	}
	for _, test := range tests {
		for _, bootstrap := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s bootstrap=%v", test.name, bootstrap), func(t *testing.T) {
				ft := newSentinelFailoverTest(bootstrap)
				ft.reconcile(t, inFailover(sfSentinels[2], "reconf_slaves"))
				if test.between {
					ft.clock = ft.clock.Add(2 * time.Second)
					assert.Zero(t, ft.reconcile(t, sfReconcile{}).reads)
					_, waiting := ft.handler.sentinelFailovers.Load("testns/test")
					assert.False(t, waiting)
				}

				ft.clock = ft.clock.Add(10 * time.Second)
				assert.Empty(t, ft.reconcile(t, test.second).monitored)
				assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
				ft.assertHold(t)
			})
		}
	}
}

// TestSentinelFailoverReadSkipsAnUnreachableSentinel checks that the read
// does not dial a Sentinel that did not answer earlier in the reconcile.
func TestSentinelFailoverReadSkipsAnUnreachableSentinel(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	gone := sfSentinels[0]
	ft.check.On("GetSentinelFailover", sfSentinels[1]).Once().Return(redis.SentinelFailover{}, nil)
	ft.check.On("GetSentinelFailover", sfSentinels[2]).Once().Return(failingOver("wait_promotion", 10*time.Second)[0], nil)
	heal := ft.handler.startSentinelHeal(ft.rf)
	heal.unreachable[gone] = true

	assert.False(t, ft.handler.sentinelChangeAllowed(ft.rf, sfSentinels, heal, sfSentinels[1]))

	ft.check.AssertNotCalled(t, "GetSentinelFailover", gone)
	ft.check.AssertExpectations(t)
}
