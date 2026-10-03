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

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// Client is how a prober holds its connections.
type Client string

const (
	// Pooled keeps one long-lived client, like an application. New
	// connections use the Secret's current password; open ones stay
	// authenticated as they were.
	Pooled Client = "pooled"
	// Retrying is Pooled with the default go-redis retries, as an application
	// with an untuned client.
	Retrying Client = "retrying"
	// Fresh dials a new connection for every probe, like a new pod.
	Fresh Client = "fresh"
	// Follower is Pooled, but replaces its client when the Secret changes, as
	// an application that reads its Secret again. Its auth failures show how
	// long the operator takes to apply a password change.
	Follower Client = "follower"
)

// EventNone is the event of an outage that started while neither a
// mutation of its instance nor a chaos action ran.
const EventNone = "none"

// Clients are the client styles every path is probed with.
var Clients = []Client{Pooled, Retrying, Fresh, Follower}

type Prober struct {
	path     Path
	client   Client
	auth     *auth.Source
	interval time.Duration
	timeout  time.Duration
	// A successful SET whose sequence number is a multiple of waitEvery is
	// followed by WAIT 1 waitTimeout.
	waitEvery   int64
	waitTimeout time.Duration
	key         string
	seq         int64
	outage      outage
	// event returns what runs now: a chaos kind, the instance's mutation
	// kind, or EventNone.
	event func() string
	log   *slog.Logger

	total          *prometheus.CounterVec
	duration       prometheus.ObserverVec
	lastSuccess    *prometheus.GaugeVec
	writable       prometheus.Gauge
	readable       prometheus.Gauge
	outageDuration prometheus.ObserverVec
	waitAcked      prometheus.Gauge
}

func New(in config.Instance, path Path, client Client, probe config.Probe, a *auth.Source, m *metrics.Metrics, log *slog.Logger) *Prober {
	labels := prometheus.Labels{
		"rf":        in.Name,
		"namespace": in.Namespace,
		"mode":      string(in.Mode),
		"path":      path.Name,
		"client":    string(client),
	}
	p := &Prober{
		path:        path,
		client:      client,
		auth:        a,
		interval:    probe.Interval.Duration,
		timeout:     probe.Timeout.Duration,
		waitEvery:   int64(probe.WaitEvery),
		waitTimeout: probe.WaitTimeout.Duration,
		key:         fmt.Sprintf("soak:%s:%s:%s:seq", in.Name, path.Name, client),
		event:       func() string { return EventNone },
		log: log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode,
			"path", path.Name, "client", client),
		total:          m.ProbeTotal.MustCurryWith(labels),
		duration:       m.ProbeDuration.MustCurryWith(labels),
		lastSuccess:    m.LastSuccess.MustCurryWith(labels),
		readable:       m.Readable.With(labels),
		outageDuration: m.OutageDuration.MustCurryWith(labels),
		waitAcked:      m.WaitAckedReplicas.WithLabelValues(in.Name, in.Namespace, string(in.Mode)),
	}
	if path.ReadKey == "" {
		p.writable = m.Writable.With(labels)
	}
	return p
}

func (p *Prober) Run(ctx context.Context) {
	var pooled *redis.Client
	var password string
	defer func() {
		if pooled != nil {
			_ = pooled.Close()
		}
	}()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		switch p.client {
		case Fresh:
			c := p.path.NewClient(Fresh)
			p.probe(ctx, c)
			_ = c.Close()
			continue
		case Follower:
			if pw := p.auth.Password(); pooled != nil && pw != password {
				_ = pooled.Close()
				pooled = nil
				p.log.Info("follower switched password")
			}
		}
		if pooled == nil {
			password = p.auth.Password()
			pooled = p.path.NewClient(p.client)
		}
		p.probe(ctx, pooled)
	}
}

func (p *Prober) probe(ctx context.Context, c *redis.Client) {
	if p.path.ReadKey != "" {
		p.read(ctx, c)
		return
	}
	start := time.Now()
	p.seq++
	setErr := p.do(ctx, "set", func(ctx context.Context) error {
		return c.Set(ctx, p.key, p.seq, 0).Err()
	})
	if setErr == nil && p.seq%p.waitEvery == 0 {
		_ = p.do(ctx, "wait", func(ctx context.Context) error {
			n, err := c.Wait(ctx, 1, p.waitTimeout).Result()
			if err == nil {
				p.waitAcked.Set(float64(n))
			}
			return err
		})
	}
	getErr := p.get(ctx, c, p.key)
	p.writable.Set(gauge(setErr == nil))
	p.readable.Set(gauge(getErr == nil))
	p.result(start, cmp.Or(setErr, getErr))
}

// read only GETs the path's read key, which may not exist yet.
func (p *Prober) read(ctx context.Context, c *redis.Client) {
	start := time.Now()
	err := p.get(ctx, c, p.path.ReadKey)
	p.readable.Set(gauge(err == nil))
	p.result(start, err)
}

func (p *Prober) get(ctx context.Context, c *redis.Client, key string) error {
	return p.do(ctx, "get", func(ctx context.Context) error {
		err := c.Get(ctx, key).Err()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return err
	})
}

func (p *Prober) result(start time.Time, err error) {
	if err != nil {
		if p.outage.fail(start) {
			p.outage.event = p.event()
			p.log.Warn("outage started", "result", Classify(err), "event", p.outage.event, "error", err.Error())
		}
		return
	}
	event := p.outage.event
	if d, ended := p.outage.succeed(start); ended {
		p.outageDuration.WithLabelValues(event).Observe(d.Seconds())
		p.log.Info("outage ended", "event", event, "duration_seconds", d.Seconds())
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
