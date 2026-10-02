package prober

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// Runner runs the probers of an instance's current paths: it starts a
// path's probers when the path appears, and stops them and deletes the
// path's series when it goes away.
type Runner struct {
	in    config.Instance
	probe config.Probe
	auth  *auth.Source
	m     *metrics.Metrics
	// base is the probers' logger, log the runner's.
	base    *slog.Logger
	log     *slog.Logger
	newPath func(string) Path
	event   func() string

	running map[string]func()
}

func NewRunner(in config.Instance, probe config.Probe, a *auth.Source, m *metrics.Metrics, log *slog.Logger) *Runner {
	return &Runner{
		in:    in,
		probe: probe,
		auth:  a,
		m:     m,
		base:  log,
		log:   log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
		newPath: func(name string) Path {
			return NewPath(name, in, probe.Timeout.Duration, a)
		},
		running: map[string]func(){},
		event:   func() string { return EventNone },
	}
}

// SetEvent sets what tells the probers' outages what ran when they
// started: a chaos kind, the instance's mutation kind, or "" for none.
func (r *Runner) SetEvent(event func() string) {
	r.event = func() string {
		if e := event(); e != "" {
			return e
		}
		return EventNone
	}
}

// Run follows paths, the names of the instance's current paths, every
// probe interval.
func (r *Runner) Run(ctx context.Context, paths func() []string) {
	defer r.sync(ctx, nil)
	t := time.NewTicker(r.probe.Interval.Duration)
	defer t.Stop()
	for {
		r.sync(ctx, paths())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Runner) sync(ctx context.Context, names []string) {
	for name, stop := range r.running {
		if !slices.Contains(names, name) {
			stop()
			delete(r.running, name)
			r.m.DeletePath(r.in.Name, r.in.Namespace, name)
			r.log.Info("path removed", "path", name)
		}
	}
	for _, name := range names {
		if _, ok := r.running[name]; ok {
			continue
		}
		path := r.newPath(name)
		pctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for _, c := range Clients {
			p := New(r.in, path, c, r.probe, r.auth, r.m, r.base)
			p.event = r.event
			wg.Go(func() { p.Run(pctx) })
		}
		r.running[name] = func() {
			cancel()
			wg.Wait()
		}
		r.log.Info("path added", "path", name)
	}
}
