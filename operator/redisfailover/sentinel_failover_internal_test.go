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
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
)

// levelLogger records the Info, Warning and Debug lines with the level in front.
type levelLogger struct {
	log.DummyLogger
	lines *[]string
}

func (l levelLogger) With(string, interface{}) log.Logger      { return l }
func (l levelLogger) WithField(string, interface{}) log.Logger { return l }
func (l levelLogger) Infof(format string, args ...interface{}) {
	*l.lines = append(*l.lines, "info: "+fmt.Sprintf(format, args...))
}

func (l levelLogger) Warningf(format string, args ...interface{}) {
	*l.lines = append(*l.lines, "warning: "+fmt.Sprintf(format, args...))
}

func (l levelLogger) Debugf(format string, args ...interface{}) {
	*l.lines = append(*l.lines, "debug: "+fmt.Sprintf(format, args...))
}

const (
	sfMaster    = "10.0.0.2"
	sfOldMaster = "10.0.0.1"
)

var sfSentinels = []string{"10.0.1.1", "10.0.1.2", "10.0.1.3"}

// sentinelFailoverTest runs CheckAndHeal with Sentinels that monitor the old
// master after a Sentinel failover. The Redis checks pass.
type sentinelFailoverTest struct {
	rf       *redisfailoverv1.RedisFailover
	check    *mRFService.RedisFailoverCheck
	heal     *mRFService.RedisFailoverHeal
	handler  *RedisFailoverHandler
	clock    time.Time
	lines    []string
	requeues []time.Duration
	// readErr is the error of each read of the failover_in_progress flag.
	readErr error
}

func newSentinelFailoverTest(bootstrap bool) *sentinelFailoverTest {
	ft := &sentinelFailoverTest{
		rf:    newCustomConfigTestRF(),
		clock: time.Unix(1000, 0),
	}
	ft.rf.Spec.Sentinel.Enabled = ptr.To(true)
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

// reconcile runs CheckAndHeal. wrong lists the Sentinels that monitor the old
// master, and leader the Sentinel with the failover_in_progress flag. It
// returns the Sentinels that got a new monitor.
func (ft *sentinelFailoverTest) reconcile(t *testing.T, wrong []string, leader string, timeout time.Duration) []string {
	t.Helper()
	ft.check.ExpectedCalls, ft.check.Calls = nil, nil
	ft.heal.ExpectedCalls, ft.heal.Calls = nil, nil
	ft.lines, ft.requeues = nil, nil
	c, h, rf := ft.check, ft.heal, ft.rf
	h.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	h.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	c.On("IsRedisRunning", rf).Maybe().Return(true)
	c.On("IsSentinelRunning", rf).Maybe().Return(true)
	c.On("IsSentinelRunningQuorum", rf).Maybe().Return(true)
	c.On("GetNumberMasters", rf).Maybe().Return(1, nil)
	c.On("GetMasterIP", rf).Maybe().Return(sfMaster, nil)
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
	for _, s := range sfSentinels {
		var err error
		if slices.Contains(wrong, s) {
			err = fmt.Errorf("sentinel monitoring %s:0 instead %s:0", sfOldMaster, sfMaster)
		}
		c.On("CheckSentinelMonitor", s, sfMaster, mock.Anything).Maybe().Return(err)
		c.On("SentinelFailoverInProgress", s).Maybe().Return(s == leader, timeout, ft.readErr)
		h.On("NewSentinelMonitor", s, sfMaster, rf).Maybe().Return(nil)
		h.On("NewSentinelMonitorWithPort", s, sfMaster, "6379", rf).Maybe().Return(nil)
		c.On("CheckSentinelNumberInMemory", s, rf).Maybe().Return(nil)
		c.On("CheckSentinelSlavesNumberInMemory", s, rf).Maybe().Return(nil)
		h.On("SetSentinelCustomConfig", s, rf).Maybe().Return(nil)
	}
	require.NoError(t, ft.handler.CheckAndHeal(rf))
	var monitored []string
	for _, call := range h.Calls {
		if call.Method == "NewSentinelMonitor" || call.Method == "NewSentinelMonitorWithPort" {
			monitored = append(monitored, call.Arguments.String(0))
		}
	}
	return monitored
}

const sfFixLine = "warning: Fixing sentinel not monitoring expected master: sentinel monitoring 10.0.0.1:0 instead 10.0.0.2:0"

const sfWaitLine = "Sentinel 10.0.1.3 leads a failover, so the operator does not change the Sentinels until the failover ends"

// TestSentinelFailoverInProgressHoldsTheNewMonitor covers the evidence of the
// soak test: SENTINEL REMOVE and MONITOR during the failover made the
// Sentinels go back to the old master. Only the leader has the flag, so the
// two Sentinels before it in the list do not get a new monitor either.
func TestSentinelFailoverInProgressHoldsTheNewMonitor(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(fmt.Sprintf("bootstrap=%v", bootstrap), func(t *testing.T) {
			ft := newSentinelFailoverTest(bootstrap)

			assert.Empty(t, ft.reconcile(t, sfSentinels, sfSentinels[2], 10*time.Second))
			assert.Equal(t, []string{sfFixLine, "info: " + sfWaitLine}, ft.lines)
			assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
			assert.Equal(t, redisfailoverv1.HealthyState, ft.rf.Status.State)
			// A SENTINEL RESET also ends the failover in the leader.
			ft.check.AssertNotCalled(t, "CheckSentinelNumberInMemory", mock.Anything, mock.Anything)
			ft.heal.AssertNotCalled(t, "SetSentinelCustomConfig", mock.Anything, mock.Anything)

			// The next reconcile in the same failover logs at Debug.
			ft.clock = ft.clock.Add(3 * time.Second)
			assert.Empty(t, ft.reconcile(t, sfSentinels, sfSentinels[2], 10*time.Second))
			assert.Equal(t, []string{sfFixLine, "debug: " + sfWaitLine}, ft.lines)
			assert.Equal(t, []time.Duration{7 * time.Second}, ft.requeues)

			// After the failover, a Sentinel that still monitors the old master
			// gets a new monitor, as before.
			ft.clock = ft.clock.Add(time.Second)
			assert.Equal(t, []string{sfSentinels[0]}, ft.reconcile(t, sfSentinels[:1], "", 10*time.Second))
			assert.Empty(t, ft.requeues)
			_, waiting := ft.handler.sentinelFailovers.Load("testns/test")
			assert.False(t, waiting)
		})
	}
}

// TestSentinelWithoutFailoverGetsANewMonitor covers the normal heal: a
// Sentinel that monitors a wrong master and leads no failover gets a new
// monitor at once. An error of the flag read gives the same heal.
func TestSentinelWithoutFailoverGetsANewMonitor(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(fmt.Sprintf("bootstrap=%v", bootstrap), func(t *testing.T) {
			ft := newSentinelFailoverTest(bootstrap)
			assert.Equal(t, sfSentinels[:2], ft.reconcile(t, sfSentinels[:2], "", 0))
			assert.Empty(t, ft.requeues)
			ft.heal.AssertCalled(t, "SetSentinelCustomConfig", sfSentinels[2], ft.rf)
		})
	}
	t.Run("the flag read fails", func(t *testing.T) {
		ft := newSentinelFailoverTest(false)
		ft.readErr = errors.New("ERR unknown subcommand")
		assert.Equal(t, sfSentinels, ft.reconcile(t, sfSentinels, sfSentinels[2], 10*time.Second))
		assert.Empty(t, ft.requeues)
	})
}

// TestStuckSentinelFailoverEndsTheWait checks that the wait ends after the
// failover-timeout of the leader, so a failover that does not end does not
// stop the heal.
func TestStuckSentinelFailoverEndsTheWait(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	assert.Empty(t, ft.reconcile(t, sfSentinels, sfSentinels[2], 10*time.Second))

	ft.clock = ft.clock.Add(10 * time.Second)
	assert.Equal(t, sfSentinels, ft.reconcile(t, sfSentinels, sfSentinels[2], 10*time.Second))
	assert.Equal(t, []string{
		sfFixLine,
		"warning: Sentinel 10.0.1.3 leads a failover for 10s, longer than its failover-timeout 10s, so the operator heals the Sentinels",
		sfFixLine,
		sfFixLine,
	}, ft.lines)
	assert.Empty(t, ft.requeues)
}

// TestSentinelFailoverWaitUsesTheDefaultTimeout covers a Sentinel that gives
// no failover-timeout.
func TestSentinelFailoverWaitUsesTheDefaultTimeout(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	assert.Empty(t, ft.reconcile(t, sfSentinels, sfSentinels[2], 0))
	assert.Equal(t, []time.Duration{10 * time.Second}, ft.requeues)
}

// TestSentinelFailoverWaitStartsAgain checks that a reconcile in which each
// Sentinel monitors the master clears the wait. A later failover then gets
// the full wait.
func TestSentinelFailoverWaitStartsAgain(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		t.Run(fmt.Sprintf("bootstrap=%v", bootstrap), func(t *testing.T) {
			ft := newSentinelFailoverTest(bootstrap)
			assert.Empty(t, ft.reconcile(t, sfSentinels, sfSentinels[2], 10*time.Second))

			ft.clock = ft.clock.Add(2 * time.Second)
			assert.Empty(t, ft.reconcile(t, nil, "", 10*time.Second))
			ft.check.AssertNotCalled(t, "SentinelFailoverInProgress", mock.Anything)
			_, waiting := ft.handler.sentinelFailovers.Load("testns/test")
			assert.False(t, waiting)

			ft.clock = ft.clock.Add(time.Hour)
			assert.Empty(t, ft.reconcile(t, sfSentinels, sfSentinels[2], 10*time.Second))
			assert.Equal(t, []string{sfFixLine, "info: " + sfWaitLine}, ft.lines)
		})
	}
}

// TestSentinelFailoverWaitSkipsAnUnreachableSentinel checks that the flag read
// does not dial a Sentinel that did not answer earlier in the reconcile.
func TestSentinelFailoverWaitSkipsAnUnreachableSentinel(t *testing.T) {
	ft := newSentinelFailoverTest(false)
	gone := sfSentinels[0]
	ft.check.On("SentinelFailoverInProgress", sfSentinels[1]).Once().Return(false, 10*time.Second, nil)
	ft.check.On("SentinelFailoverInProgress", sfSentinels[2]).Once().Return(true, 10*time.Second, nil)
	heal := ft.handler.startSentinelHeal(ft.rf)
	heal.unreachable[gone] = true

	assert.True(t, ft.handler.waitForSentinelFailover(ft.rf, sfSentinels, heal))

	ft.check.AssertNotCalled(t, "SentinelFailoverInProgress", gone)
}
