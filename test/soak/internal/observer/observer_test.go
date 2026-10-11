package observer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

func newTestObserver(t *testing.T) (*Observer, *prometheus.Registry) {
	t.Helper()
	cfg, err := config.Parse([]byte("instances: [{name: x, namespace: ns, mode: sentinel}]"))
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	o := New(cfg.Instances[0], cfg, nil, nil, nil, nil, metrics.New(reg, time.Minute), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return o, reg
}

func TestApply(t *testing.T) {
	o, reg := newTestObserver(t)
	labels := `mode="sentinel",namespace="ns",rf="x"`

	s := healthy(true)
	o.apply(at(0), s, 1, false)
	o.apply(at(5), s, 1, false)

	// rfr-x-0 is killed: rfr-x-1 takes over, and the replacement for
	// rfr-x-0 isn't up yet.
	s = healthy(true)
	s.redis = s.redis[1:]
	s.redis[0].info = replicationInfo("master", "", "", "2000")
	s.redis[1].info["master_host"] = "10.0.0.11"
	s.redis[1].info["slave_repl_offset"] = "1500"
	s.endpoints = []string{"10.0.0.11"}
	for i := range s.sentinels {
		s.sentinels[i].master = "10.0.0.11:6379"
	}
	o.apply(at(10), s, 1, false)

	// rfr-x-0 is back as a replica.
	s = healthy(true)
	s.redis[0].UID = "u0-new"
	s.redis[0].info = replicationInfo("slave", "10.0.0.11", "up", "2000")
	s.redis[1].info = replicationInfo("master", "", "", "2000")
	s.redis[2].info = replicationInfo("slave", "10.0.0.11", "up", "2000")
	s.endpoints = []string{"10.0.0.11"}
	for i := range s.sentinels {
		s.sentinels[i].master = "10.0.0.11:6379"
	}
	o.apply(at(30), s, 1, false)

	want := `
# HELP redis_soak_failovers_total Changes of the master's identity.
# TYPE redis_soak_failovers_total counter
redis_soak_failovers_total{` + labels + `} 1
# HELP redis_soak_findings_total Invariant violations outside a convergence window.
# TYPE redis_soak_findings_total counter
redis_soak_findings_total{invariant="healthy",` + labels + `} 0
redis_soak_findings_total{invariant="master_service",` + labels + `} 0
redis_soak_findings_total{invariant="one_master",` + labels + `} 0
redis_soak_findings_total{invariant="oom_killed",` + labels + `} 0
redis_soak_findings_total{invariant="pods",` + labels + `} 1
redis_soak_findings_total{invariant="replica_ready_without_data",` + labels + `} 0
redis_soak_findings_total{invariant="replication",` + labels + `} 0
redis_soak_findings_total{invariant="sentinel_agreement",` + labels + `} 0
# HELP redis_soak_masters Redis pods reporting the master role. 1 is right.
# TYPE redis_soak_masters gauge
redis_soak_masters{` + labels + `} 1
# HELP redis_soak_replication_lag_bytes The master's replication offset minus the replica's.
# TYPE redis_soak_replication_lag_bytes gauge
redis_soak_replication_lag_bytes{mode="sentinel",namespace="ns",pod="rfr-x-0",rf="x"} 0
redis_soak_replication_lag_bytes{mode="sentinel",namespace="ns",pod="rfr-x-2",rf="x"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"redis_soak_failovers_total", "redis_soak_findings_total", "redis_soak_masters",
		"redis_soak_replication_lag_bytes"); err != nil {
		t.Error(err)
	}
	for _, inv := range []string{invPods, invOneMaster, invMasterService, invReplication, invSentinelAgreement, invHealthy} {
		if v := testutil.ToFloat64(o.ok.WithLabelValues(inv)); v != 1 {
			t.Errorf("invariant_ok{%s} = %v", inv, v)
		}
	}
	if n := testutil.CollectAndCount(o.violation.(prometheus.Collector), "redis_soak_invariant_violation_seconds"); n != 1 {
		t.Errorf("%d violation series, want only pods", n)
	}
}

func TestApplyLagWhileFailingOver(t *testing.T) {
	o, reg := newTestObserver(t)
	o.apply(at(0), healthy(true), 1, false)
	if n := testutil.CollectAndCount(reg, "redis_soak_replication_lag_bytes"); n != 2 {
		t.Errorf("%d lag series, want 2", n)
	}
	s := healthy(true)
	s.redis[0].info = nil
	o.apply(at(5), s, 1, false)
	if n := testutil.CollectAndCount(reg, "redis_soak_replication_lag_bytes"); n != 0 {
		t.Errorf("%d lag series without a master, want 0", n)
	}
	if v := testutil.ToFloat64(o.masters); v != 0 {
		t.Errorf("masters = %v", v)
	}
}

func TestGenerationOpensWindow(t *testing.T) {
	o, _ := newTestObserver(t)
	// The first observation opens a window: a change may be in flight.
	s := healthy(true)
	s.redis[2].Ready = false
	o.apply(at(0), s, 1, false)
	o.apply(at(5), healthy(true), 1, false)
	if o.tracker.windowOpen() {
		t.Error("window still open after convergence")
	}
	o.apply(at(10), s, 2, false)
	o.apply(at(15), s, 2, false)
	if v := testutil.ToFloat64(o.findings.WithLabelValues(invPods)); v != 0 {
		t.Errorf("findings = %v, want 0", v)
	}
	// Not converged, but within the timeout.
	if !o.tracker.windowOpen() {
		t.Error("window closed early")
	}
	o.apply(at(20), healthy(true), 2, false)
	o.apply(at(25), s, 2, false)
	if v := testutil.ToFloat64(o.findings.WithLabelValues(invPods)); v != 1 {
		t.Errorf("findings = %v, want 1", v)
	}
}

func TestHold(t *testing.T) {
	o, _ := newTestObserver(t)
	ctx := context.Background()
	step := func(now time.Time, s snapshot) {
		o.takeHold()
		o.apply(now, s, 1, o.converged(ctx))
	}
	step(at(0), healthy(true))
	if !o.Quiet() || o.Master() != "rfr-x-0" {
		t.Fatalf("quiet %v, master %q", o.Quiet(), o.Master())
	}

	var converged atomic.Bool
	h := o.Hold(time.Minute, func(context.Context) error {
		if converged.Load() {
			return nil
		}
		return errors.New("not yet")
	})
	h.Applied()
	result := h.Done()
	// The killed master's replacement isn't there yet.
	s := healthy(true)
	s.redis = s.redis[1:]
	s.redis[0].info = replicationInfo("master", "", "", "2000")
	s.redis[1].info["master_host"] = "10.0.0.11"
	s.endpoints = []string{"10.0.0.11"}
	for i := range s.sentinels {
		s.sentinels[i].master = "10.0.0.11:6379"
	}
	step(time.Now(), s)
	if o.Quiet() {
		t.Error("quiet while held")
	}
	converged.Store(true)
	step(time.Now().Add(5*time.Second), healthy(true))
	select {
	case <-result:
		t.Fatal("window closed before the dwell")
	default:
	}
	step(time.Now().Add(20*time.Second), healthy(true))
	if ok := <-result; !ok {
		t.Error("window timed out")
	}
	if v := testutil.ToFloat64(o.findings.WithLabelValues(invPods)); v != 0 {
		t.Errorf("findings = %v, want 0", v)
	}
	if !o.Quiet() {
		t.Error("not quiet after the window closed")
	}
}
