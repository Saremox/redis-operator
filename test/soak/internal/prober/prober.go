// Package prober checks that an instance's master is reachable, writable
// and readable, the way an application would.
package prober

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// Client is how a prober holds its connections.
type Client string

const (
	// Pooled keeps one long-lived client, like an application.
	Pooled Client = "pooled"
	// Retrying is Pooled with go-redis's default retries, like an
	// application that didn't tune its client.
	Retrying Client = "retrying"
	// Fresh dials a new connection for every probe, like a new pod.
	Fresh Client = "fresh"
)

type Prober struct {
	path     Path
	client   Client
	interval time.Duration
	timeout  time.Duration
	key      string
	seq      int64
	outage   outage
	log      *slog.Logger

	total          *prometheus.CounterVec
	duration       prometheus.ObserverVec
	lastSuccess    *prometheus.GaugeVec
	writable       prometheus.Gauge
	readable       prometheus.Gauge
	outageDuration prometheus.Observer
}

func New(in config.Instance, path Path, client Client, probe config.Probe, m *metrics.Metrics, log *slog.Logger) *Prober {
	labels := prometheus.Labels{
		"rf":        in.Name,
		"namespace": in.Namespace,
		"mode":      string(in.Mode),
		"path":      path.Name,
		"client":    string(client),
	}
	return &Prober{
		path:     path,
		client:   client,
		interval: probe.Interval.Duration,
		timeout:  probe.Timeout.Duration,
		key:      fmt.Sprintf("soak:%s:%s:%s:seq", in.Name, path.Name, client),
		log: log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode,
			"path", path.Name, "client", client),
		total:          m.ProbeTotal.MustCurryWith(labels),
		duration:       m.ProbeDuration.MustCurryWith(labels),
		lastSuccess:    m.LastSuccess.MustCurryWith(labels),
		writable:       m.Writable.With(labels),
		readable:       m.Readable.With(labels),
		outageDuration: m.OutageDuration.With(labels),
	}
}

func (p *Prober) Run(ctx context.Context) {
	var pooled *redis.Client
	if p.client != Fresh {
		pooled = p.path.NewClient(p.client)
		defer func() { _ = pooled.Close() }()
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c := pooled
		if c == nil {
			c = p.path.NewClient(Fresh)
		}
		p.probe(ctx, c)
		if pooled == nil {
			_ = c.Close()
		}
	}
}

func (p *Prober) probe(ctx context.Context, c *redis.Client) {
	start := time.Now()
	p.seq++
	setErr := p.do(ctx, "set", func(ctx context.Context) error {
		return c.Set(ctx, p.key, p.seq, 0).Err()
	})
	getErr := p.do(ctx, "get", func(ctx context.Context) error {
		err := c.Get(ctx, p.key).Err()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return err
	})
	p.writable.Set(gauge(setErr == nil))
	p.readable.Set(gauge(getErr == nil))

	if err := cmp.Or(setErr, getErr); err != nil {
		if p.outage.fail(start) {
			p.log.Warn("outage started", "result", Classify(err), "error", err.Error())
		}
		return
	}
	if d, ended := p.outage.succeed(start); ended {
		p.outageDuration.Observe(d.Seconds())
		p.log.Info("outage ended", "duration_seconds", d.Seconds())
	}
}

func (p *Prober) do(ctx context.Context, op string, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	start := time.Now()
	err := f(ctx)
	p.duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	p.total.WithLabelValues(op, Classify(err)).Inc()
	if err == nil {
		p.lastSuccess.WithLabelValues(op).SetToCurrentTime()
	}
	return err
}

func gauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
