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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// Pods lists an instance's redis pods.
type Pods interface {
	RedisAddrs() []observer.PodAddr
}

// catchUp is how long a pod may take to replicate what the source's master
// had when it was read.
const catchUp = 30 * time.Second

// position is where a replication stream is: its ID and offset.
type position struct {
	replID string
	offset int64
}

// Replica verifies a bootstrapping instance, which is read-only: it writes
// nothing, and checks that every pod holds what the source's master holds
// of the source's ledger.
type Replica struct {
	in       config.Instance
	cfg      config.Bootstrap
	source   *Data
	pods     Pods
	auth     *auth.Source
	rnd      *rand.Rand
	log      *slog.Logger
	requests chan *request

	lost     *prometheus.CounterVec
	verified *prometheus.CounterVec
}

func NewReplica(in config.Instance, cfg *config.Config, source *Data, pods Pods, a *auth.Source, m *metrics.Metrics, log *slog.Logger) *Replica {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	h := fnv.New64a()
	_, _ = h.Write([]byte(in.Namespace + "/" + in.Name))
	r := &Replica{
		in:       in,
		cfg:      *in.Bootstrap,
		source:   source,
		pods:     pods,
		auth:     a,
		rnd:      rand.New(rand.NewPCG(uint64(cfg.Mutation.Seed), h.Sum64())),
		log:      log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
		requests: make(chan *request),
		lost:     m.LostWrites.MustCurryWith(labels),
		verified: m.LedgerVerified.MustCurryWith(labels),
	}
	initEvents(r.lost, cfg, in)
	return r
}

func (r *Replica) Filled() bool { return true }
func (r *Replica) Begin()       {}
func (r *Replica) Refill()      {}

func (r *Replica) Verify(ctx context.Context, event string, step int) (int, error) {
	return verifyAfter(ctx, r.requests, event, step)
}

func (r *Replica) Burst(context.Context, time.Duration) (string, error) {
	return "read-only instance", nil
}

func (r *Replica) Writable(context.Context) error {
	return errors.New("read-only instance")
}

func (r *Replica) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.VerifyInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-r.requests:
			req.lost, req.err = r.verifyRetrying(req.ctx, req.event, req.step)
			close(req.done)
		case <-t.C:
			_, _ = r.verifyRetrying(ctx, config.EventPeriodic, 0)
		}
	}
}

func (r *Replica) verifyRetrying(ctx context.Context, event string, step int) (int, error) {
	for attempt := 1; ; attempt++ {
		lost, err := r.verify(ctx, event, step)
		if err == nil || ctx.Err() != nil {
			return lost, err
		}
		if attempt%10 == 1 {
			r.log.Warn("verifying the data", "event", event, "step", step, "attempt", attempt, "error", err.Error())
		}
		if !sleep(ctx, 2*time.Second) {
			return 0, ctx.Err()
		}
	}
}

// verify reads a sample of the source's recent ledger writes from the
// source's master, and then from every pod once it has replicated as far.
func (r *Replica) verify(ctx context.Context, event string, step int) (int, error) {
	start := time.Now()
	src := r.source
	seqs := src.ledger.recentSample(r.rnd, r.cfg.SampleKeys)
	if len(seqs) == 0 {
		return 0, errors.New("no source writes since its last verification yet")
	}
	addr := src.master.MasterAddr()
	if addr == "" {
		return 0, errNoMaster
	}
	c := src.podClient(addr, src.auth)
	defer func() { _ = c.Close() }()
	size := src.cfg.Ledger.ValueBytes
	gone, _, err := src.check(ctx, c, LedgerKey, size, seqs)
	if err != nil {
		return 0, err
	}
	// The source's own losses are counted on the source.
	present := slices.DeleteFunc(seqs, func(n int64) bool { return slices.Contains(gone, n) })
	info, err := replication(ctx, c)
	if err != nil {
		return 0, err
	}
	at := position{replID: info["master_replid"]}
	if at.offset, err = strconv.ParseInt(info["master_repl_offset"], 10, 64); err != nil {
		return 0, err
	}
	pods := r.pods.RedisAddrs()
	if len(pods) == 0 {
		return 0, errors.New("no redis pod")
	}
	lost := 0
	for _, p := range pods {
		n, err := r.verifyPod(ctx, p, present, at)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p.Name, err)
		}
		lost += n
	}
	r.lost.WithLabelValues(event).Add(float64(lost))
	r.verified.WithLabelValues(event).Inc()
	log := r.log.With("event", event, "step", step, "source", src.in.Name, "keys", len(present),
		"pods", len(pods), "lost", lost, "duration_seconds", time.Since(start).Seconds())
	if lost > 0 {
		log.Warn("lost writes")
	}
	log.Info("data verified")
	return lost, nil
}

func (r *Replica) verifyPod(ctx context.Context, p observer.PodAddr, seqs []int64, at position) (int, error) {
	c := r.source.podClient(p.Addr, r.auth)
	defer func() { _ = c.Close() }()
	deadline := time.Now().Add(catchUp)
	for {
		info, err := replication(ctx, c)
		if err != nil {
			return 0, err
		}
		if caughtUp(info, at) {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("replicates %s up to %s with link %s, the source's master was at %s:%d %s before",
				info["master_replid"], info["slave_repl_offset"], info["master_link_status"], at.replID, at.offset, catchUp)
		}
		if !sleep(ctx, 200*time.Millisecond) {
			return 0, ctx.Err()
		}
	}
	lost, _, err := r.source.check(ctx, c, LedgerKey, r.source.cfg.Ledger.ValueBytes, seqs)
	if len(lost) > 0 {
		r.log.Warn("source writes missing on a pod", "pod", p.Name, "lost", len(lost), "lost_seqs", format(spansOf(lost)))
	}
	return len(lost), err
}

// caughtUp reports whether INFO replication of a replica shows that it
// replicated as far as at. Offsets of different streams are not comparable:
// after a new source master starts, a replica that did not sync with it yet
// can be far ahead in the old stream.
func caughtUp(info map[string]string, at position) bool {
	offset, err := strconv.ParseInt(info["slave_repl_offset"], 10, 64)
	return err == nil && info["master_link_status"] == "up" && info["master_replid"] == at.replID && offset >= at.offset
}

func replication(ctx context.Context, c *redis.Client) (map[string]string, error) {
	s, err := c.Info(ctx, "replication").Result()
	if err != nil {
		return nil, err
	}
	info := map[string]string{}
	for line := range strings.Lines(s) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			info[k] = v
		}
	}
	return info, nil
}
