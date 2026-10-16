package observer

import (
	"io"
	"log/slog"
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

const mib = 1 << 20

// memory is an INFO reply of a server with the policy, maxmemory, used memory
// and memory not counted for eviction.
func memory(policy string, maxmemory, used, notCounted int64) info {
	i := info{
		"maxmemory_policy": policy,
		"maxmemory":        strconv.FormatInt(maxmemory, 10),
		"used_memory":      strconv.FormatInt(used, 10),
	}
	if notCounted >= 0 {
		i["mem_not_counted_for_evict"] = strconv.FormatInt(notCounted, 10)
	}
	return i
}

func TestFull(t *testing.T) {
	// The margin is 1 percent of maxmemory, at least 1Mi: 2Mi of 200Mi.
	for _, c := range []struct {
		name string
		i    info
		want bool
	}{
		{"noeviction at maxmemory", memory("noeviction", 200*mib, 201*mib, -1), true},
		{"noeviction at the margin", memory("noeviction", 200*mib, 198*mib, -1), true},
		{"noeviction below the margin", memory("noeviction", 200*mib, 198*mib-1, -1), false},
		{"the buffers do not count", memory("noeviction", 200*mib, 205*mib, 10*mib), false},
		{"the buffers do not count, full", memory("noeviction", 200*mib, 205*mib, 5*mib), true},
		{"a small maxmemory has a margin of 1Mi", memory("noeviction", 50*mib, 49*mib, 0), true},
		{"volatile-lru evicts", memory("volatile-lru", 200*mib, 201*mib, -1), false},
		{"allkeys-lru evicts", memory("allkeys-lru", 200*mib, 201*mib, -1), false},
		{"no maxmemory", memory("noeviction", 0, 201*mib, -1), false},
		{"no INFO", nil, false},
	} {
		if got := c.i.full(); got != c.want {
			t.Errorf("%s: full %t, want %t", c.name, got, c.want)
		}
	}
}

func newNoevictionObserver(t *testing.T, limit string) *Observer {
	t.Helper()
	cfg, err := config.Parse([]byte("observer: {noevictionFullLimit: " + limit + "}\ninstances: [{name: x, namespace: ns, maxMemoryPolicy: noeviction}]"))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg.Instances[0], cfg, nil, nil, nil, &global.Lock{}, metrics.New(prometheus.NewRegistry(), time.Minute),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// withMemory gives each redis pod of a healthy snapshot the memory INFO.
func withMemory(i info) snapshot {
	s := healthy(false)
	for p := range s.redis {
		maps.Copy(s.redis[p].info, i)
	}
	return s
}

// The full state is expected for noevictionFullLimit. Then it is one finding,
// and no longer expected.
func TestNoevictionFullBound(t *testing.T) {
	o := newNoevictionObserver(t, "10m")
	full := withMemory(memory("noeviction", 200*mib, 201*mib, -1))
	finding := func() float64 { return testutil.ToFloat64(o.findings.WithLabelValues(invNoevictionFull)) }
	start := time.Unix(10000, 0)
	for _, c := range []struct {
		after    time.Duration
		s        snapshot
		expected bool
		findings float64
	}{
		{0, full, true, 0},
		{9 * time.Minute, full, true, 0},
		{10 * time.Minute, full, false, 1},
		{20 * time.Minute, full, false, 1},
		// Memory freed, for example by a change of the policy.
		{21 * time.Minute, withMemory(memory("noeviction", 200*mib, 100*mib, -1)), false, 1},
		{22 * time.Minute, full, true, 1},
		{33 * time.Minute, full, false, 2},
	} {
		o.apply(start.Add(c.after), c.s, 1, false)
		if o.NoevictionFull() != c.expected || finding() != c.findings {
			t.Errorf("after %s: expected %t, %v findings; want %t, %v", c.after, o.NoevictionFull(), finding(), c.expected, c.findings)
		}
	}
	// Without a single master, no state is expected.
	s := full
	s.redis[1].info["role"] = "master"
	o.apply(start.Add(40*time.Minute), s, 1, false)
	if o.NoevictionFull() {
		t.Error("expected without a single master")
	}
}

// A round without a single master tells nothing about the memory. It keeps
// the time of the full state, and it gives no time beyond the limit.
func TestNoevictionFullUnknownRound(t *testing.T) {
	full := withMemory(memory("noeviction", 200*mib, 201*mib, -1))
	notFull := withMemory(memory("noeviction", 200*mib, 100*mib, -1))
	unknown := withMemory(memory("noeviction", 200*mib, 201*mib, -1))
	unknown.redis[0].info = nil
	type round struct {
		after    time.Duration
		s        snapshot
		expected bool
		findings float64
	}
	for _, c := range []struct {
		name   string
		rounds []round
	}{
		{"unknown rounds keep the time", []round{
			{0, full, true, 0}, {4 * time.Minute, unknown, true, 0}, {8 * time.Minute, unknown, true, 0},
			{9 * time.Minute, full, true, 0}, {10 * time.Minute, full, false, 1}, {11 * time.Minute, full, false, 1},
		}},
		{"a not full round ends the state", []round{
			{0, full, true, 0}, {4 * time.Minute, unknown, true, 0}, {5 * time.Minute, notFull, false, 0},
			{6 * time.Minute, full, true, 0}, {15 * time.Minute, full, true, 0}, {16 * time.Minute, full, false, 1},
		}},
		{"unknown beyond the limit", []round{
			{0, full, true, 0}, {5 * time.Minute, unknown, true, 0}, {12 * time.Minute, unknown, false, 1},
			{13 * time.Minute, full, false, 1},
		}},
		{"unknown before a full round", []round{{0, unknown, false, 0}, {1 * time.Minute, full, true, 0}}},
	} {
		o := newNoevictionObserver(t, "10m")
		start := time.Unix(10000, 0)
		for _, r := range c.rounds {
			o.apply(start.Add(r.after), r.s, 1, false)
			got := testutil.ToFloat64(o.findings.WithLabelValues(invNoevictionFull))
			if o.NoevictionFull() != r.expected || got != r.findings {
				t.Errorf("%s, after %s: expected %t, %v findings; want %t, %v", c.name, r.after, o.NoevictionFull(), got, r.expected, r.findings)
			}
		}
	}
}

// An instance without maxMemoryPolicy can get maxMemory by a change by hand.
// Its finding series start at 0, so that the alert sees the increase of the
// first finding.
func TestNoevictionSeriesAtStart(t *testing.T) {
	o, reg := newTestObserver(t)
	for _, inv := range []string{invNoevictionFull, invMaxmemoryBelowUsed} {
		if !hasSeries(t, reg, "redis_soak_findings_total", "invariant", inv) {
			t.Errorf("no %s series at the start", inv)
		}
	}
	o.apply(time.Unix(0, 0), withMemory(memory("noeviction", 200*mib, 150*mib, -1)), 1, false)
	o.apply(time.Unix(5, 0), withMemory(memory("noeviction", 100*mib, 150*mib, -1)), 1, false)
	if got := testutil.ToFloat64(o.findings.WithLabelValues(invMaxmemoryBelowUsed)); got != 3 {
		t.Errorf("%v findings, want 3", got)
	}
}

// The default limit is 30 minutes.
func TestNoevictionFullDefault(t *testing.T) {
	cfg, err := config.Parse([]byte("instances: [{name: x, namespace: ns}]"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Observer.NoevictionFullLimit.Duration; got != 30*time.Minute {
		t.Errorf("noevictionFullLimit %s", got)
	}
	if _, err := config.Parse([]byte("observer: {noevictionFullLimit: -1s}\ninstances: [{name: x, namespace: ns}]")); err == nil {
		t.Error("a negative limit")
	}
}

// A maxmemory that decreases below the used memory is a finding under a
// policy that is not allkeys-*.
func TestMaxmemoryBelowUsed(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after info
		want          float64
	}{
		{"noeviction, lowered below used", memory("noeviction", 200*mib, 150*mib, -1), memory("noeviction", 100*mib, 150*mib, -1), 3},
		{"volatile-lru, lowered below used", memory("volatile-lru", 200*mib, 150*mib, -1), memory("volatile-lru", 100*mib, 150*mib, -1), 3},
		{"allkeys-lru evicts", memory("allkeys-lru", 200*mib, 150*mib, -1), memory("allkeys-lru", 100*mib, 150*mib, -1), 0},
		{"lowered, above used", memory("noeviction", 200*mib, 90*mib, -1), memory("noeviction", 100*mib, 90*mib, -1), 0},
		// The margin covers a write between the CONFIG SET and the sample.
		{"lowered, used in the margin", memory("noeviction", 200*mib, 101*mib, -1), memory("noeviction", 100*mib, 101*mib, -1), 0},
		{"the buffers do not count", memory("noeviction", 200*mib, 150*mib, 60*mib), memory("noeviction", 100*mib, 150*mib, 60*mib), 0},
		{"raised", memory("noeviction", 100*mib, 150*mib, -1), memory("noeviction", 200*mib, 150*mib, -1), 0},
		{"unlimited to below used", memory("noeviction", 0, 150*mib, -1), memory("noeviction", 100*mib, 150*mib, -1), 3},
		{"to unlimited", memory("noeviction", 100*mib, 150*mib, -1), memory("noeviction", 0, 150*mib, -1), 0},
	} {
		o := newNoevictionObserver(t, "30m")
		o.apply(time.Unix(0, 0), withMemory(c.before), 1, false)
		o.apply(time.Unix(5, 0), withMemory(c.after), 1, false)
		if got := testutil.ToFloat64(o.findings.WithLabelValues(invMaxmemoryBelowUsed)); got != c.want {
			t.Errorf("%s: %v findings, want %v", c.name, got, c.want)
		}
	}
}

// A pod without INFO keeps its last maxmemory, and the first round of a pod
// has no earlier value.
func TestMaxmemoryBelowUsedWithoutInfo(t *testing.T) {
	o := newNoevictionObserver(t, "30m")
	o.apply(time.Unix(0, 0), withMemory(memory("noeviction", 200*mib, 150*mib, -1)), 1, false)
	s := withMemory(memory("noeviction", 100*mib, 150*mib, -1))
	s.redis[0].info = nil
	s.redis[1].UID = "u1-new"
	o.apply(time.Unix(5, 0), s, 1, false)
	if got := testutil.ToFloat64(o.findings.WithLabelValues(invMaxmemoryBelowUsed)); got != 1 {
		t.Fatalf("%v findings, want 1 for rfr-x-2", got)
	}
	o.apply(time.Unix(10, 0), withMemory(memory("noeviction", 100*mib, 150*mib, -1)), 1, false)
	if got := testutil.ToFloat64(o.findings.WithLabelValues(invMaxmemoryBelowUsed)); got != 2 {
		t.Errorf("%v findings, want 2: rfr-x-0 kept 200Mi", got)
	}
}
