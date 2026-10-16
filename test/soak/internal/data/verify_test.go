package data

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// switchMaster is a Master whose address a test changes. It counts the
// calls of MasterAddr, one for each verification attempt.
type switchMaster struct {
	addr  atomic.Pointer[string]
	calls atomic.Int64
	ch    chan string
}

func newSwitchMaster(addr string) *switchMaster {
	m := &switchMaster{ch: make(chan string, 1)}
	m.set(addr)
	return m
}

func (m *switchMaster) set(addr string) { m.addr.Store(&addr) }

func (m *switchMaster) MasterAddr() string {
	m.calls.Add(1)
	return *m.addr.Load()
}

func (m *switchMaster) Failovers() <-chan string { return m.ch }

func verified(mt *metrics.Metrics, name, event string) float64 {
	return testutil.ToFloat64(mt.LedgerVerified.WithLabelValues(name, "ns", "operator", event))
}

// startVerifier runs the verifier of d until the test ends or the returned
// function cancels its ctx.
func startVerifier(t *testing.T, d *Data) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.runVerifier(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

// Verify gets its result from the running verifier, counts a lossless
// loss as unexpected and ends the mutation.
func TestVerifyThroughTheVerifier(t *testing.T) {
	d, m, _, mt := newTestData(t, false)
	ctx := context.Background()
	d.writeFill(ctx, 50)
	m.Del(FillKey("x", 7))
	startVerifier(t, d)
	d.Begin()
	n, err := d.Verify(ctx, "kill_master", 1, true)
	if err != nil || n != 1 {
		t.Fatalf("Verify = %d, %v, want 1 lost", n, err)
	}
	if got := testutil.ToFloat64(mt.UnexpectedLost.WithLabelValues("x", "ns", "operator", "kill_master")); got != 1 {
		t.Errorf("unexpected %v, want 1", got)
	}
	if d.mutating.Load() {
		t.Error("the mutation did not end")
	}
}

// The caller's ctx cancels a verification that does not succeed. The
// verifier then takes the next request.
func TestVerifyCancelledByItsCaller(t *testing.T) {
	d, m, _, mt := newTestData(t, false)
	master := newSwitchMaster("")
	d.master = master
	startVerifier(t, d)
	d.Begin()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := d.Verify(ctx, "kill_master", 1, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify: %v", err)
	}
	if master.calls.Load() == 0 {
		t.Error("the verifier did not try")
	}
	master.set(m.Addr())
	if _, err := d.Verify(context.Background(), "kill_master", 2, false); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if got := verified(mt, "x", "kill_master"); got != 1 {
		t.Errorf("verified %v, want 1", got)
	}
}

// Only the caller's ctx cancels a verification: the end of the verifier's
// ctx does not.
func TestVerifyOutlivesTheVerifierContext(t *testing.T) {
	d, m, _, mt := newTestData(t, false)
	master := newSwitchMaster("")
	d.master = master
	stop := startVerifier(t, d)
	d.Begin()
	type result struct {
		n   int
		err error
	}
	res := make(chan result, 1)
	go func() {
		n, err := d.Verify(context.Background(), "kill_master", 1, false)
		res <- result{n, err}
	}()
	waitFor(t, func() bool { return master.calls.Load() > 0 })
	stop()
	select {
	case r := <-res:
		t.Fatalf("Verify returned %d, %v when the verifier's ctx ended", r.n, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	master.set(m.Addr())
	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("Verify: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Verify did not return")
	}
	if got := verified(mt, "x", "kill_master"); got != 1 {
		t.Errorf("verified %v, want 1", got)
	}
}

// A failover while the verifier verifies for a mutation waits, and the
// verifier verifies it after the mutation ends.
func TestFailoverDuringVerifyIsVerifiedAfter(t *testing.T) {
	d, m, fi, mt := newTestData(t, false)
	master := newSwitchMaster(m.Addr())
	d.master = master
	fi.afterInfo = func(call int64) {
		if call == 1 {
			master.ch <- config.EventFailover
		}
	}
	startVerifier(t, d)
	d.Begin()
	if _, err := d.Verify(context.Background(), "kill_master", 1, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return verified(mt, "x", config.EventFailover) == 1 })
	if got := verified(mt, "x", "kill_master"); got != 1 {
		t.Errorf("verified %v, want 1", got)
	}
}

// Replica.Verify gets its result from Run, and only the caller's ctx
// cancels it.
func TestReplicaVerifyThroughRun(t *testing.T) {
	src, m, fi, mt := newTestData(t, true)
	const replication = "# Replication\r\nmaster_replid:r1\r\nmaster_repl_offset:100\r\nslave_repl_offset:100\r\nmaster_link_status:up\r\n"
	m.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if strings.EqualFold(cmd, "INFO") && len(args) == 1 && args[0] == "replication" {
			c.WriteBulk(replication)
			return true
		}
		return fi.hook(c, cmd, args...)
	})
	cfg, err := config.Parse([]byte("mutation: {seed: 1}\ninstances: [{name: x, namespace: ns, data: {fill: {sizeMi: 1}, ledger: {}}}, {name: b, namespace: ns, bootstrap: {source: x}}]"))
	if err != nil {
		t.Fatal(err)
	}
	r := NewReplica(cfg.Instances[1], cfg, src, fakePods{{Name: "rfr-b-0", Addr: m.Addr()}}, nil, mt, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// Without source writes, the verification does not succeed.
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	if _, err := r.Verify(short, "kill_master", 1, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify without writes: %v", err)
	}
	for range 10 {
		seq := src.ledger.begin()
		key := LedgerKey("x", seq)
		src.ledger.end(seq, src.client.Set(context.Background(), key, Value(key, 64), 0).Err() == nil)
	}
	m.Del(LedgerKey("x", 3))
	_ = m.Set(LedgerKey("x", 3), "garbage")
	n, err := r.Verify(context.Background(), "kill_master", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	// The pod is the source's master: the key with a wrong value counts as
	// gone on the source, not as lost on the pod.
	if n != 0 || verified(mt, "b", "kill_master") != 1 {
		t.Errorf("Verify = %d, verified %v", n, verified(mt, "b", "kill_master"))
	}
}

// A failed read of verify returns its error, and verify counts nothing and
// keeps the ledger as it was. MGET 1 reads the recent writes, 2 the older
// sample, 3 all older writes after a loss in the sample, and 4 the fill.
// Read 0 is INFO memory.
func TestVerifyReadErrors(t *testing.T) {
	for _, failAt := range []int64{0, 1, 2, 3, 4} {
		d, m, fi, mt := newTestData(t, true)
		ctx := context.Background()
		write := func(n int) {
			for range n {
				seq := d.ledger.begin()
				key := LedgerKey("x", seq)
				d.ledger.end(seq, d.client.Set(ctx, key, Value(key, 64), 0).Err() == nil)
			}
		}
		write(10)
		d.writeFill(ctx, 20)
		if _, err := d.verify(ctx, config.EventPeriodic, 0, false); err != nil {
			t.Fatal(err)
		}
		write(10)
		m.Del(LedgerKey("x", 2))
		var mgets atomic.Int64
		m.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
			if strings.EqualFold(cmd, "MGET") && mgets.Add(1) == failAt {
				c.WriteError("ERR MGET failed")
				return true
			}
			return fi.hook(c, cmd, args...)
		})
		if failAt == 0 {
			fi.failInfo.Store(fi.infos.Load() + 1)
		}
		acked := format(d.ledger.acked.spans)
		n, err := d.verify(ctx, "kill_master", 1, false)
		if err == nil || n != 0 {
			t.Errorf("read %d: verify = %d, %v, want an error", failAt, n, err)
		}
		if got := verified(mt, "x", "kill_master"); got != 0 {
			t.Errorf("read %d: verified %v", failAt, got)
		}
		if got := format(d.ledger.acked.spans); got != acked || !m.Exists(LedgerKey("x", 5)) {
			t.Errorf("read %d: acked %s, want %s", failAt, got, acked)
		}
		if failAt > 0 && mgets.Load() != failAt {
			t.Errorf("read %d: %d MGETs", failAt, mgets.Load())
		}
	}
}
