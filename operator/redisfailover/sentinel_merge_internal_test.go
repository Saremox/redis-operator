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
	pendingIP bool
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
	addrs := rfservice.SentinelAddrs{Peers: map[string][]string{}, Masters: map[string][]string{}, Own: map[string]bool{mgMaster: true, mgReplica: true}, Groups: r.groups, Unread: r.unread, PendingIP: r.pendingIP}
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

var errMgWrongMaster = errors.New("sentinel monitoring 10.0.9.8:6379 instead 10.0.0.1:6379")

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

const (
	mgMergeStart  = "warning: Sentinels of this RedisFailover know addresses of another group: peer 10.0.9.9 (Sentinels 10.0.1.1). The operator sends no SENTINEL MONITOR, REMOVE or RESET to the Sentinels until these addresses are gone, because the other group undoes each change in seconds"
	mgMergeEnd    = "info: The addresses of another group are gone from the Sentinels: peer 10.0.9.9 (Sentinels 10.0.1.1). The operator heals the Sentinels again"
	mgMergeStatus = "Sentinels know the addresses 10.0.9.9 of another group, so the operator sends them no SENTINEL MONITOR or RESET"
)

// TestForeignPeerBackAfterAReset covers a Sentinel of another group that
// sends hellos. A reset removes it for 2 seconds only. The operator resets
// one time, and then sends no SENTINEL MONITOR or RESET to the Sentinels
// until the peer stops. The Redis side does not depend on this check.
func TestForeignPeerBackAfterAReset(t *testing.T) {
	mt := newMergeTest()
	got := mt.reconcile(t, mgForeignRound(0))
	assert.Equal(t, []string{mgS1}, got.reset, "a reset finds out whether the peer comes back")
	assert.Empty(t, got.monitored)

	// The peer is back. The other group moved mgS2 to its master, and the
	// count of mgS3 is wrong.
	round := mgForeignRound(30 * time.Second)
	round.monitor = map[string]error{mgS2: errMgWrongMaster}
	round.count = map[string]error{mgS3: errors.New("sentinels in memory mismatch")}
	got = mt.reconcile(t, round)
	require.NoError(t, got.err)
	assert.Empty(t, got.reset)
	assert.Empty(t, got.monitored)
	assert.Equal(t, []string{mgMergeStart}, got.warnings())
	assert.Equal(t, redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: mgMergeStatus}, got.status)

	// No Warning in the next reconciles, and still no change.
	got = mt.reconcile(t, round)
	assert.Empty(t, got.reset)
	assert.Empty(t, got.monitored)
	assert.Empty(t, got.warnings())
	assert.Contains(t, got.lines, "debug: Sentinels still know addresses of another group: peer 10.0.9.9 (Sentinels 10.0.1.1)")
	assert.Contains(t, got.lines, "debug: Sentinel 10.0.1.3 mismatch number of sentinels in memory. The Sentinels know another group, so the operator does not reset it")
	assert.Contains(t, got.lines, "debug: The Sentinels know another group, so the operator does not change the master of Sentinel 10.0.1.2")
	assert.Equal(t, mgMergeStatus, got.status.Message)

	// The Sentinel of the other group stopped: the guard ends, and the heal
	// resets mgS1 and monitors mgS2 again.
	round.after = 30 * time.Second
	round.groups[mgS1] = mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3"), mgDead(mgForeign, "rf"))
	got = mt.reconcile(t, round)
	assert.Equal(t, []string{mgS1, mgS3}, got.reset)
	assert.Equal(t, []string{mgS2}, got.monitored)
	assert.Equal(t, mgMergeEnd, got.warnings()[0])
	assert.Equal(t, redisfailoverv1.HealthyState, got.status.State)
}

// TestForeignPeerGuardEndsWhenThePeerIsGone ends the guard when no Sentinel
// knows the peer, for example after the Sentinels of the other group are
// replaced.
func TestForeignPeerGuardEndsWhenThePeerIsGone(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgForeignRound(0))
	require.Equal(t, []string{mgMergeStart}, mt.reconcile(t, mgForeignRound(30*time.Second)).warnings())

	round := mgHealthy(30 * time.Second)
	round.monitor = map[string]error{mgS2: errMgWrongMaster}
	got := mt.reconcile(t, round)

	assert.Equal(t, []string{mgMergeEnd}, got.warnings()[:1])
	assert.Equal(t, []string{mgS2}, got.monitored)
	assert.Equal(t, redisfailoverv1.HealthyState, got.status.State)
}

// TestForeignPeerGuardNeedsACompleteCheck keeps the guard while a Sentinel
// does not answer, because that Sentinel can still know the peer.
func TestForeignPeerGuardNeedsACompleteCheck(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgForeignRound(0))
	mt.reconcile(t, mgForeignRound(30*time.Second))

	round := mgHealthy(30 * time.Second)
	delete(round.groups, mgS1)
	round.unread = []string{mgS1}
	round.monitor = map[string]error{mgS2: errMgWrongMaster}
	got := mt.reconcile(t, round)

	assert.Empty(t, got.monitored)
	assert.Empty(t, got.warnings())
	assert.Equal(t, mgMergeStatus, got.status.Message)
}

// TestForeignPeerGuardMonitorsAnUnsetSentinel gives a new monitor to a
// Sentinel without a master also during the guard. Without it, such a
// Sentinel gives 127.0.0.1 to the clients. A Sentinel that knows an alive
// peer of another group gets no monitor.
func TestForeignPeerGuardMonitorsAnUnsetSentinel(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgForeignRound(0))

	round := mgForeignRound(30 * time.Second)
	round.pods = []string{mgS1, mgS2, mgS3, mgS4}
	round.sentinels = []string{mgS1, mgS2, mgS3, mgS4}
	round.groups[mgS3] = redis.SentinelPeers{Master: "127.0.0.1"}
	round.groups[mgS4] = redis.SentinelPeers{Master: "127.0.0.1", Peers: []redis.SentinelPeer{mgAlive(mgForeign, "rf")}}
	round.monitor = map[string]error{
		mgS2: errMgWrongMaster,
		mgS3: errors.New("sentinel monitoring 127.0.0.1:6379 instead 10.0.0.1:6379"),
		mgS4: errors.New("sentinel monitoring 127.0.0.1:6379 instead 10.0.0.1:6379"),
	}
	got := mt.reconcile(t, round)

	// mgS4 got no reset, so the peer counts only for mgS1.
	assert.Equal(t, []string{mgMergeStart, "warning: Fixing sentinel not monitoring expected master: sentinel monitoring 127.0.0.1:6379 instead 10.0.0.1:6379"}, got.warnings())
	assert.Equal(t, []string{mgS3}, got.monitored)
	assert.Empty(t, got.reset)
}

// TestForeignPeerGuardMonitorsASentinelWithoutTheMaster covers a Sentinel
// after SENTINEL REMOVE. It knows no peer, so it gets the master.
func TestForeignPeerGuardMonitorsASentinelWithoutTheMaster(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgForeignRound(0))

	round := mgForeignRound(30 * time.Second)
	delete(round.groups, mgS3)
	round.unread = []string{mgS3}
	round.monitor = map[string]error{mgS2: errMgWrongMaster, mgS3: errors.New("ERR No such master with that name")}
	got := mt.reconcile(t, round)

	assert.Equal(t, []string{mgS3}, got.monitored)
}

// TestUnsetSentinelMonitorWithoutAGuard keeps the monitor of a new Sentinel
// at 127.0.0.1 without another group.
func TestUnsetSentinelMonitorWithoutAGuard(t *testing.T) {
	mt := newMergeTest()
	round := mgHealthy(0)
	round.groups[mgS3] = redis.SentinelPeers{Master: "127.0.0.1"}
	round.monitor = map[string]error{mgS3: errors.New("sentinel monitoring 127.0.0.1:6379 instead 10.0.0.1:6379")}

	got := mt.reconcile(t, round)

	assert.Equal(t, []string{mgS3}, got.monitored)
	assert.Empty(t, got.reset)
	assert.Empty(t, mt.handler.loadSentinelUnknown(failoverKey(mt.rf)).merged)
}

// TestForeignMasterNeedsAnAlivePeer stops the heal for a master of another
// group only together with an alive unknown peer. Without such a peer, for
// example after the Sentinel followed a reused IP of an old master, a new
// monitor ends the state. A master that does not answer is no reason either.
func TestForeignMasterNeedsAnAlivePeer(t *testing.T) {
	foreignMaster := func(peers ...redis.SentinelPeer) mgRound {
		round := mgHealthy(0)
		round.groups[mgS2] = redis.SentinelPeers{Master: mgForeignMaster, Peers: append([]redis.SentinelPeer{mgAlive(mgS1, "r1"), mgAlive(mgS3, "r3")}, peers...)}
		round.monitor = map[string]error{mgS2: errMgWrongMaster}
		return round
	}
	t.Run("no unknown peer", func(t *testing.T) {
		mt := newMergeTest()
		got := mt.reconcile(t, foreignMaster())
		assert.Equal(t, []string{mgS2}, got.monitored)
		assert.Equal(t, redisfailoverv1.HealthyState, got.status.State)
	})
	t.Run("a dead unknown peer", func(t *testing.T) {
		mt := newMergeTest()
		got := mt.reconcile(t, foreignMaster(mgDead(mgForeign, "rf")))
		assert.Equal(t, []string{mgS2}, got.monitored)
	})
	t.Run("an alive unknown peer", func(t *testing.T) {
		mt := newMergeTest()
		got := mt.reconcile(t, foreignMaster(mgAlive(mgForeign, "rf")))
		assert.Empty(t, got.monitored)
		assert.Empty(t, got.reset)
		assert.Equal(t, []string{"warning: Sentinels of this RedisFailover know addresses of another group: master 10.0.9.8 (Sentinels 10.0.1.2). The operator sends no SENTINEL MONITOR, REMOVE or RESET to the Sentinels until these addresses are gone, because the other group undoes each change in seconds"}, got.warnings())
		assert.Equal(t, "Sentinels know the addresses 10.0.9.8 of another group, so the operator sends them no SENTINEL MONITOR or RESET", got.status.Message)
	})
	t.Run("a master that does not answer", func(t *testing.T) {
		mt := newMergeTest()
		round := foreignMaster(mgAlive(mgForeign, "rf"))
		group := round.groups[mgS2]
		group.MasterDown = true
		round.groups[mgS2] = group
		got := mt.reconcile(t, round)
		assert.Equal(t, []string{mgS2}, got.monitored)
	})
}

// mgNoReport fails the test for a log line of a merge or an unknown peer.
func mgNoReport(t *testing.T, got mgResult) {
	t.Helper()
	assert.NoError(t, got.err)
	for _, line := range got.warnings() {
		assert.NotContains(t, line, "another group")
		assert.NotContains(t, line, "unknown")
	}
	assert.NotEqual(t, redisfailoverv1.NotHealthyState, got.status.State, got.status.Message)
}

// TestNoMergeReportInARollingUpdate replaces the Sentinel pods one at a time.
// The old pod is Terminating first, and then gone while the others still
// know it. The new pod first monitors 127.0.0.1.
func TestNoMergeReportInARollingUpdate(t *testing.T) {
	mt := newMergeTest()
	mgNoReport(t, mt.reconcile(t, mgHealthy(0)))

	// mgS3 is Terminating.
	round := mgHealthy(5 * time.Second)
	round.sentinels = []string{mgS1, mgS2}
	mgNoReport(t, mt.reconcile(t, round))

	// mgS3 is gone, and mgS4 starts.
	round = mgDeleted(5*time.Second, mgAlive(mgS3, "r3"))
	round.pods, round.sentinels = []string{mgS1, mgS2, mgS4}, []string{mgS1, mgS2, mgS4}
	round.groups[mgS4] = redis.SentinelPeers{Master: "127.0.0.1"}
	round.monitor = map[string]error{mgS4: errors.New("sentinel monitoring 127.0.0.1:6379 instead 10.0.0.1:6379")}
	got := mt.reconcile(t, round)
	mgNoReport(t, got)
	assert.Equal(t, []string{mgS4}, got.monitored)
	assert.Equal(t, []string{mgS1, mgS2}, got.reset)

	// The Sentinels know each other again.
	round = mgRound{
		after: 30 * time.Second, pods: []string{mgS1, mgS2, mgS4}, sentinels: []string{mgS1, mgS2, mgS4},
		groups: map[string]redis.SentinelPeers{
			mgS1: mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS4, "r4")),
			mgS2: mgGroup(mgAlive(mgS1, "r1"), mgAlive(mgS4, "r4")),
			mgS4: mgGroup(mgAlive(mgS1, "r1"), mgAlive(mgS2, "r2")),
		},
	}
	for range 4 {
		got = mt.reconcile(t, round)
		mgNoReport(t, got)
		assert.Empty(t, got.reset)
	}
}

// TestNoMergeReportWhenANewPodGetsTheOldIP covers a new Sentinel pod that
// gets the IP of the deleted pod after the reset. Its Sentinel joins the
// others with a new run ID before its pod status has the IP. The peer came
// back after the reset and has another run ID, but it is no other group.
func TestNoMergeReportWhenANewPodGetsTheOldIP(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgHealthy(0))
	got := mt.reconcile(t, mgDeleted(5*time.Second, mgDead(mgS3, "r3")))
	require.Equal(t, []string{mgS1, mgS2}, got.reset)

	round := mgDeleted(5*time.Second, mgAlive(mgS3, "r3new"))
	round.pendingIP = true
	mgNoReport(t, mt.reconcile(t, round))

	round = mgHealthy(5 * time.Second)
	round.groups[mgS1] = mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3new"))
	round.groups[mgS2] = mgGroup(mgAlive(mgS1, "r1"), mgAlive(mgS3, "r3new"))
	for range 4 {
		got := mt.reconcile(t, round)
		mgNoReport(t, got)
		assert.Empty(t, got.reset)
		round.after = 30 * time.Second
	}
}

// TestNoMergeReportInAScaleUp adds a Sentinel pod.
func TestNoMergeReportInAScaleUp(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgHealthy(0))

	round := mgHealthy(30 * time.Second)
	round.pods, round.sentinels = []string{mgS1, mgS2, mgS3, mgS4}, []string{mgS1, mgS2, mgS3, mgS4}
	round.groups[mgS4] = redis.SentinelPeers{Master: "127.0.0.1"}
	round.monitor = map[string]error{mgS4: errors.New("sentinel monitoring 127.0.0.1:6379 instead 10.0.0.1:6379")}
	got := mt.reconcile(t, round)
	mgNoReport(t, got)
	assert.Equal(t, []string{mgS4}, got.monitored)
	assert.Empty(t, got.reset)

	round = mgHealthy(30 * time.Second)
	round.pods, round.sentinels = []string{mgS1, mgS2, mgS3, mgS4}, []string{mgS1, mgS2, mgS3, mgS4}
	for _, sip := range []string{mgS1, mgS2, mgS3} {
		group := round.groups[sip]
		group.Peers = append(slices.Clone(group.Peers), mgAlive(mgS4, "r4"))
		round.groups[sip] = group
	}
	round.groups[mgS4] = mgGroup(mgAlive(mgS1, "r1"), mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3"))
	for range 4 {
		got = mt.reconcile(t, round)
		mgNoReport(t, got)
		assert.Empty(t, got.reset)
	}
}

// TestNoMergeReportAfterAnOperatorRestart starts without the memory of the
// pods and the run IDs. The Sentinels still know a pod that was deleted just
// before the restart.
func TestNoMergeReportAfterAnOperatorRestart(t *testing.T) {
	for name, entry := range map[string]redis.SentinelPeer{"alive": mgAlive(mgS3, "r3"), "dead": mgDead(mgS3, "r3")} {
		t.Run(name, func(t *testing.T) {
			mt := newMergeTest()
			got := mt.reconcile(t, mgDeleted(0, entry))
			mgNoReport(t, got)
			assert.Equal(t, []string{mgS1, mgS2}, got.reset)

			round := mgDeleted(30*time.Second, entry)
			round.groups = map[string]redis.SentinelPeers{mgS1: mgGroup(mgAlive(mgS2, "r2")), mgS2: mgGroup(mgAlive(mgS1, "r1"))}
			for range 4 {
				got = mt.reconcile(t, round)
				mgNoReport(t, got)
				assert.Empty(t, got.reset)
			}
		})
	}
}

// TestForeignPeerGuardStaysOnAPodListError keeps the guard and its status
// when the operator cannot list the pods. The heal resets no peer of an old
// check.
func TestForeignPeerGuardStaysOnAPodListError(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgForeignRound(0))
	mt.reconcile(t, mgForeignRound(30*time.Second))

	mt.check.ExpectedCalls, mt.check.Calls = nil, nil
	mt.check.On("GetUnknownSentinelAddrs", mt.rf).Once().Return(rfservice.SentinelAddrs{}, errors.New("boom"))
	mt.rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState}
	mt.handler.observeSentinelAddrs(mt.rf)

	assert.Equal(t, mgMergeStatus, mt.rf.Status.Message)
	state := mt.handler.loadSentinelUnknown(failoverKey(mt.rf))
	assert.NotEmpty(t, state.merged)
	assert.Nil(t, state.stale)
	assert.Nil(t, state.unset)
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

// TestSentinelMergeKeepsTheRedisHeal runs a full reconcile while the
// Sentinels know a Sentinel of another group. The operator still moves the
// replicas to the Redis master, because the clients use the Redis data path.
// It sends no SENTINEL MONITOR, and the status names the address.
func TestSentinelMergeKeepsTheRedisHeal(t *testing.T) {
	st := newSentinelMasterTest()
	// A reset removed the peer from the first Sentinel 30 seconds ago.
	state := st.handler.loadSentinelUnknown("testns/test")
	state.resets[mgForeign] = map[string]time.Time{smSentinels[0]: st.clock.Add(-30 * time.Second)}
	st.handler.sentinelUnknown.Store("testns/test", state)
	addrs := mgRound{pods: smSentinels, groups: map[string]redis.SentinelPeers{
		smSentinels[0]: {Master: smPod1, Peers: []redis.SentinelPeer{mgAlive(smSentinels[1], "r2"), mgAlive(mgForeign, "rf")}},
	}}.addrs()

	res := st.run(t, smReconcile{nMasters: 1, master: smPod1, named: []string{smPod1, smPod1, smPod1}, addrs: &addrs})

	assert.Equal(t, 1, res.replicaOf, "REPLICAOF of the replica on the wrong master")
	assert.Empty(t, res.monitored)
	assert.Equal(t, redisfailoverv1.NotHealthyState, st.rf.Status.State)
	assert.Equal(t, mgMergeStatus, st.rf.Status.Message)
}

// TestAlivePeerSendsHellos decides by the age of the last hello, not by the
// flags. A Sentinel of another group can send hellos while this group cannot
// reach it, and a Sentinel with another master name answers PING but sends no
// hello for this name.
func TestAlivePeerSendsHellos(t *testing.T) {
	mt := newMergeTest()
	mt.reconcile(t, mgForeignRound(0))

	round := mgForeignRound(30 * time.Second)
	round.groups[mgS1] = mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3"), redis.SentinelPeer{IP: mgForeign, RunID: "rf", Down: true, LastHello: time.Second})
	got := mt.reconcile(t, round)
	assert.Equal(t, []string{mgMergeStart}, got.warnings(), "hellos without a link")

	round.groups[mgS1] = mgGroup(mgAlive(mgS2, "r2"), mgAlive(mgS3, "r3"), redis.SentinelPeer{IP: mgForeign, RunID: "rf", LastHello: 30 * time.Second})
	got = mt.reconcile(t, round)
	assert.Equal(t, mgMergeEnd, got.warnings()[0], "a link without hellos")
	assert.Equal(t, []string{mgS1}, got.reset)
}

// TestAlivePeerWithoutTheHelloAge falls back to the flags.
func TestAlivePeerWithoutTheHelloAge(t *testing.T) {
	assert.True(t, alivePeer(redis.SentinelPeer{LastHello: -1}))
	assert.False(t, alivePeer(redis.SentinelPeer{LastHello: -1, Down: true}))
	assert.True(t, alivePeer(redis.SentinelPeer{LastHello: sentinelPeerHelloFresh - time.Millisecond, Down: true}))
	assert.False(t, alivePeer(redis.SentinelPeer{LastHello: sentinelPeerHelloFresh}))
}
