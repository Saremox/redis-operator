package redisfailover

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// noUnknownSentinelAddrs makes mc answer that the Sentinels know only pods of
// the RedisFailover.
func noUnknownSentinelAddrs(mc *mRFService.RedisFailoverCheck) *mRFService.RedisFailoverCheck {
	mc.On("GetUnknownSentinelAddrs", mock.Anything).Maybe().Return(rfservice.SentinelAddrs{}, nil)
	return mc
}

// unknownAddrsRecorder keeps the last values of SetSentinelUnknownAddrs.
type unknownAddrsRecorder struct {
	metrics.Recorder
	sets   int
	peers  int
	master bool
}

func (r *unknownAddrsRecorder) SetSentinelUnknownAddrs(_, _ string, peers int, master bool) {
	r.sets++
	r.peers, r.master = peers, master
}

// upRound is one check of the Sentinel addresses: after is the time since
// the check before.
type upRound struct {
	after  time.Duration
	addrs  rfservice.SentinelAddrs
	err    error
	forget bool
	// lines are the log lines, peers and master the metric values.
	lines  []string
	peers  int
	master bool
}

const (
	upForeign = "10.0.9.9"
	upS1      = "10.0.1.1"
	upS2      = "10.0.1.2"
)

func upPeers(sentinels ...string) rfservice.SentinelAddrs {
	return rfservice.SentinelAddrs{Peers: map[string][]string{upForeign: sentinels}}
}

func upMaster(ip string, sentinels ...string) rfservice.SentinelAddrs {
	return rfservice.SentinelAddrs{Masters: map[string][]string{ip: sentinels}}
}

// upOwn is a check that finds a pod with the IP upForeign.
var upOwn = rfservice.SentinelAddrs{Own: map[string]bool{upForeign: true}}

func upLine(level, format, addrs string) string {
	return level + ": " + fmt.Sprintf(format, addrs)
}

var (
	upPeerStart  = upLine("warning", unknownPeerLog.start, upForeign+" (Sentinels 10.0.1.1 10.0.1.2)")
	upPeerLast   = upLine("debug", unknownPeerLog.last, upForeign+" (Sentinels 10.0.1.1 10.0.1.2)")
	upPeerEnd    = upLine("info", unknownPeerLog.end, upForeign)
	upMasterGone = upLine("info", unknownMasterLog.end, upForeign)
	upUnread     = "debug: Sentinels 10.0.1.2 did not give their peers, so no unknown address ends in this check"
)

// newPeersTest returns a handler with a clock and the recorder of the gauges.
func newPeersTest(lines *[]string) (*RedisFailoverHandler, *mRFService.RedisFailoverCheck, *unknownAddrsRecorder, *time.Time) {
	rec := &unknownAddrsRecorder{Recorder: metrics.Dummy}
	check := &mRFService.RedisFailoverCheck{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, check, &mRFService.RedisFailoverHeal{}, &mK8SService.Services{}, rec, levelLogger{lines: lines})
	clock := time.Unix(1000, 0)
	handler.now = func() time.Time { return clock }
	return handler, check, rec, &clock
}

func TestObserveSentinelAddrs(t *testing.T) {
	tests := []struct {
		name   string
		rounds []upRound
	}{
		{
			name:   "own peers only",
			rounds: []upRound{{}, {after: 90 * time.Second}},
		},
		{
			name: "an unknown peer in two checks 90s apart",
			rounds: []upRound{
				{addrs: upPeers(upS2, upS1)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
				{after: 30 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerLast}, peers: 1},
			},
		},
		{
			name: "an unknown peer in one check",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2)},
			},
		},
		{
			name: "an unknown peer in checks less than 90s apart",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 30 * time.Second, addrs: upPeers(upS1, upS2)},
				{after: 59 * time.Second, addrs: upPeers(upS1, upS2)},
				{after: time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
			},
		},
		{
			name: "the unknown peer is gone",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
				{after: 30 * time.Second, lines: []string{upPeerEnd}},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2)},
			},
		},
		{
			name: "an unknown master address",
			rounds: []upRound{
				{addrs: upMaster(upForeign, upS1)},
				{
					after: 90 * time.Second, addrs: upMaster(upForeign, upS1), master: true,
					lines: []string{upLine("warning", unknownMasterLog.start, upForeign+" (Sentinels 10.0.1.1)")},
				},
				{after: 30 * time.Second, lines: []string{upMasterGone}},
			},
		},
		{
			name: "a loopback or unspecified master",
			rounds: []upRound{
				{addrs: rfservice.SentinelAddrs{Masters: map[string][]string{"127.0.0.1": {upS1}, "0.0.0.0": {upS2}, "": {upS1}}}},
				{after: 90 * time.Second, addrs: rfservice.SentinelAddrs{Masters: map[string][]string{"127.0.0.1": {upS1}, "::": {upS2}, "": {upS1}}}},
			},
		},
		{
			name: "the old IP of a deleted pod in the last 5 minutes",
			rounds: []upRound{
				{addrs: upOwn},
				{after: 30 * time.Second, addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2)},
				{after: 179 * time.Second, addrs: rfservice.SentinelAddrs{Peers: map[string][]string{upForeign: {upS1}}, Masters: map[string][]string{upForeign: {upS1}}}},
				// 5 minutes after the last check that found the pod.
				{after: time.Second, addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
			},
		},
		{
			name: "a reported IP that a new pod gets",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
				{after: 30 * time.Second, addrs: upOwn, lines: []string{upPeerEnd}},
			},
		},
		{
			name: "a read error does not end a reported address",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
				{after: 30 * time.Second, addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}, peers: 1},
				{after: 30 * time.Second, lines: []string{upPeerEnd}},
			},
		},
		{
			name: "a read error does not drop a first check",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 45 * time.Second, addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}},
				{after: 45 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
			},
		},
		{
			name: "a read error starts nothing",
			rounds: []upRound{
				{addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}},
				{after: 90 * time.Second, addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}},
			},
		},
		{
			name: "a pod list error changes nothing",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
				{after: 30 * time.Second, err: errors.New("boom"), lines: []string{"debug: The operator did not list the pods, so it does not check the Sentinel peers: boom"}, peers: 1},
				{after: 30 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerLast}, peers: 1},
			},
		},
		{
			name: "Forget drops the first check",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, forget: true, addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
			},
		},
		{
			name: "Forget drops the pod IPs",
			rounds: []upRound{
				{addrs: upOwn},
				{after: 30 * time.Second, forget: true, addrs: upPeers(upS1, upS2)},
				{after: 90 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := newCustomConfigTestRF()
			var lines []string
			handler, check, rec, clock := newPeersTest(&lines)
			for i, round := range test.rounds {
				*clock = clock.Add(round.after)
				if round.forget {
					handler.Forget(failoverKey(rf))
				}
				check.ExpectedCalls = nil
				check.On("GetUnknownSentinelAddrs", rf).Once().Return(round.addrs, round.err)
				lines = nil
				handler.observeSentinelAddrs(rf)
				assert.Equal(t, round.lines, lines, "round %d", i)
				assert.Equal(t, round.peers, rec.peers, "round %d", i)
				assert.Equal(t, round.master, rec.master, "round %d", i)
			}
			check.AssertExpectations(t)
		})
	}
}

// TestObserveSentinelAddrsPrunesThePodIPs keeps an IP only for 5 minutes
// after the last check that found it, so the memory stays bounded.
func TestObserveSentinelAddrsPrunesThePodIPs(t *testing.T) {
	rf := newCustomConfigTestRF()
	handler, check, _, clock := newPeersTest(&[]string{})
	for _, own := range []map[string]bool{{"10.0.0.1": true, "10.0.0.2": true}, {"10.0.0.2": true}} {
		check.ExpectedCalls = nil
		check.On("GetUnknownSentinelAddrs", rf).Once().Return(rfservice.SentinelAddrs{Own: own}, nil)
		handler.observeSentinelAddrs(rf)
		*clock = clock.Add(sentinelOwnIPMemory)
	}
	assert.Equal(t, map[string]time.Time{"10.0.0.2": time.Unix(1000, 0).Add(sentinelOwnIPMemory)}, handler.loadSentinelUnknown(failoverKey(rf)).own)
}

// TestObserveSentinelAddrsSetsNoMetricOnAPodListError keeps the last values
// of the gauges, because the check did not run.
func TestObserveSentinelAddrsSetsNoMetricOnAPodListError(t *testing.T) {
	rf := newCustomConfigTestRF()
	handler, check, rec, _ := newPeersTest(&[]string{})
	check.On("GetUnknownSentinelAddrs", rf).Once().Return(rfservice.SentinelAddrs{}, errors.New("boom"))

	handler.observeSentinelAddrs(rf)

	assert.Zero(t, rec.sets)
}

// reportPeer makes handler report upForeign as an unknown peer.
func reportPeer(t *testing.T, handler *RedisFailoverHandler, check *mRFService.RedisFailoverCheck, clock *time.Time) {
	t.Helper()
	rf := newCustomConfigTestRF()
	for range 2 {
		check.ExpectedCalls = nil
		check.On("GetUnknownSentinelAddrs", rf).Once().Return(rfservice.SentinelAddrs{Peers: map[string][]string{upForeign: {upS1}}, Own: map[string]bool{upS1: true}}, nil)
		handler.observeSentinelAddrs(rf)
		*clock = clock.Add(sentinelUnknownPersist)
	}
}

// TestSkipSentinelAddrsEndsTheReport sets the gauges to 0 in a reconcile
// without the check, and keeps the pod IPs.
func TestSkipSentinelAddrsEndsTheReport(t *testing.T) {
	rf := newCustomConfigTestRF()
	var lines []string
	handler, check, rec, clock := newPeersTest(&lines)
	handler.skipSentinelAddrs(rf)
	assert.Zero(t, rec.sets, "no state")

	reportPeer(t, handler, check, clock)
	assert.Equal(t, 1, rec.peers)
	lines = nil
	handler.skipSentinelAddrs(rf)
	assert.Equal(t, []string{"info: The operator does not check the Sentinels in this reconcile, so the report of the unknown addresses 10.0.9.9 ends"}, lines)
	assert.Zero(t, rec.peers)
	state := handler.loadSentinelUnknown(failoverKey(rf))
	assert.Empty(t, state.peers)
	assert.Contains(t, state.own, upS1)

	sets := rec.sets
	lines = nil
	handler.skipSentinelAddrs(rf)
	assert.Empty(t, lines)
	assert.Equal(t, sets, rec.sets)
}

// TestSkipSentinelAddrsDropsAFirstCheck ends an address that is not reported
// yet without a log line or a gauge change.
func TestSkipSentinelAddrsDropsAFirstCheck(t *testing.T) {
	rf := newCustomConfigTestRF()
	var lines []string
	handler, check, rec, _ := newPeersTest(&lines)
	check.On("GetUnknownSentinelAddrs", rf).Once().Return(upPeers(upS1), nil)
	handler.observeSentinelAddrs(rf)
	lines = nil

	handler.skipSentinelAddrs(rf)

	assert.Empty(t, lines)
	assert.Equal(t, 1, rec.sets)
	assert.Empty(t, handler.loadSentinelUnknown(failoverKey(rf)).peers)
}

// TestSentinelAddrsOnlyInSentinelMode runs the check in Sentinel mode before
// the count of the masters, so two masters do not stop it. Bootstrap mode
// monitors an external master, so it has no such check. Each reconcile ends
// a reported address: the check does not find it, or the reconcile skips the
// check.
func TestSentinelAddrsOnlyInSentinelMode(t *testing.T) {
	tests := []struct {
		name  string
		s     smReconcile
		check bool
	}{
		{name: "Sentinel mode", s: smReconcile{nMasters: 1, master: smPod1, named: []string{smPod1, smPod1, smPod1}}, check: true},
		{name: "two masters", s: smReconcile{nMasters: 2, master: smPod1}, check: true},
		{name: "no Sentinel quorum", s: smReconcile{nMasters: 1, master: smPod1, noSentinelQuorum: true}},
		{name: "operator-managed mode", s: smReconcile{mode: "operator"}},
		{name: "bootstrap mode", s: smReconcile{mode: "bootstrap"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newSentinelMasterTest()
			var lines []string
			handler, check, rec, clock := newPeersTest(&lines)
			reportPeer(t, handler, check, clock)
			st.handler.sentinelUnknown.Store("testns/test", handler.loadSentinelUnknown("testns/test"))
			st.handler.mClient = rec
			sets := rec.sets

			st.run(t, test.s)

			called := false
			for _, call := range st.check.Calls {
				called = called || call.Method == "GetUnknownSentinelAddrs"
			}
			assert.Equal(t, test.check, called)
			assert.Equal(t, sets+1, rec.sets)
			assert.Zero(t, rec.peers)
			assert.Empty(t, st.handler.loadSentinelUnknown("testns/test").peers)
		})
	}
}
