package data

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

func TestValue(t *testing.T) {
	a := Value("soak:x:fill:1", 64)
	if len(a) != 64 || a != Value("soak:x:fill:1", 64) {
		t.Errorf("not deterministic: %q", a)
	}
	if a == Value("soak:x:fill:2", 64) || a == Value("soak:y:fill:1", 64) {
		t.Error("two keys have the same value")
	}
	if b := Value("soak:x:fill:1", 1024); len(b) != 1024 || !strings.HasPrefix(b, a) {
		t.Errorf("a longer value doesn't extend the shorter one")
	}
	if FillKey("x", 3) != "soak:x:fill:3" || LedgerKey("x", 3) != "soak:x:ledger:3" || BurstKey("x", 3) != "soak:x:burst:3" {
		t.Error("key names")
	}
}

func TestRanges(t *testing.T) {
	var r ranges
	for _, n := range []int64{0, 1, 2, 3, 5, 6, 9, 4, 2, 8} {
		r.add(n)
	}
	if got := format(r.spans); got != "0-6,8-9" {
		t.Errorf("spans %s", got)
	}
	r.remove(3)
	r.remove(0)
	r.remove(9)
	r.remove(7)
	if got := format(r.spans); got != "1-2,4-6,8" {
		t.Errorf("after remove: %s", got)
	}
	if got := format(r.within(2, 6)); got != "2,4-5" {
		t.Errorf("within: %s", got)
	}
	if count(r.spans) != 6 || !slices.Equal(members(r.within(0, 5)), []int64{1, 2, 4}) {
		t.Errorf("count %d, members %v", count(r.spans), members(r.within(0, 5)))
	}
	r.dropBelow(5)
	if got := format(r.spans); got != "5-6,8" {
		t.Errorf("dropBelow: %s", got)
	}

	// A long, mostly consecutive run stays a few spans.
	var big ranges
	for n := range int64(1_000_000) {
		if n%250_000 != 7 {
			big.add(n)
		}
	}
	if len(big.spans) != 5 || count(big.spans) != 1_000_000-4 {
		t.Errorf("%d spans, %d numbers", len(big.spans), count(big.spans))
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	s := sample(rnd, big.spans, 100)
	if len(s) != 100 || !slices.IsSorted(s) || slices.Compact(slices.Clone(s))[99] != s[99] {
		t.Errorf("sample %v", s)
	}
	for _, n := range s {
		if n%250_000 == 7 {
			t.Errorf("sampled %d, which isn't in the set", n)
		}
	}
	if got := sample(rnd, r.spans, 10); !slices.Equal(got, []int64{5, 6, 8}) {
		t.Errorf("small sample %v", got)
	}
}

func TestLedgerAging(t *testing.T) {
	l := newLedger()
	rnd := rand.New(rand.NewPCG(1, 2))
	write := func(n int, ack func(int64) bool) {
		for range n {
			seq := l.begin()
			l.end(seq, ack(seq))
		}
	}
	all := func(int64) bool { return true }
	write(10, func(seq int64) bool { return seq != 4 })

	// The first round checks everything acknowledged so far.
	r := l.plan(rnd, 3)
	if format(r.recent) != "0-3,5-9" || len(r.older) != 0 || r.to != 10 {
		t.Fatalf("round 1: %+v", r)
	}
	if old := l.commit(r, []int64{7}); old.lo != old.hi {
		t.Errorf("nothing to age yet: %+v", old)
	}

	// A write in flight isn't checked yet.
	write(5, all)
	inflight := l.begin()
	r = l.plan(rnd, 3)
	if format(r.recent) != "10-14" || len(r.older) != 3 || r.to != inflight {
		t.Fatalf("round 2: %+v", r)
	}
	for _, n := range r.older {
		if n == 4 || n == 7 || n >= 10 {
			t.Errorf("sampled %d", n)
		}
	}
	l.end(inflight, true)
	old := l.commit(r, nil)
	if old != (span{0, 10}) {
		t.Errorf("aged %+v, want the first round", old)
	}
	l.agedOut(old)

	// Rounds before the previous one are forgotten.
	write(2, all)
	r = l.plan(rnd, 100)
	if format(r.recent) != "15-17" || format(spansOf(r.older)) != "10-14" {
		t.Errorf("round 3: recent %s, older %v", format(r.recent), r.older)
	}
	if got := format(l.acked.spans); got != "10-17" {
		t.Errorf("acked %s", got)
	}
}

type fakeMaster struct {
	addr string
	ch   chan string
}

func (f fakeMaster) MasterAddr() string       { return f.addr }
func (f fakeMaster) Failovers() <-chan string { return f.ch }

// fakeInfo answers INFO memory with the fill keys' size as used_memory,
// and what policy and maxmemory hold.
type fakeInfo struct {
	m         *miniredis.Miniredis
	maxMemory atomic.Int64
	policy    atomic.Pointer[string]
	// oomAbove rejects SET with OOM once the instance holds more keys.
	oomAbove atomic.Int64
}

func (f *fakeInfo) hook(c *server.Peer, cmd string, args ...string) bool {
	switch {
	case strings.EqualFold(cmd, "INFO") && len(args) == 1 && args[0] == "memory":
		c.WriteBulk(fmt.Sprintf("# Memory\r\nused_memory:%d\r\nmaxmemory:%d\r\nmaxmemory_policy:%s\r\n",
			len(f.m.Keys())*1024, f.maxMemory.Load(), *f.policy.Load()))
		return true
	case strings.EqualFold(cmd, "SET") && f.oomAbove.Load() > 0 && int64(len(f.m.Keys())) >= f.oomAbove.Load():
		c.WriteError("OOM command not allowed when used memory > 'maxmemory'.")
		return true
	}
	return false
}

func newTestData(t *testing.T, ledger bool) (*Data, *miniredis.Miniredis, *fakeInfo, *metrics.Metrics) {
	t.Helper()
	cfgYAML := "instances: [{name: x, namespace: ns, maxMemoryPolicy: noeviction, data: {fill: {percent: 50, sizeMi: 1, valueBytes: 1024, keysPerSecond: 100000, batch: 100, sampleKeys: 20}}}]"
	if ledger {
		cfgYAML = strings.Replace(cfgYAML, "sampleKeys: 20}", "sampleKeys: 20}, ledger: {sampleKeys: 5}", 1)
	}
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	m := miniredis.RunT(t)
	fi := &fakeInfo{m: m}
	policy := "noeviction"
	fi.policy.Store(&policy)
	fi.maxMemory.Store(400 * 1024)
	m.Server().SetPreHook(fi.hook)
	mt := metrics.New(prometheus.NewRegistry(), time.Minute)
	d := New(cfg.Instances[0], cfg, fakeMaster{addr: m.Addr(), ch: make(chan string, 1)}, nil, mt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = d.client.Close()
	d.client = redis.NewClient(&redis.Options{Addr: m.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = d.client.Close() })
	return d, m, fi, mt
}

func TestFill(t *testing.T) {
	d, m, fi, _ := newTestData(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.runFiller(ctx); close(done) }()
	waitFor(t, d.Filled)
	// 50% of 400 keys' worth.
	if n := len(m.Keys()); n < 200 || n > 210 {
		t.Errorf("%d keys, want about 200", n)
	}
	for _, k := range m.Keys() {
		if v, _ := m.Get(k); v != Value(k, 1024) {
			t.Fatalf("%s has the wrong value", k)
		}
	}
	// Topped up after an eviction.
	for i := range 50 {
		m.Del(FillKey("x", int64(i)))
	}
	waitFor(t, func() bool { return len(m.Keys()) >= 200 })
	// Without maxmemory, sizeMi.
	fi.maxMemory.Store(0)
	waitFor(t, func() bool { return len(m.Keys()) >= 1024 })
	cancel()
	<-done
}

func TestVerify(t *testing.T) {
	d, m, _, mt := newTestData(t, true)
	ctx := context.Background()
	lost := func(event string) float64 {
		return testutil.ToFloat64(mt.LostWrites.WithLabelValues("x", "ns", "operator", event))
	}
	// 100 ledger writes and 50 fill keys, acknowledged.
	for range 100 {
		seq := d.ledger.begin()
		key := LedgerKey("x", seq)
		d.ledger.end(seq, d.client.Set(ctx, key, Value(key, 64), 0).Err() == nil)
	}
	d.writeFill(ctx, 50)

	if _, err := d.verify(ctx, "kill_master", 1); err != nil {
		t.Fatal(err)
	}
	if lost("kill_master") != 0 {
		t.Fatalf("lost %v writes on an intact instance", lost("kill_master"))
	}

	// After a failover, writes since the previous verification are gone
	// or wrong, and an older one is corrupted.
	for range 20 {
		seq := d.ledger.begin()
		key := LedgerKey("x", seq)
		d.ledger.end(seq, d.client.Set(ctx, key, Value(key, 64), 0).Err() == nil)
	}
	m.Del(LedgerKey("x", 110))
	m.Del(LedgerKey("x", 111))
	_ = m.Set(LedgerKey("x", 115), "garbage")
	m.Del(FillKey("x", 3))
	_ = m.Set(FillKey("x", 4), "garbage")
	d.cfg.Fill.SampleKeys = 50
	d.failedOver.Store(true)
	n, err := d.verify(ctx, config.EventFailover, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Every recent write, and both fill keys as all are sampled.
	if got := lost(config.EventFailover); got != 5 || n != 5 {
		t.Errorf("lost %v, returned %d, want 5", got, n)
	}
	if testutil.ToFloat64(mt.LedgerVerified.WithLabelValues("x", "ns", "operator", config.EventFailover)) != 1 {
		t.Error("ledger_verified_total not counted")
	}
	// The first round's keys are aged out, the second's kept for samples.
	if m.Exists(LedgerKey("x", 50)) || !m.Exists(LedgerKey("x", 105)) {
		t.Errorf("aging: 50 exists %v, 105 exists %v", m.Exists(LedgerKey("x", 50)), m.Exists(LedgerKey("x", 105)))
	}
	if got := format(d.ledger.acked.spans); got != "100-109,112-114,116-119" {
		t.Errorf("acked %s", got)
	}
	// Each loss is counted once, and the samples of the second round
	// pass.
	if _, err := d.verify(ctx, config.EventPeriodic, 0); err != nil {
		t.Fatal(err)
	}
	if got := lost(config.EventPeriodic); got != 0 {
		t.Errorf("counted %v losses again", got)
	}
	if m.Exists(LedgerKey("x", 105)) || len(d.ledger.acked.spans) != 0 {
		t.Errorf("second round not aged out: %s", format(d.ledger.acked.spans))
	}
}

func TestVerifyAfterReset(t *testing.T) {
	d, m, _, mt := newTestData(t, true)
	ctx := context.Background()
	lost := func(event string) float64 {
		return testutil.ToFloat64(mt.LostWrites.WithLabelValues("x", "ns", "operator", event))
	}
	write := func(n int) {
		for range n {
			seq := d.ledger.begin()
			key := LedgerKey("x", seq)
			d.ledger.end(seq, d.client.Set(ctx, key, Value(key, 64), 0).Err() == nil)
		}
	}
	write(100)
	d.writeFill(ctx, 1000)
	if _, err := d.verify(ctx, config.EventPeriodic, 0); err != nil {
		t.Fatal(err)
	}
	write(30)
	// The only pod is killed: everything is gone.
	m.FlushAll()
	if _, err := d.verify(ctx, "kill_master", 1); err != nil {
		t.Fatal(err)
	}
	// Every ledger write, though only 5 older ones are sampled, and the
	// sampled fill keys.
	if got := lost("kill_master"); got != 130+20 {
		t.Errorf("lost %v, want 150", got)
	}
	write(10)
	d.writeFill(ctx, 10)
	if _, err := d.verify(ctx, "redis_replicas", 2); err != nil {
		t.Fatal(err)
	}
	if got := lost("redis_replicas"); got != 0 {
		t.Errorf("the next event inherited %v losses", got)
	}
}

// A reset recreates the instance empty on purpose: only the writes since
// count.
func TestVerifyRefilled(t *testing.T) {
	d, m, _, mt := newTestData(t, true)
	ctx := context.Background()
	write := func(n int) {
		for range n {
			seq := d.ledger.begin()
			key := LedgerKey("x", seq)
			d.ledger.end(seq, d.client.Set(ctx, key, Value(key, 64), 0).Err() == nil)
		}
	}
	write(100)
	d.writeFill(ctx, 1000)
	if _, err := d.verify(ctx, config.EventPeriodic, 0); err != nil {
		t.Fatal(err)
	}
	write(30)
	m.FlushAll()
	d.Refill()
	if d.Filled() {
		t.Error("filled after the reset")
	}
	write(10)
	d.writeFill(ctx, 10)
	m.Del(LedgerKey("x", 135))
	if n, err := d.verify(ctx, config.EventReset, 3); err != nil || n != 1 {
		t.Errorf("verify = %d, %v, want 1 lost", n, err)
	}
	if got := testutil.ToFloat64(mt.LostWrites.WithLabelValues("x", "ns", "operator", config.EventReset)); got != 1 {
		t.Errorf("lost %v, want 1", got)
	}
}

func TestVerifyEvictable(t *testing.T) {
	d, m, fi, mt := newTestData(t, false)
	ctx := context.Background()
	d.writeFill(ctx, 50)
	for i := range 10 {
		m.Del(FillKey("x", int64(i)))
	}
	_ = m.Set(FillKey("x", 20), "garbage")
	policy := "allkeys-lru"
	fi.policy.Store(&policy)
	d.cfg.Fill.SampleKeys = 50
	if _, err := d.verify(ctx, "redis_memory", 1); err != nil {
		t.Fatal(err)
	}
	// Evicted keys are expected; a wrong value isn't.
	if got := testutil.ToFloat64(mt.LostWrites.WithLabelValues("x", "ns", "operator", "redis_memory")); got != 1 {
		t.Errorf("lost %v, want 1", got)
	}
}

func TestBurst(t *testing.T) {
	d, m, fi, mt := newTestData(t, true)
	ctx := context.Background()
	d.writeFill(ctx, 100)
	fi.oomAbove.Store(300)
	skip, err := d.Burst(ctx, 300*time.Millisecond)
	if skip != "" || err != nil {
		t.Fatalf("skip %q, err %v", skip, err)
	}
	if n := testutil.ToFloat64(mt.OOMRejections.WithLabelValues("x", "ns", "operator")); n < 2 {
		t.Errorf("oom_rejections_total = %v", n)
	}
	// Only the fill keys are left.
	if n := len(m.Keys()); n != 100 {
		t.Errorf("%d keys left", n)
	}
	fi.oomAbove.Store(0)
	if err := d.Writable(ctx); err != nil {
		t.Error(err)
	}

	policy := "volatile-lru"
	fi.policy.Store(&policy)
	if skip, _ := d.Burst(ctx, time.Second); skip == "" {
		t.Error("burst without noeviction")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
