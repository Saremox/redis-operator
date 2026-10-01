package prober

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

func TestProbe(t *testing.T) {
	s := miniredis.RunT(t)
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, time.Minute)
	in := config.Instance{Name: "op-basic", Namespace: "op-basic", Mode: config.ModeOperator, Port: 6379}
	path := Path{Name: PathRFRM, NewClient: func(Client) *redis.Client {
		return redis.NewClient(&redis.Options{Addr: s.Addr(), MaxRetries: -1})
	}}
	probe := config.Probe{Interval: metav1.Duration{Duration: time.Second}, Timeout: metav1.Duration{Duration: time.Second},
		WaitEvery: 2, WaitTimeout: metav1.Duration{Duration: 100 * time.Millisecond}}
	p := New(in, path, Fresh, probe, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	run := func() {
		c := path.NewClient(Fresh)
		defer func() { _ = c.Close() }()
		p.probe(ctx, c)
	}

	run()
	run()
	if v, _ := s.Get("soak:op-basic:rfrm:fresh:seq"); v != "2" {
		t.Errorf("seq = %q, want 2", v)
	}
	s.SetError("READONLY You can't write against a read only replica.")
	run()
	run()
	s.SetError("")
	run()

	labels := `{client="fresh",mode="operator",namespace="op-basic",path="rfrm",rf="op-basic"}`
	want := `
# HELP redis_soak_writable 1 if the last write probe succeeded.
# TYPE redis_soak_writable gauge
redis_soak_writable` + labels + ` 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "redis_soak_writable"); err != nil {
		t.Error(err)
	}
	for op, result := range map[string]string{"set": "ok", "get": "ok"} {
		c := m.ProbeTotal.WithLabelValues("op-basic", "op-basic", "operator", "rfrm", "fresh", op, result)
		if got := testutil.ToFloat64(c); got != 3 {
			t.Errorf("%s %s = %v, want 3", op, result, got)
		}
		c = m.ProbeTotal.WithLabelValues("op-basic", "op-basic", "operator", "rfrm", "fresh", op, ResultReadOnly)
		if got := testutil.ToFloat64(c); got != 2 {
			t.Errorf("%s readonly = %v, want 2", op, got)
		}
	}
	// WAIT after the 2nd and 4th SET; the 4th failed.
	if got := testutil.ToFloat64(m.ProbeTotal.WithLabelValues("op-basic", "op-basic", "operator", "rfrm", "fresh", "wait", "ok")); got != 1 {
		t.Errorf("wait ok = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.WaitAckedReplicas.WithLabelValues("op-basic", "op-basic", "operator")); got != 0 {
		t.Errorf("wait_acked_replicas = %v, want 0", got)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "redis_soak_outage_duration_seconds" {
			if n := mf.GetMetric()[0].GetHistogram().GetSampleCount(); n != 1 {
				t.Errorf("outages = %d, want 1", n)
			}
		}
	}
}

func TestPaths(t *testing.T) {
	names := func(mode config.Mode) []string {
		var n []string
		for _, p := range Paths(config.Instance{Name: "x", Namespace: "ns", Mode: mode, Port: 6379}, time.Second) {
			n = append(n, p.Name)
		}
		return n
	}
	if got := names(config.ModeOperator); !slices.Equal(got, []string{PathRFRM}) {
		t.Errorf("operator paths = %v", got)
	}
	if got := names(config.ModeSentinel); !slices.Equal(got, []string{PathSentinel, PathRFRM}) {
		t.Errorf("sentinel paths = %v", got)
	}
}
