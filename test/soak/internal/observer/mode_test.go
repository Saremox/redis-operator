package observer

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
)

const bootstrapHost = "10.96.0.5"

// bootstrapped is a converged three-pod instance that replicates from
// bootstrapHost, which is at offset 1000.
func bootstrapped() snapshot {
	s := healthy(false)
	s.bootstrap = &redisfailoverv1.BootstrapSettings{Host: bootstrapHost, Port: "6379"}
	s.endpoints = nil
	for i, offset := range []string{"1000", "990", "400"} {
		s.redis[i].info = replicationInfo("slave", bootstrapHost, "up", offset)
	}
	s.sourceOffset = 1000
	return s
}

func TestEvaluateBootstrap(t *testing.T) {
	cases := []struct {
		name   string
		change func(*snapshot)
		want   []string
	}{
		{"healthy", func(*snapshot) {}, nil},
		{"link down", func(s *snapshot) {
			s.redis[1].info["master_link_status"] = "down"
		}, []string{invOneMaster}},
		{"promoted", func(s *snapshot) {
			s.redis[0].info = replicationInfo("master", "", "", "1000")
		}, []string{invOneMaster}},
		{"replicates from elsewhere", func(s *snapshot) {
			s.redis[2].info["master_host"] = "10.0.0.10"
		}, []string{invOneMaster}},
		{"wrong port", func(s *snapshot) {
			s.redis[2].info["master_port"] = "6380"
		}, []string{invOneMaster}},
		{"unreachable", func(s *snapshot) {
			s.redis[0].info, s.redis[0].err = nil, errors.New("connection refused")
		}, []string{invOneMaster}},
		// The operator labels no pod master while bootstrapping.
		{"labelled master", func(s *snapshot) {
			s.endpoints = []string{"10.0.0.10"}
		}, []string{invMasterService}},
		{"pod missing", func(s *snapshot) {
			s.redis = s.redis[:2]
		}, []string{invPods}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := bootstrapped()
			c.change(&s)
			checks := evaluate(s)
			if v := violated(checks); !slices.Equal(v, c.want) {
				t.Errorf("violated %v, want %v", v, c.want)
			}
			for _, ch := range checks {
				if ch.invariant == invReplication || ch.invariant == invSentinelAgreement {
					t.Errorf("%s evaluated while bootstrapping", ch.invariant)
				}
			}
		})
	}
	lags := bootstrapped().bootstrapLags()
	if want := map[string]int64{"rfr-x-0": 0, "rfr-x-1": 10, "rfr-x-2": 600}; !mapsEqual(lags, want) {
		t.Errorf("lags %v, want %v", lags, want)
	}
}

func mapsEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestApplyBootstrap(t *testing.T) {
	o, reg := newTestObserver(t)
	o.apply(at(0), bootstrapped(), 1, false)
	o.apply(at(5), bootstrapped(), 1, false)
	if !o.Quiet() {
		t.Error("not quiet")
	}
	if v := testutil.ToFloat64(o.masters); v != 0 {
		t.Errorf("masters = %v", v)
	}
	want := `
# HELP redis_soak_replication_lag_bytes The master's replication offset minus the replica's.
# TYPE redis_soak_replication_lag_bytes gauge
redis_soak_replication_lag_bytes{mode="sentinel",namespace="ns",pod="rfr-x-0",rf="x"} 0
redis_soak_replication_lag_bytes{mode="sentinel",namespace="ns",pod="rfr-x-1",rf="x"} 10
redis_soak_replication_lag_bytes{mode="sentinel",namespace="ns",pod="rfr-x-2",rf="x"} 600
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "redis_soak_replication_lag_bytes"); err != nil {
		t.Error(err)
	}
	if got := len(o.RedisAddrs()); got != 3 {
		t.Errorf("%d redis addresses", got)
	}
	// Without the source's offset there is no lag to tell.
	s := bootstrapped()
	s.sourceErr = errNoMaster
	o.apply(at(10), s, 1, false)
	if n := testutil.CollectAndCount(reg, "redis_soak_replication_lag_bytes"); n != 0 {
		t.Errorf("%d lag series without the source's offset", n)
	}
}

// TestSourceWindow checks that a bootstrapping instance's link going down
// while its source fails over is no finding.
func TestSourceWindow(t *testing.T) {
	src, _ := newTestObserver(t)
	src.apply(at(0), healthy(false), 1, false)
	src.apply(at(5), healthy(false), 1, false)
	o, _ := newTestObserver(t)
	o.SetSource(src)
	o.apply(at(0), bootstrapped(), 1, false)
	o.apply(at(5), bootstrapped(), 1, false)

	// The source's master is killed.
	noMaster := healthy(false)
	noMaster.redis[0].info, noMaster.redis[0].err = nil, errors.New("connection refused")
	src.apply(at(10), noMaster, 1, false)
	down := bootstrapped()
	down.redis[1].info["master_link_status"] = "down"
	o.apply(at(10), down, 1, false)
	if v := testutil.ToFloat64(o.findings.WithLabelValues(invOneMaster)); v != 0 {
		t.Errorf("findings = %v while the source failed over", v)
	}
	src.apply(at(15), healthy(false), 1, false)
	o.apply(at(15), bootstrapped(), 1, false)
	if !o.Quiet() {
		t.Error("not quiet after recovering")
	}
	// Without the source converging, the same is a finding.
	o.apply(at(20), down, 1, false)
	if v := testutil.ToFloat64(o.findings.WithLabelValues(invOneMaster)); v != 1 {
		t.Errorf("findings = %v, want 1", v)
	}
}

func TestOperatorDownWindow(t *testing.T) {
	o, _ := newTestObserver(t)
	lock := &global.Lock{}
	o.lock = lock
	o.apply(at(0), healthy(true), 1, false)
	o.apply(at(5), healthy(true), 1, false)
	lock.SetOperatorDown(true)
	s := healthy(true)
	s.state = redisfailoverv1.NotHealthyState
	// Longer than the convergence timeout: the window is restarted while
	// the operator is down.
	for i := 2; i < 200; i++ {
		o.apply(at(5*i), s, 1, false)
	}
	lock.SetOperatorDown(false)
	o.apply(at(1000), s, 1, false)
	o.apply(at(1005), healthy(true), 1, false)
	if v := testutil.ToFloat64(o.findings.WithLabelValues(invHealthy)); v != 0 {
		t.Errorf("findings = %v while the operator was down", v)
	}
	if !o.Quiet() {
		t.Error("not quiet after the operator came back")
	}
}

// TestSentinelSwitchedOff checks that sentinel_agreement and its violation
// are forgotten when Sentinel is switched off, and that the Sentinel path
// follows the mode.
func TestSentinelSwitchedOff(t *testing.T) {
	o, reg := newTestObserver(t)
	o.apply(at(0), healthy(true), 1, false)
	if !o.SentinelPath() {
		t.Error("no Sentinel path while the Sentinels agree")
	}
	s := healthy(true)
	s.sentinels[0].err = errors.New("connection refused")
	o.apply(at(5), s, 2, false)
	if _, violated := o.tracker.violated[invSentinelAgreement]; !violated {
		t.Fatal("sentinel_agreement not violated")
	}
	o.apply(at(10), healthy(false), 3, false)
	if o.SentinelPath() {
		t.Error("Sentinel path without Sentinel")
	}
	if _, ok := o.tracker.violated[invSentinelAgreement]; ok {
		t.Error("sentinel_agreement still tracked")
	}
	if hasSeries(t, reg, "redis_soak_invariant_ok", "invariant", invSentinelAgreement) {
		t.Error("sentinel_agreement series left")
	}
	// The window closes, and no finding is left behind.
	o.apply(at(15), healthy(false), 3, false)
	o.apply(at(1000), healthy(false), 3, false)
	if v := testutil.ToFloat64(o.findings.WithLabelValues(invSentinelAgreement)); v != 0 {
		t.Errorf("findings = %v", v)
	}
	// Switched on again, the path waits for the Sentinels to agree, and
	// to be Ready, as the Sentinel Service routes to Ready pods only.
	s = healthy(true)
	s.sentinels[0].master = "10.0.0.11:6379"
	o.apply(at(1005), s, 4, false)
	if o.SentinelPath() {
		t.Error("Sentinel path before the Sentinels agree")
	}
	s = healthy(true)
	s.sentinels[2].Ready = false
	o.apply(at(1007), s, 4, false)
	if o.SentinelPath() {
		t.Error("Sentinel path before the Sentinels are Ready")
	}
	o.apply(at(1010), healthy(true), 4, false)
	if !o.SentinelPath() {
		t.Error("no Sentinel path after the Sentinels agree")
	}
}

func hasSeries(t *testing.T, reg *prometheus.Registry, name, label, value string) bool {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return true
				}
			}
		}
	}
	return false
}

func TestResetEvent(t *testing.T) {
	o, _ := newTestObserver(t)
	single := func(uid, rfUID string, pvc bool) snapshot {
		s := healthy(false)
		s.redis = s.redis[:1]
		s.redisReplicas = 1
		s.endpoints = []string{"10.0.0.10"}
		s.redis[0].UID = uid
		s.uid, s.pvc = rfUID, pvc
		return s
	}
	o.apply(at(0), single("a", "rf1", false), 1, false)
	o.apply(at(5), single("b", "rf1", false), 1, false)
	if ev := <-o.Failovers(); ev != config.EventReset {
		t.Errorf("only pod without a volume replaced: %s", ev)
	}
	o.apply(at(10), single("c", "rf1", true), 1, false)
	if ev := <-o.Failovers(); ev != config.EventFailover {
		t.Errorf("only pod with a volume replaced: %s", ev)
	}
	s := healthy(false)
	s.uid, s.pvc = "rf1", true
	o.apply(at(15), s, 1, false)
	<-o.Failovers()
	// Recreated: the RedisFailover's UID changes before the new master is
	// up.
	s.uid = "rf2"
	s.redis[0].info, s.redis[0].err = nil, errors.New("connection refused")
	o.apply(at(20), s, 1, false)
	s = healthy(false)
	s.uid, s.pvc = "rf2", true
	s.redis[0].UID = "new"
	o.apply(at(25), s, 1, false)
	if ev := <-o.Failovers(); ev != config.EventReset {
		t.Errorf("recreated: %s", ev)
	}
}
