package data

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
	"github.com/saremox/redis-operator/test/soak/internal/poll"
	"github.com/saremox/redis-operator/test/soak/internal/prober"
)

// Master tells where the master is, and when it changed: Failovers
// receives config.EventFailover or config.EventReset.
type Master interface {
	MasterAddr() string
	Failovers() <-chan string
}

// Data fills one instance and keeps its ledger.
type Data struct {
	in      config.Instance
	cfg     config.Data
	timeout time.Duration
	// client writes through rfrm, like an application.
	client *redis.Client
	master Master
	auth   *auth.Source
	rnd    *rand.Rand
	log    *slog.Logger

	fillMu   sync.Mutex
	fillNext int64
	fillAck  ranges
	ledger   *ledger

	// policy is the master's maxmemory-policy as the filler last saw it.
	policy     atomic.Pointer[string]
	filled     atomic.Bool
	mutating   atomic.Bool
	failedOver atomic.Bool
	requests   chan *request

	oom        prometheus.Counter
	lost       *prometheus.CounterVec
	unexpected *prometheus.CounterVec
	verified   *prometheus.CounterVec
}

type request struct {
	ctx      context.Context
	event    string
	step     int
	lossless bool
	done     chan struct{}
	lost     int
	err      error
}

func New(in config.Instance, cfg *config.Config, master Master, a *auth.Source, m *metrics.Metrics, log *slog.Logger) *Data {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	h := fnv.New64a()
	_, _ = h.Write([]byte(in.Namespace + "/" + in.Name))
	d := &Data{
		in:         in,
		cfg:        *in.Data,
		timeout:    cfg.Probe.Timeout.Duration,
		client:     prober.MasterService(in, cfg.Probe.Timeout.Duration, a).NewClient(prober.Pooled),
		master:     master,
		auth:       a,
		rnd:        rand.New(rand.NewPCG(uint64(cfg.Mutation.Seed), h.Sum64())),
		log:        log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
		requests:   make(chan *request),
		oom:        m.OOMRejections.With(labels),
		lost:       m.LostWrites.MustCurryWith(labels),
		unexpected: m.UnexpectedLost.MustCurryWith(labels),
		verified:   m.LedgerVerified.MustCurryWith(labels),
	}
	if in.Data.Ledger != nil {
		d.ledger = newLedger()
	}
	initEvents(cfg, in, d.lost, d.unexpected)
	return d
}

// initEvents creates the series of each event at 0, because the alerts use
// their increase, which does not show a series that starts above 0.
func initEvents(cfg *config.Config, in config.Instance, counters ...*prometheus.CounterVec) {
	for _, e := range cfg.Events(in) {
		for _, c := range counters {
			c.WithLabelValues(e)
		}
	}
}

func (d *Data) Run(ctx context.Context) {
	defer func() { _ = d.client.Close() }()
	// Keys of an earlier run would never be verified or deleted.
	for !d.deleteMatching(ctx, "soak:"+d.in.Name+":ledger:*", "soak:"+d.in.Name+":burst:*") {
		if !sleep(ctx, 2*time.Second) {
			return
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { d.runFiller(ctx) })
	if d.ledger != nil {
		wg.Go(func() { d.runLedger(ctx) })
	}
	wg.Go(func() { d.runVerifier(ctx) })
	wg.Wait()
}

// Filled reports whether the data reached its target once.
func (d *Data) Filled() bool { return d.filled.Load() }

// Begin marks a mutation in progress: a failover meanwhile is verified
// after the mutation, as the mutation's.
func (d *Data) Begin() { d.mutating.Store(true) }

// Refill makes Filled false until the filler reached its target again,
// after the instance was recreated empty, and forgets every write so far.
func (d *Data) Refill() {
	d.filled.Store(false)
	if d.ledger != nil {
		d.ledger.forget()
	}
	d.fillMu.Lock()
	d.fillAck.dropBelow(d.fillNext)
	d.fillMu.Unlock()
}

// Verify verifies the data after a mutation and ends the mutation. event is
// the kind of the mutation, or config.EventReset for a mutation that loses the
// data by design. Lost writes of a lossless event are unexpected. Verify
// returns the number of lost writes, or an error if ctx is done before the
// verification.
func (d *Data) Verify(ctx context.Context, event string, step int, lossless bool) (int, error) {
	// The verifier does not get the request if ctx is done first.
	defer d.mutating.Store(false)
	return verifyAfter(ctx, d.requests, &request{ctx: ctx, event: event, step: step, lossless: lossless})
}

func verifyAfter(ctx context.Context, requests chan<- *request, r *request) (int, error) {
	r.done = make(chan struct{})
	select {
	case requests <- r:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return r.lost, r.err
}

func (d *Data) runVerifier(ctx context.Context) {
	var periodic <-chan time.Time
	if d.ledger != nil {
		t := time.NewTicker(d.cfg.Ledger.VerifyInterval.Duration)
		defer t.Stop()
		periodic = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-d.requests:
			r.lost, r.err = d.verifyRetrying(r.ctx, r.event, r.step, r.lossless)
			d.mutating.Store(false)
			close(r.done)
		case event := <-d.master.Failovers():
			d.failedOver.Store(true)
			if !d.mutating.Load() {
				_, _ = d.verifyRetrying(ctx, event, 0, false)
			}
		case <-periodic:
			if !d.mutating.Load() {
				_, _ = d.verifyRetrying(ctx, config.EventPeriodic, 0, false)
			}
		}
	}
}

// verifyRetrying verifies until it succeeds or ctx is done.
func (d *Data) verifyRetrying(ctx context.Context, event string, step int, lossless bool) (int, error) {
	return retry(ctx, d.log.With("event", event, "step", step), func(ctx context.Context) (int, error) {
		return d.verify(ctx, event, step, lossless)
	})
}

// retry calls verify every 2s until it succeeds or ctx is done, and logs
// every tenth error.
func retry(ctx context.Context, log *slog.Logger, verify func(context.Context) (int, error)) (int, error) {
	lost, attempt := 0, 0
	err := poll.Until(ctx, 2*time.Second, 0, func(ctx context.Context) error {
		var err error
		if lost, err = verify(ctx); err != nil && attempt%10 == 0 {
			log.Warn("verifying the data", "attempt", attempt+1, "error", err.Error())
		}
		attempt++
		return err
	})
	return lost, err
}

var errNoMaster = errors.New("no single master")

// verify checks the writes acknowledged since the previous verification,
// a sample of older ones and a sample of fill keys on the master.
func (d *Data) verify(ctx context.Context, event string, step int, lossless bool) (int, error) {
	addr := d.master.MasterAddr()
	if addr == "" {
		return 0, errNoMaster
	}
	start := time.Now()
	c := d.podClient(addr)
	defer func() { _ = c.Close() }()
	mi, err := memoryInfo(ctx, c)
	if err != nil {
		return 0, err
	}

	var r round
	var lostRecent, lostOlder []int64
	if d.ledger != nil {
		r = d.ledger.plan(d.rnd, d.cfg.Ledger.SampleKeys)
		size := d.cfg.Ledger.ValueBytes
		if lostRecent, _, err = d.check(ctx, c, LedgerKey, size, members(r.recent)); err != nil {
			return 0, err
		}
		if lostOlder, _, err = d.check(ctx, c, LedgerKey, size, r.older); err != nil {
			return 0, err
		}
		// If the sample lost writes, an unknown share of the older writes is
		// lost. Count all of them now, so that later events do not get this
		// loss.
		if len(lostOlder) > 0 {
			if lostOlder, _, err = d.check(ctx, c, LedgerKey, size, members(r.olderAll)); err != nil {
				return 0, err
			}
		}
	}
	d.fillMu.Lock()
	fillTo := d.fillNext
	fillSeqs := sample(d.rnd, d.fillAck.spans, d.cfg.Fill.SampleKeys)
	d.fillMu.Unlock()
	lostFill, missingFill, err := d.check(ctx, c, FillKey, d.cfg.Fill.ValueBytes, fillSeqs)
	if err != nil {
		return 0, err
	}
	// Fill keys may be evicted or expire, but never change.
	if d.cfg.Fill.TTL.Duration > 0 || mi.maxMemory > 0 && strings.HasPrefix(mi.policy, "allkeys-") {
		lostFill = slices.DeleteFunc(lostFill, func(n int64) bool { return slices.Contains(missingFill, n) })
	}

	if d.ledger != nil {
		old := d.ledger.commit(r, append(slices.Clone(lostRecent), lostOlder...))
		if old.lo < old.hi && d.deleteSeqs(ctx, c, old) {
			d.ledger.agedOut(old)
		}
	}
	if len(lostFill) > 0 {
		// The fill is only sampled. Forget the keys that the sample cannot
		// tell about, so that later events do not get this loss.
		d.fillMu.Lock()
		d.fillAck.dropBelow(fillTo)
		d.fillMu.Unlock()
	}

	lost := len(lostRecent) + len(lostOlder) + len(lostFill)
	d.lost.WithLabelValues(event).Add(float64(lost))
	if lossless {
		d.unexpected.WithLabelValues(event).Add(float64(lost))
	}
	d.verified.WithLabelValues(event).Inc()
	log := d.log.With("event", event, "step", step, "lossless", lossless, "failover", d.failedOver.Swap(false),
		"recent", count(r.recent), "recent_seqs", format(r.recent), "older", len(r.older), "fill", len(fillSeqs),
		"lost", lost, "duration_seconds", time.Since(start).Seconds())
	if lost > 0 {
		log.Warn("lost writes", "lost_recent", len(lostRecent), "lost_recent_seqs", format(spansOf(lostRecent)),
			"lost_older", len(lostOlder), "lost_older_seqs", format(spansOf(lostOlder)),
			"lost_fill", len(lostFill), "lost_fill_keys", format(spansOf(lostFill)))
	}
	log.Info("data verified")
	return lost, nil
}

// podClient connects to a pod directly, for verifications, whose
// pipelines read many keys.
func (d *Data) podClient(addr string) *redis.Client {
	return observer.Client(addr, d.auth.Provider(), 5*d.timeout)
}

const chunk = 500

// check reads the keys of seqs in pipelined MGETs, and returns those
// missing or with a value other than Value(key, size), and those missing.
func (d *Data) check(ctx context.Context, c *redis.Client, key func(string, int64) string, size int, seqs []int64) (lost, missing []int64, err error) {
	pipe := c.Pipeline()
	var cmds []*redis.SliceCmd
	for i := 0; i < len(seqs); i += chunk {
		keys := make([]string, 0, chunk)
		for _, n := range seqs[i:min(i+chunk, len(seqs))] {
			keys = append(keys, key(d.in.Name, n))
		}
		cmds = append(cmds, pipe.MGet(ctx, keys...))
	}
	if len(cmds) == 0 {
		return nil, nil, nil
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, nil, err
	}
	for ci, cmd := range cmds {
		for j, v := range cmd.Val() {
			n := seqs[ci*chunk+j]
			s, ok := v.(string)
			switch {
			case v == nil:
				lost = append(lost, n)
				missing = append(missing, n)
			case !ok || s != Value(key(d.in.Name, n), size):
				lost = append(lost, n)
			}
		}
	}
	return lost, missing, nil
}

// deleteSeqs deletes the ledger keys of s.
func (d *Data) deleteSeqs(ctx context.Context, c *redis.Client, s span) bool {
	for lo := s.lo; lo < s.hi; lo += chunk {
		keys := make([]string, 0, chunk)
		for n := lo; n < min(lo+chunk, s.hi); n++ {
			keys = append(keys, LedgerKey(d.in.Name, n))
		}
		if err := c.Unlink(ctx, keys...).Err(); err != nil {
			d.log.Warn("deleting aged ledger keys", "error", err.Error())
			return false
		}
	}
	return true
}

// deleteMatching deletes the keys matching the patterns.
func (d *Data) deleteMatching(ctx context.Context, patterns ...string) bool {
	for _, p := range patterns {
		iter := d.client.Scan(ctx, 0, p, 1000).Iterator()
		var keys []string
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		if err := iter.Err(); err != nil {
			d.log.Warn("deleting old keys", "pattern", p, "error", err.Error())
			return false
		}
		for i := 0; i < len(keys); i += chunk {
			if err := d.client.Unlink(ctx, keys[i:min(i+chunk, len(keys))]...).Err(); err != nil {
				d.log.Warn("deleting old keys", "pattern", p, "error", err.Error())
				return false
			}
		}
		if len(keys) > 0 {
			d.log.Info("deleted old keys", "pattern", p, "keys", len(keys))
		}
	}
	return true
}

func (d *Data) runLedger(ctx context.Context) {
	t := time.NewTicker(time.Second / time.Duration(d.cfg.Ledger.WritesPerSecond))
	defer t.Stop()
	paused := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// The ledger's keys must never be evicted; config validation keeps
		// the tester from switching to allkeys-*, but not a person.
		if p := d.policy.Load(); p != nil && strings.HasPrefix(*p, "allkeys-") {
			if !paused {
				d.log.Error("ledger paused: its keys can be evicted", "policy", *p)
				paused = true
			}
			continue
		}
		if paused {
			d.log.Info("ledger resumed")
			paused = false
		}
		seq := d.ledger.begin()
		key := LedgerKey(d.in.Name, seq)
		wctx, cancel := context.WithTimeout(ctx, d.timeout)
		err := d.client.Set(wctx, key, Value(key, d.cfg.Ledger.ValueBytes), 0).Err()
		cancel()
		d.ledger.end(seq, err == nil)
		d.countOOM(err)
	}
}

func (d *Data) countOOM(err error) {
	if prober.Classify(err) == prober.ResultOOM {
		d.oom.Inc()
	}
}

// runFiller keeps used_memory at the target: a share of maxmemory, or a
// fixed size without one.
func (d *Data) runFiller(ctx context.Context) {
	f := d.cfg.Fill
	lim := rate.NewLimiter(rate.Limit(f.KeysPerSecond), f.Batch)
	for ctx.Err() == nil {
		mi, err := memoryInfo(ctx, d.client)
		if err != nil {
			sleep(ctx, time.Second)
			continue
		}
		d.policy.Store(&mi.policy)
		target := f.SizeMi << 20
		if mi.maxMemory > 0 {
			target = mi.maxMemory * f.Percent / 100
		}
		if mi.used >= target {
			if !d.filled.Swap(true) {
				d.log.Info("data filled", "used_memory", mi.used, "target", target, "keys", d.fillNext)
			}
			sleep(ctx, time.Second)
			continue
		}
		// About a second's worth, before looking again.
		n := min(int64(f.KeysPerSecond), (target-mi.used)/int64(f.ValueBytes)+1)
		for n > 0 && ctx.Err() == nil {
			b := min(n, int64(f.Batch))
			if lim.WaitN(ctx, int(b)) != nil {
				return
			}
			d.writeFill(ctx, b)
			n -= b
		}
	}
}

func (d *Data) writeFill(ctx context.Context, n int64) {
	d.fillMu.Lock()
	from := d.fillNext
	d.fillNext += n
	d.fillMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	pipe := d.client.Pipeline()
	cmds := make([]*redis.StatusCmd, n)
	for i := range n {
		key := FillKey(d.in.Name, from+i)
		cmds[i] = pipe.Set(ctx, key, Value(key, d.cfg.Fill.ValueBytes), d.cfg.Fill.TTL.Duration)
	}
	_, _ = pipe.Exec(ctx)
	d.fillMu.Lock()
	defer d.fillMu.Unlock()
	for i, cmd := range cmds {
		if err := cmd.Err(); err == nil {
			d.fillAck.add(from + int64(i))
		} else {
			d.countOOM(err)
		}
	}
}

// Burst writes past maxmemory on a noeviction master until the master rejects
// writes, continues for hold, then deletes what it wrote. If it did not start,
// it returns why.
func (d *Data) Burst(ctx context.Context, hold time.Duration) (string, error) {
	mi, err := memoryInfo(ctx, d.client)
	if err != nil {
		return "", err
	}
	if mi.policy != "noeviction" || mi.maxMemory == 0 {
		return fmt.Sprintf("maxmemory %d, policy %s", mi.maxMemory, mi.policy), nil
	}
	f := d.cfg.Fill
	lim := rate.NewLimiter(rate.Limit(4*f.KeysPerSecond), f.Batch)
	// Bounded in case writes are never rejected.
	most := 2 * mi.maxMemory / int64(f.ValueBytes)
	var n, written, rejected int64
	var full time.Time
	for n < most && (full.IsZero() || time.Since(full) < hold) {
		b := int64(f.Batch)
		if !full.IsZero() {
			b = 1
			if !sleep(ctx, 100*time.Millisecond) {
				break
			}
		} else if lim.WaitN(ctx, int(b)) != nil {
			break
		}
		ok, oom := d.writeBurst(ctx, n, b)
		n += b
		written += ok
		rejected += oom
		if oom > 0 && full.IsZero() {
			full = time.Now()
			d.log.Info("burst rejected", "used_memory_before", mi.used, "maxmemory", mi.maxMemory, "keys", written)
		}
	}
	for !d.deleteRange(ctx, n) {
		if !sleep(ctx, time.Second) {
			return "", ctx.Err()
		}
	}
	d.log.Info("burst done", "keys_written", written, "oom_rejections", rejected, "full_seconds", sinceOrZero(full))
	if rejected == 0 {
		return "", fmt.Errorf("no write was rejected after %d keys", written)
	}
	return "", nil
}

func sinceOrZero(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return time.Since(t).Seconds()
}

func (d *Data) writeBurst(ctx context.Context, from, n int64) (ok, oom int64) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	pipe := d.client.Pipeline()
	cmds := make([]*redis.StatusCmd, n)
	for i := range n {
		key := BurstKey(d.in.Name, from+i)
		cmds[i] = pipe.Set(ctx, key, Value(key, d.cfg.Fill.ValueBytes), 0)
	}
	_, _ = pipe.Exec(ctx)
	for _, cmd := range cmds {
		err := cmd.Err()
		switch {
		case err == nil:
			ok++
		case prober.Classify(err) == prober.ResultOOM:
			oom++
		}
		d.countOOM(err)
	}
	return ok, oom
}

// deleteRange deletes the burst keys below n.
func (d *Data) deleteRange(ctx context.Context, n int64) bool {
	for lo := int64(0); lo < n; lo += chunk {
		keys := make([]string, 0, chunk)
		for i := lo; i < min(lo+chunk, n); i++ {
			keys = append(keys, BurstKey(d.in.Name, i))
		}
		if err := d.client.Unlink(ctx, keys...).Err(); err != nil {
			d.log.Warn("deleting burst keys", "error", err.Error())
			return false
		}
	}
	return true
}

// Writable writes and deletes a key through rfrm.
func (d *Data) Writable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	key := "soak:" + d.in.Name + ":burst:writable"
	if err := d.client.Set(ctx, key, "1", 0).Err(); err != nil {
		return fmt.Errorf("writes not accepted: %w", err)
	}
	return d.client.Unlink(ctx, key).Err()
}

type memInfo struct {
	used, maxMemory int64
	policy          string
}

func memoryInfo(ctx context.Context, c *redis.Client) (memInfo, error) {
	s, err := c.Info(ctx, "memory").Result()
	if err != nil {
		return memInfo{}, err
	}
	i := observer.ParseInfo(s)
	mi := memInfo{policy: i["maxmemory_policy"]}
	mi.used, _ = strconv.ParseInt(i["used_memory"], 10, 64)
	mi.maxMemory, _ = strconv.ParseInt(i["maxmemory"], 10, 64)
	return mi, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
