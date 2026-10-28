package redisfailover

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

const (
	mgMaster  = "10.0.0.1"
	mgReplica = "10.0.0.2"
	mgS1      = "10.0.1.1"
	mgS2      = "10.0.1.2"
	mgS3      = "10.0.1.3"
	mgS4      = "10.0.1.4"
	// mgForeign is a Sentinel of another group, mgForeignMaster its master.
	mgForeign       = "10.0.9.9"
	mgForeignMaster = "10.0.9.8"
)

// mgAlive is an entry of a peer that sends hellos, mgDead one of a peer that
// stopped.
func mgAlive(ip, runID string) redis.SentinelPeer {
	return redis.SentinelPeer{IP: ip, RunID: runID, LastHello: time.Second}
}

func mgDead(ip, runID string) redis.SentinelPeer {
	return redis.SentinelPeer{IP: ip, RunID: runID, Down: true, LastHello: 30 * time.Second}
}

// mgGroup is a Sentinel that monitors the master of the RedisFailover.
func mgGroup(peers ...redis.SentinelPeer) redis.SentinelPeers {
	return redis.SentinelPeers{Master: mgMaster, Peers: peers}
}

// mgRound is one reconcile. pods holds the IPs of the Sentinel pods, and
// sentinels the running Sentinels that the heal changes. groups holds the
// answer of each Sentinel.
type mgRound struct {
	after     time.Duration
	pods      []string
	sentinels []string
	groups    map[string]redis.SentinelPeers
	unread    []string
	// monitor gives the CheckSentinelMonitor error of a Sentinel, count the
	// CheckSentinelNumberInMemory error.
	monitor  map[string]error
	count    map[string]error
	resetErr map[string]error
	// failover gives a Sentinel that fails over.
	failover string
}

// mgAddrs gives the answer of GetUnknownSentinelAddrs for the round, as
// RedisFailoverChecker computes it.
func (r mgRound) addrs() rfservice.SentinelAddrs {
	addrs := rfservice.SentinelAddrs{Peers: map[string][]string{}, Masters: map[string][]string{}, Own: map[string]bool{mgMaster: true, mgReplica: true}, Groups: r.groups, Unread: r.unread}
	for _, ip := range r.pods {
		addrs.Own[ip] = true
	}
	for sip, group := range r.groups {
		for _, peer := range group.Peers {
			if !slices.Contains(r.pods, peer.IP) && !slices.Contains(addrs.Peers[peer.IP], sip) {
				addrs.Peers[peer.IP] = append(addrs.Peers[peer.IP], sip)
			}
		}
		if group.Master != mgMaster && group.Master != mgReplica {
			addrs.Masters[group.Master] = append(addrs.Masters[group.Master], sip)
		}
	}
	return addrs
}

// mgResult holds the Sentinel commands of a reconcile and its log lines.
type mgResult struct {
	monitored, reset []string
	lines            []string
	status           redisfailoverv1.RedisFailoverStatus
	requeues         []time.Duration
	err              error
}

// warnings returns the Warning and Info lines, without the lines of the
// count and the replica checks.
func (r mgResult) warnings() []string {
	var lines []string
	for _, line := range r.lines {
		if strings.HasPrefix(line, "warning: ") || strings.HasPrefix(line, "info: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

type mergeTest struct {
	rf      *redisfailoverv1.RedisFailover
	check   *mRFService.RedisFailoverCheck
	heal    *mRFService.RedisFailoverHeal
	handler *RedisFailoverHandler
	rec     *unknownAddrsRecorder
	lines   []string
	clock   time.Time
}

func newMergeTest() *mergeTest {
	mt := &mergeTest{
		rf:    newCustomConfigTestRF(),
		check: &mRFService.RedisFailoverCheck{},
		heal:  &mRFService.RedisFailoverHeal{},
		rec:   &unknownAddrsRecorder{Recorder: metrics.Dummy},
		clock: time.Unix(1000, 0),
	}
	mt.handler = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, mt.check, mt.heal, &mK8SService.Services{}, mt.rec, levelLogger{lines: &mt.lines})
	mt.handler.now = func() time.Time { return mt.clock }
	return mt
}

// reconcile runs the check of the Sentinel addresses and the Sentinel heal,
// as checkAndHealSentinelMode does.
func (mt *mergeTest) reconcile(t *testing.T, round mgRound) mgResult {
	t.Helper()
	mt.clock = mt.clock.Add(round.after)
	mt.check.ExpectedCalls, mt.check.Calls = nil, nil
	mt.heal.ExpectedCalls, mt.heal.Calls = nil, nil
	mt.lines = nil
	var result mgResult
	mt.handler.requeue = func(_ string, after time.Duration) { result.requeues = append(result.requeues, after) }
	mt.rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState}
	mt.check.On("GetUnknownSentinelAddrs", mt.rf).Once().Return(round.addrs(), nil)
	mt.check.On("GetSentinelsIPs", mt.rf).Once().Return(round.sentinels, nil)
	mt.check.On("GetMasterIP", mt.rf).Maybe().Return(mgMaster, nil)
	for _, sip := range round.sentinels {
		mt.check.On("CheckSentinelMonitor", sip, mgMaster, "0").Once().Return(round.monitor[sip])
		mt.check.On("CheckSentinelNumberInMemory", sip, mt.rf).Maybe().Return(round.count[sip])
		mt.check.On("CheckSentinelSlavesNumberInMemory", sip, mt.rf).Maybe().Return(nil)
		failover := redis.SentinelFailover{}
		if sip == round.failover {
			failover = redis.SentinelFailover{InProgress: true, State: "wait_promotion", Master: mgMaster + ":6379"}
		}
		mt.check.On("GetSentinelFailover", sip).Maybe().Return(failover, nil)
		mt.heal.On("NewSentinelMonitor", sip, mgMaster, mt.rf).Maybe().Return(nil)
		mt.heal.On("RestoreSentinel", sip).Maybe().Return(round.resetErr[sip])
		mt.heal.On("SetSentinelCustomConfig", sip, mt.rf).Maybe().Return(nil)
	}

	mt.handler.observeSentinelAddrs(mt.rf)
	result.err = mt.handler.healSentinelMonitors(mt.rf, mgMaster)

	mt.check.AssertExpectations(t)
	for _, call := range mt.heal.Calls {
		switch call.Method {
		case "NewSentinelMonitor":
			result.monitored = append(result.monitored, call.Arguments.String(0))
		case "RestoreSentinel":
			result.reset = append(result.reset, call.Arguments.String(0))
		}
	}
	result.lines, result.status = mt.lines, mt.rf.Status
	return result
}

// mgHealthy is a round of three Sentinels that know each other.
func mgHealthy(after time.Duration) mgRound {
	return mgRound{
		after: after, pods: []string{mgS1, mgS2, mgS3}, sentinels: []string{mgS1, mgS2, mgS3},
		groups: map[string]redis.SentinelPeers{
			mgS1: mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3")),
			mgS2: mgGroup(mgAlive(mgS1, "r1"), mgAlive(mgS3, "r3")),
			mgS3: mgGroup(mgAlive(mgS1, "r1"), mgAlive(mgS2, "r2")),
		},
	}
}

// mgDeleted is a round after the deletion of the pod of mgS3. The other
// Sentinels still know entry at its IP.
func mgDeleted(after time.Duration, entry redis.SentinelPeer) mgRound {
	return mgRound{
		after: after, pods: []string{mgS1, mgS2}, sentinels: []string{mgS1, mgS2},
		groups: map[string]redis.SentinelPeers{
			mgS1: mgGroup(mgAlive(mgS2, "r2"), entry),
			mgS2: mgGroup(mgAlive(mgS1, "r1"), entry),
		},
	}
}

const mgStaleWarning = "warning: Sentinel %s knows the peers 10.0.1.3, which are no Sentinel pods. resetting"

func mgStale(sip string) string { return strings.Replace(mgStaleWarning, "%s", sip, 1) }

// TestStalePeerResetAfterThePodIsGone resets each Sentinel that knows the IP
// of a deleted Sentinel pod, also when the count of the Sentinels is correct.
// The check by count then finds the Sentinel in the hold-off, so the
// Sentinel gets one reset.
func TestStalePeerResetAfterThePodIsGone(t *testing.T) {
	mt := newMergeTest()
	require.Empty(t, mt.reconcile(t, mgHealthy(0)).reset)

	round := mgDeleted(30*time.Second, mgDead(mgS3, "r3"))
	round.count = map[string]error{mgS1: errors.New("sentinels in memory mismatch")}
	// Redis 6.2 can keep two entries of one IP.
	round.groups[mgS2] = mgGroup(mgAlive(mgS1, "r1"), mgDead(mgS3, "r3"), mgDead(mgS3, "r3old"))
	got := mt.reconcile(t, round)

	require.NoError(t, got.err)
	assert.Equal(t, []string{mgS1, mgS2}, got.reset)
	assert.Equal(t, []string{mgStale(mgS1), mgStale(mgS2)}, got.warnings())
	assert.Contains(t, got.lines, "debug: Sentinel 10.0.1.1 mismatch number of sentinels in memory. last reset 0s ago, waiting")
	assert.Equal(t, redisfailoverv1.HealthyState, got.status.State)

	// The entry is gone after the reset.
	round = mgDeleted(30*time.Second, mgDead(mgS3, "r3"))
	round.groups = map[string]redis.SentinelPeers{mgS1: mgGroup(mgAlive(mgS2, "r2")), mgS2: mgGroup(mgAlive(mgS1, "r1"))}
	got = mt.reconcile(t, round)
	assert.Empty(t, got.reset)
	assert.Empty(t, got.warnings())
}

// TestStalePeerResetOnlyOfTheSentinelsThatKnowIt leaves a Sentinel without
// the stale peer alone.
func TestStalePeerResetOnlyOfTheSentinelsThatKnowIt(t *testing.T) {
	mt := newMergeTest()
	round := mgDeleted(0, mgDead(mgS3, "r3"))
	round.groups[mgS2] = mgGroup(mgAlive(mgS1, "r1"))

	got := mt.reconcile(t, round)

	assert.Equal(t, []string{mgS1}, got.reset)
}

// TestStalePeerNoResetWhileThePodExists keeps the peer of a Sentinel pod in
// deletion: the pod list still has it, and its Sentinel still announces
// itself. A pod in each phase counts.
func TestStalePeerNoResetWhileThePodExists(t *testing.T) {
	mt := newMergeTest()
	round := mgDeleted(0, mgAlive(mgS3, "r3"))
	round.pods = []string{mgS1, mgS2, mgS3}

	got := mt.reconcile(t, round)

	assert.Empty(t, got.reset)
	assert.Empty(t, got.warnings())
}

// TestStalePeerResetWaits keeps the hold-off of a Sentinel after a reset or
// a new monitor, and the hold while a Sentinel fails over.
func TestStalePeerResetWaits(t *testing.T) {
	t.Run("hold-off", func(t *testing.T) {
		mt := newMergeTest()
		mt.handler.sentinelResets.Store(failoverKey(mt.rf), map[string]time.Time{mgS1: mt.clock})
		got := mt.reconcile(t, mgDeleted(10*time.Second, mgDead(mgS3, "r3")))
		assert.Equal(t, []string{mgS2}, got.reset)
		assert.Equal(t, []time.Duration{15 * time.Second}, got.requeues)

		got = mt.reconcile(t, mgDeleted(15*time.Second, mgDead(mgS3, "r3")))
		assert.Equal(t, []string{mgS1}, got.reset, "the reset after the hold-off")
	})
	t.Run("failover", func(t *testing.T) {
		mt := newMergeTest()
		round := mgDeleted(0, mgDead(mgS3, "r3"))
		round.failover = mgS2
		got := mt.reconcile(t, round)
		assert.Empty(t, got.reset)
		assert.Equal(t, sentinelFailoverMsg, got.status.Message)
	})
}

// mgForeignRound is a round in which mgS1 knows the alive peer mgForeign.
func mgForeignRound(after time.Duration) mgRound {
	round := mgHealthy(after)
	round.groups[mgS1] = mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3"), mgAlive(mgForeign, "rf"))
	return round
}

// TestStalePeerBackAfterAResetGetsNoSecondReset covers a Sentinel of another
// group that sends hellos. A reset removes it for 2 seconds only, so the
// operator resets the Sentinel one time. After the peer stops, a reset
// removes it.
func TestStalePeerBackAfterAResetGetsNoSecondReset(t *testing.T) {
	mt := newMergeTest()
	assert.Equal(t, []string{mgS1}, mt.reconcile(t, mgForeignRound(0)).reset)
	for range 3 {
		assert.Empty(t, mt.reconcile(t, mgForeignRound(30*time.Second)).reset)
	}

	round := mgForeignRound(30 * time.Second)
	round.groups[mgS1] = mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3"), mgDead(mgForeign, "rf"))
	assert.Equal(t, []string{mgS1}, mt.reconcile(t, round).reset)
}

// TestStalePeerResetErrors stops the heal on a failed reset, and skips a
// Sentinel that does not answer.
func TestStalePeerResetErrors(t *testing.T) {
	t.Run("an error", func(t *testing.T) {
		mt := newMergeTest()
		round := mgDeleted(0, mgDead(mgS3, "r3"))
		mt.check.ExpectedCalls, mt.heal.ExpectedCalls = nil, nil
		mt.heal.On("RestoreSentinel", mgS1).Once().Return(errors.New("ERR unknown subcommand"))
		mt.check.On("GetUnknownSentinelAddrs", mt.rf).Once().Return(round.addrs(), nil)
		mt.check.On("GetSentinelsIPs", mt.rf).Once().Return(round.sentinels, nil)
		mt.check.On("CheckSentinelMonitor", mock.Anything, mgMaster, "0").Return(nil)
		mt.check.On("GetSentinelFailover", mock.Anything).Return(redis.SentinelFailover{}, nil)
		mt.handler.observeSentinelAddrs(mt.rf)

		err := mt.handler.healSentinelMonitors(mt.rf, mgMaster)

		require.EqualError(t, err, "ERR unknown subcommand")
		assert.Equal(t, redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: "unable to restore sentinel"}, mt.rf.Status)
		assert.Empty(t, mt.handler.loadSentinelUnknown(failoverKey(mt.rf)).resets)
	})
	t.Run("unreachable", func(t *testing.T) {
		mt := newMergeTest()
		round := mgDeleted(0, mgDead(mgS3, "r3"))
		round.resetErr = map[string]error{mgS1: errors.New("dial tcp 10.0.1.1:26379: i/o timeout")}
		got := mt.reconcile(t, round)
		assert.EqualError(t, got.err, "sentinel(s) 10.0.1.1 unreachable")
		assert.Equal(t, []string{mgS1, mgS2}, got.reset)
		resets := mt.handler.loadSentinelUnknown(failoverKey(mt.rf)).resets
		assert.Equal(t, map[string]map[string]time.Time{mgS3: {mgS2: mt.clock}}, resets)
	})
	t.Run("unreachable before", func(t *testing.T) {
		mt := newMergeTest()
		round := mgDeleted(0, mgDead(mgS3, "r3"))
		round.monitor = map[string]error{mgS1: errors.New("dial tcp 10.0.1.1:26379: i/o timeout")}
		got := mt.reconcile(t, round)
		assert.Equal(t, []string{mgS2}, got.reset)
	})
}

// TestSentinelPeerMemoryIsBounded drops a reset peer after
// sentinelPeerResetMemory or when a pod gets its IP.
func TestSentinelPeerMemoryIsBounded(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgHealthy(0))
	key := failoverKey(mt.rf)

	mt.reconcile(t, mgForeignRound(0))
	require.Contains(t, mt.handler.loadSentinelUnknown(key).resets, mgForeign)

	// The peer stays away for sentinelPeerResetMemory.
	mt.reconcile(t, mgDeleted(sentinelPeerResetMemory-time.Second, mgDead(mgS3, "r3")))
	state := mt.handler.loadSentinelUnknown(key)
	assert.Contains(t, state.resets, mgForeign)
	mt.reconcile(t, mgDeleted(time.Second, mgDead(mgS3, "r3")))
	state = mt.handler.loadSentinelUnknown(key)
	assert.NotContains(t, state.resets, mgForeign)
	assert.Contains(t, state.resets, mgS3)

	// A pod gets the reset IP.
	mt.reconcile(t, mgHealthy(time.Second))
	assert.Empty(t, mt.handler.loadSentinelUnknown(key).resets)
}
