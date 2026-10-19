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
	// lines are the log lines, peers and master the metric values. kept is
	// true when the handler keeps a state after the check.
	lines  []string
	peers  int
	master bool
	kept   bool
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

func TestObserveSentinelAddrs(t *testing.T) {
	tests := []struct {
		name   string
		rounds []upRound
	}{
		{
			name:   "own peers only",
			rounds: []upRound{{}, {after: 10 * time.Second}},
		},
		{
			name: "an unknown peer in two checks 10s apart",
			rounds: []upRound{
				{addrs: upPeers(upS2, upS1), kept: true},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1, kept: true},
				{after: 30 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerLast}, peers: 1, kept: true},
			},
		},
		{
			name: "an unknown peer in one check",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2), kept: true},
				{after: 10 * time.Second},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), kept: true},
			},
		},
		{
			name: "an unknown peer in two checks less than 10s apart",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2), kept: true},
				{after: 9 * time.Second, addrs: upPeers(upS1, upS2), kept: true},
				{after: time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1, kept: true},
			},
		},
		{
			name: "the unknown peer is gone",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2), kept: true},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1, kept: true},
				{after: 10 * time.Second, lines: []string{upPeerEnd}},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), kept: true},
			},
		},
		{
			name: "an unknown master address",
			rounds: []upRound{
				{addrs: upMaster(upForeign, upS1), kept: true},
				{
					after: 10 * time.Second, addrs: upMaster(upForeign, upS1), master: true, kept: true,
					lines: []string{upLine("warning", unknownMasterLog.start, upForeign+" (Sentinels 10.0.1.1)")},
				},
				{after: 10 * time.Second, lines: []string{upMasterGone}},
			},
		},
		{
			name: "a loopback or unspecified master",
			rounds: []upRound{
				{addrs: rfservice.SentinelAddrs{Masters: map[string][]string{"127.0.0.1": {upS1}, "0.0.0.0": {upS2}, "": {upS1}}}},
				{after: 10 * time.Second, addrs: rfservice.SentinelAddrs{Masters: map[string][]string{"127.0.0.1": {upS1}, "::": {upS2}, "": {upS1}}}},
			},
		},
		{
			name: "a read error does not end a reported address",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2), kept: true},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1, kept: true},
				{after: 10 * time.Second, addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}, peers: 1, kept: true},
				{after: 10 * time.Second, lines: []string{upPeerEnd}},
			},
		},
		{
			name: "a read error does not drop a first check",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2), kept: true},
				{after: 5 * time.Second, addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}, kept: true},
				{after: 5 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1, kept: true},
			},
		},
		{
			name: "a read error starts nothing",
			rounds: []upRound{
				{addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}},
				{after: 10 * time.Second, addrs: rfservice.SentinelAddrs{Unread: []string{upS2}}, lines: []string{upUnread}},
			},
		},
		{
			name: "a pod list error changes nothing",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2), kept: true},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1, kept: true},
				{after: 10 * time.Second, err: errors.New("boom"), lines: []string{"debug: The operator did not list the pods, so it does not check the Sentinel peers: boom"}, peers: 1, kept: true},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerLast}, peers: 1, kept: true},
			},
		},
		{
			name: "Forget drops the first check",
			rounds: []upRound{
				{addrs: upPeers(upS1, upS2), kept: true},
				{after: 10 * time.Second, forget: true, addrs: upPeers(upS1, upS2), kept: true},
				{after: 10 * time.Second, addrs: upPeers(upS1, upS2), lines: []string{upPeerStart}, peers: 1, kept: true},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := newCustomConfigTestRF()
			var lines []string
			rec := &unknownAddrsRecorder{Recorder: metrics.Dummy}
			check := &mRFService.RedisFailoverCheck{}
			handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, check, &mRFService.RedisFailoverHeal{}, &mK8SService.Services{}, rec, levelLogger{lines: &lines})
			clock := time.Unix(1000, 0)
			handler.now = func() time.Time { return clock }
			for i, round := range test.rounds {
				clock = clock.Add(round.after)
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
				_, kept := handler.sentinelUnknown.Load(failoverKey(rf))
				assert.Equal(t, round.kept, kept, "round %d", i)
			}
			check.AssertExpectations(t)
		})
	}
}

// TestObserveSentinelAddrsSetsNoMetricOnAPodListError keeps the last values
// of the gauges, because the check did not run.
func TestObserveSentinelAddrsSetsNoMetricOnAPodListError(t *testing.T) {
	rf := newCustomConfigTestRF()
	rec := &unknownAddrsRecorder{Recorder: metrics.Dummy}
	check := &mRFService.RedisFailoverCheck{}
	check.On("GetUnknownSentinelAddrs", rf).Once().Return(rfservice.SentinelAddrs{}, errors.New("boom"))
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, check, &mRFService.RedisFailoverHeal{}, &mK8SService.Services{}, rec, levelLogger{lines: &[]string{}})

	handler.observeSentinelAddrs(rf)

	assert.Zero(t, rec.sets)
}

// TestSentinelAddrsOnlyInSentinelMode runs the check in Sentinel mode before
// the count of the masters, so two masters do not stop it. Bootstrap mode
// monitors an external master, so it has no such check.
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
			st.run(t, test.s)
			called := false
			for _, call := range st.check.Calls {
				called = called || call.Method == "GetUnknownSentinelAddrs"
			}
			assert.Equal(t, test.check, called)
		})
	}
}
