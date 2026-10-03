package prober

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

var fastProbe = config.Probe{
	Interval: metav1.Duration{Duration: 10 * time.Millisecond}, Timeout: metav1.Duration{Duration: time.Second},
	WaitEvery: 1000, WaitTimeout: metav1.Duration{Duration: 100 * time.Millisecond},
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// series counts the series of a metric with label=value.
func series(t *testing.T, reg *prometheus.Registry, name, label, value string) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					n++
				}
			}
		}
	}
	return n
}

func TestRunnerFollowsPaths(t *testing.T) {
	s := miniredis.RunT(t)
	reg := prometheus.NewRegistry()
	m := metrics.New(reg, time.Minute)
	in := config.Instance{Name: "x", Namespace: "ns", Mode: config.ModeSentinel, Port: 6379}
	r := NewRunner(in, fastProbe, nil, m, discard())
	r.newPath = func(name string) Path {
		return Path{Name: name, NewClient: func(Client) *redis.Client {
			return redis.NewClient(&redis.Options{Addr: s.Addr(), MaxRetries: -1})
		}}
	}
	var mu sync.Mutex
	paths := []string{PathSentinel, PathRFRM}
	setPaths := func(p ...string) {
		mu.Lock()
		defer mu.Unlock()
		paths = p
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(paths)
		})
	}()
	perPath := map[string]int{
		"redis_soak_writable": len(Clients), "redis_soak_readable": len(Clients),
		"redis_soak_probe_duration_seconds": len(Clients) * 2, "redis_soak_last_success_timestamp_seconds": len(Clients) * 2,
	}
	probed := func(path string) func() bool {
		return func() bool {
			for name, n := range perPath {
				if series(t, reg, name, "path", path) != n {
					return false
				}
			}
			return true
		}
	}
	waitFor(t, "both paths probed", func() bool { return probed(PathSentinel)() && probed(PathRFRM)() })

	setPaths(PathRFRM)
	waitFor(t, "the sentinel path's series deleted", func() bool {
		for _, name := range []string{"redis_soak_probe_total", "redis_soak_writable", "redis_soak_readable",
			"redis_soak_probe_duration_seconds", "redis_soak_last_success_timestamp_seconds"} {
			if series(t, reg, name, "path", PathSentinel) != 0 {
				return false
			}
		}
		return true
	})
	ok := func() float64 {
		return testutil.ToFloat64(m.ProbeTotal.WithLabelValues("x", "ns", "sentinel", PathRFRM, string(Pooled), "set", ResultOK))
	}
	before := ok()
	waitFor(t, "rfrm still probed", func() bool { return ok() > before })
	if n := series(t, reg, "redis_soak_writable", "path", PathSentinel); n != 0 {
		t.Errorf("%d sentinel series came back", n)
	}

	setPaths(PathSentinel, PathRFRM)
	waitFor(t, "the sentinel path probed again", probed(PathSentinel))
	cancel()
	<-done
}

type secrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *secrets) get(name string) (*corev1.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pw, ok := s.m[name]; ok {
		return auth.Secret(nil, "ns", name, pw), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
}

func (s *secrets) set(name, pw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = pw
}

// TestFollowerSwitchesPassword changes the Secret before the server's
// password: the follower fails until the server has it too, the pooled
// client keeps its authenticated connection.
func TestFollowerSwitchesPassword(t *testing.T) {
	s := miniredis.RunT(t)
	s.RequireAuth("one")
	sec := &secrets{m: map[string]string{"x-auth": "one"}}
	a := auth.New(sec.get)
	rf := &redisfailoverv1.RedisFailover{}
	rf.Spec.Auth.SecretPath = "x-auth"
	a.Update(rf)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg, time.Minute)
	in := config.Instance{Name: "x", Namespace: "ns", Mode: config.ModeOperator, Port: 6379}
	path := Path{Name: PathRFRM, NewClient: func(c Client) *redis.Client {
		return redis.NewClient(&redis.Options{Addr: s.Addr(), CredentialsProvider: credentials(c, a), MaxRetries: -1, PoolSize: 1})
	}}
	var probers []*Prober
	for _, c := range []Client{Pooled, Follower} {
		probers = append(probers, New(in, path, c, fastProbe, a, m, discard()))
	}
	// miniredis reads its password unlocked in HELLO, so it is changed
	// only while nothing dials.
	run := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for _, p := range probers {
			wg.Go(func() { p.Run(ctx) })
		}
		return func() {
			cancel()
			wg.Wait()
		}
	}
	total := func(c Client, result string) float64 {
		return testutil.ToFloat64(m.ProbeTotal.WithLabelValues("x", "ns", "operator", PathRFRM, string(c), "set", result))
	}
	stop := run()
	waitFor(t, "both probing", func() bool { return total(Pooled, ResultOK) > 0 && total(Follower, ResultOK) > 0 })

	sec.set("x-auth", "two")
	waitFor(t, "the follower refused", func() bool { return total(Follower, ResultAuth) > 0 })
	pooledOK := total(Pooled, ResultOK)
	waitFor(t, "the pooled client still writing", func() bool { return total(Pooled, ResultOK) > pooledOK })
	stop()
	s.RequireAuth("two")
	stop = run()
	outages := func() uint64 {
		mfs, _ := reg.Gather()
		for _, mf := range mfs {
			if mf.GetName() != "redis_soak_outage_duration_seconds" {
				continue
			}
			for _, mt := range mf.GetMetric() {
				for _, l := range mt.GetLabel() {
					if l.GetName() == "client" && l.GetValue() == string(Follower) {
						return mt.GetHistogram().GetSampleCount()
					}
				}
			}
		}
		return 0
	}
	waitFor(t, "the follower's outage ended", func() bool { return outages() == 1 })
	stop()
	if n := total(Pooled, ResultAuth); n != 0 {
		t.Errorf("the pooled client was refused %v times", n)
	}
}
