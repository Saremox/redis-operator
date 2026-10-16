package prober

import (
	"context"
	"io"
	"log/slog"
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

// A write that the master refuses with OOM while it is full under noeviction
// is expected. Each other failure is not.
func TestExpectedNotWritable(t *testing.T) {
	const oom = "OOM command not allowed when used memory > 'maxmemory'."
	for _, c := range []struct {
		name  string
		err   string
		full  bool
		want  float64
		count float64
	}{
		{"OOM, full", oom, true, 1, 1},
		{"OOM, not full", oom, false, 0, 0},
		{"another error, full", "READONLY You can't write against a read only replica.", true, 0, 0},
		{"no error, full", "", true, 0, 0},
	} {
		s := miniredis.RunT(t)
		m := metrics.New(prometheus.NewRegistry(), time.Minute)
		in := config.Instance{Name: "op-noevict", Namespace: "ns", Mode: config.ModeOperator, Port: 6379}
		path := Path{Name: PathRFRM, NewClient: func(Client) *redis.Client {
			return redis.NewClient(&redis.Options{Addr: s.Addr(), MaxRetries: -1})
		}}
		probe := config.Probe{Interval: metav1.Duration{Duration: time.Second}, Timeout: metav1.Duration{Duration: 200 * time.Millisecond}}
		p := New(in, path, Pooled, probe, nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
		p.full = func() bool { return c.full }
		s.SetError(c.err)
		client := path.NewClient(Pooled)
		p.probe(context.Background(), client)
		_ = client.Close()
		labels := []string{"op-noevict", "ns", "operator", PathRFRM, string(Pooled), ReasonNoevictionFull}
		if got := testutil.ToFloat64(m.ExpectedNotWritable.WithLabelValues(labels...)); got != c.want {
			t.Errorf("%s: expected_not_writable %v, want %v", c.name, got, c.want)
		}
		if got := testutil.ToFloat64(m.ExpectedNotWritableSeconds.WithLabelValues(labels...)); got != c.count {
			t.Errorf("%s: expected_not_writable_seconds_total %v, want %v", c.name, got, c.count)
		}
	}
}

// The runner gives each prober the function of the observer.
func TestRunnerSetFull(t *testing.T) {
	r := NewRunner(config.Instance{Name: "x"}, config.Probe{}, nil, nil, slog.New(slog.DiscardHandler))
	if r.full() {
		t.Error("full without SetFull")
	}
	r.SetFull(func() bool { return true })
	if !r.full() {
		t.Error("SetFull")
	}
}
