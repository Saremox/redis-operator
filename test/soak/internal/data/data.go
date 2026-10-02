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

	oom      prometheus.Counter
	lost     *prometheus.CounterVec
	verified *prometheus.CounterVec
}

type request struct {
	ctx   context.Context
	event string
	step  int
	done  chan struct{}
	lost  int
	err   error
}

func New(in config.Instance, cfg *config.Config, master Master, a *auth.Source, m *metrics.Metrics, log *slog.Logger) *Data {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	h := fnv.New64a()
	_, _ = h.Write([]byte(in.Namespace + "/" + in.Name))
	d := &Data{
		in:       in,
		cfg:      *in.Data,
		timeout:  cfg.Probe.Timeout.Duration,
		client:   prober.MasterService(in, cfg.Probe.Timeout.Duration, a).NewClient(prober.Pooled),
		master:   master,
		auth:     a,
		rnd:      rand.New(rand.NewPCG(uint64(cfg.Mutation.Seed), h.Sum64())),
		log:      log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
		requests: make(chan *request),
		oom:      m.OOMRejections.With(labels),
		lost:     m.LostWrites.MustCurryWith(labels),
		verified: m.LedgerVerified.MustCurryWith(labels),
	}
	if in.Data.Ledger != nil {
		d.ledger = newLedger()
	}
	initEvents(d.lost, cfg, in)
	return d
}

// initEvents creates the lost_writes_total series of every event at 0:
// alerts take their increase, which a series that first appears with the
// writes a verification lost wouldn't show.
func initEvents(lost *prometheus.CounterVec, cfg *config.Config, in config.Instance) {
	for _, e := range cfg.Events(in) {
		lost.WithLabelValues(e)
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

// Verify verifies the data after a mutation, at the mutator's step, and
// ends the mutation. event is the mutation's kind, or config.EventReset for
// one that loses the data by design. It returns the writes found lost, or
// an error if the data couldn't be verified before ctx was done.
func (d *Data) Verify(ctx context.Context, event string, step int) (int, error) {
	return verifyAfter(ctx, d.requests, event, step)
}

func verifyAfter(ctx context.Context, requests chan<- *request, event string, step int) (int, error) {
	r := &request{ctx: ctx, event: event, step: step, done: make(chan struct{})}
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
			r.lost, r.err = d.verifyRetrying(r.ctx, r.event, r.step)
			d.mutating.Store(false)
			close(r.done)
		case event := <-d.master.Failovers():
			d.failedOver.Store(true)
			if !d.mutating.Load() {
				_, _ = d.verifyRetrying(ctx, event, 0)
			}
		case <-periodic:
			if !d.mutating.Load() {
				_, _ = d.verifyRetrying(ctx, config.EventPeriodic, 0)
			}
		}
	}
}

// verifyRetrying verifies until it succeeds or ctx is done.
func (d *Data) verifyRetrying(ctx context.Context, event string, step int) (int, error) {
	for attempt := 1; ; attempt++ {
		lost, err := d.verify(ctx, event, step)
		if err == nil || ctx.Err() != nil {
			return lost, err
		}
		if attempt%10 == 1 {
			d.log.Warn("verifying the data", "event", event, "step", step, "attempt", attempt, "error", err.Error())
		}
		if !sleep(ctx, 2*time.Second) {
			return 0, ctx.Err()
		}
	}
}

var errNoMaster = errors.New("no single master")

// verify checks the writes acknowledged since the previous verification,
// a sample of older ones and a sample of fill keys on the master.
func (d *Data) verify(ctx context.Context, event string, step int) (int, error) {
	addr := d.master.MasterAddr()
	if addr == "" {
		return 0, errNoMaster
	}
	start := time.Now()
	c := d.podClient(addr, d.auth)
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
		// A sample that lost writes lost an unknown share of the older
		// ones: count them all now, so later events don't inherit them.
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
		// The fill is only sampled: forget what the sample can't tell
		// about, so later events don't inherit this one's losses.
		d.fillMu.Lock()
		d.fillAck.dropBelow(fillTo)
		d.fillMu.Unlock()
	}

	lost := len(lostRecent) + len(lostOlder) + len(lostFill)
	d.lost.WithLabelValues(event).Add(float64(lost))
	d.verified.WithLabelValues(event).Inc()
	log := d.log.With("event", event, "step", step, "failover", d.failedOver.Swap(false),
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

// podClient connects to a pod directly, for verifications.
func (d *Data) podClient(addr string, a *auth.Source) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  addr,
		CredentialsProvider:   a.Provider(),
		DialTimeout:           d.timeout,
		ReadTimeout:           5 * d.timeout,
		WriteTimeout:          5 * d.timeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
	})
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

// Burst writes past maxmemory on a noeviction master until writes are
// rejected, keeps trying for hold, then deletes what it wrote. It returns
// why it didn't start, if it didn't.
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
	var mi memInfo
	for line := range strings.Lines(s) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), ":")
		switch k {
		case "used_memory":
			mi.used, _ = strconv.ParseInt(v, 10, 64)
		case "maxmemory":
			mi.maxMemory, _ = strconv.ParseInt(v, 10, 64)
		case "maxmemory_policy":
			mi.policy = v
		}
	}
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
