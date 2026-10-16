// Package mutator keeps changing an instance, one mutation at a time, and
// measures how the operator converges after each.
package mutator

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/autoscale"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/instances"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
	"github.com/saremox/redis-operator/test/soak/internal/poll"
)

// Results, the values of the result label.
const (
	resultConverged = "converged"
	resultTimeout   = "timeout"
	resultRejected  = "rejected"
	resultSkipped   = "skipped"
	// resultStuckRestored is an auth mutation that timed out, because the
	// operator could not apply the password. It converged after the tester
	// put the previous password back.
	resultStuckRestored = "stuck_restored"
)

// holdHandle is what a mutation needs of its window: the observer's Hold.
type holdHandle interface {
	Applied()
	Done() <-chan bool
}

// watcher is what the mutator needs of the observer of its instance.
type watcher interface {
	Quiet() bool
	Master() string
	MasterAddr() string
	Report() observer.Report
}

// Data is what the mutator needs of an instance's data.
type Data interface {
	Filled() bool
	// Refill makes Filled false until the recreated instance is filled.
	Refill()
	// Begin and Verify bracket a mutation: Verify checks the data after
	// it, and returns the writes it lost. event is the mutation's kind, or
	// config.EventReset.
	Begin()
	Verify(ctx context.Context, event string, step int, lossless bool) (int, error)
	Burst(ctx context.Context, hold time.Duration) (string, error)
	Writable(ctx context.Context) error
}

// Mutator changes one instance, one mutation at a time.
type Mutator struct {
	in       config.Instance
	cfg      config.Mutation
	operator config.Operator
	timeout  time.Duration
	// converge bounds each step of a mutation that waits on the operator
	// itself.
	convergeTimeout time.Duration
	observerCfg     config.Observer
	// sentinelPort is the port of the Sentinels.
	sentinelPort int
	// versions holds the version catalogue and the transition graph.
	versions *config.Config
	kube     kubernetes.Interface
	rfs      versioned.Interface
	observer watcher
	// logs reads the log of a pod.
	logs func(ctx context.Context, pod string, opts *corev1.PodLogOptions) ([]byte, error)
	// grace is how long a stuck change that the mutator adopts must hold.
	grace time.Duration
	// hold opens the convergence window of a mutation in the observer.
	hold func(time.Duration, func(context.Context) error) holdHandle
	data Data
	auth *auth.Source
	lock *global.Lock
	// instance recreates the instance from its template, nil without one.
	instance *instances.Instance
	log      *slog.Logger

	// resets counts the resets, which take a chain's starts in turn;
	// mixed is the version change whose mixed window is open, and
	// resetWhy why the next mutation is a reset.
	resets   int
	mixed    *mixedWindow
	resetWhy string
	// current is the kind of the mutation running, "" between mutations,
	// and allowed what it may do to the redis pods.
	current atomic.Pointer[config.Kind]
	allowed atomic.Pointer[autoscale.Allow]

	total        *prometheus.CounterVec
	converge     prometheus.ObserverVec
	recreated    *prometheus.CounterVec
	inProgress   *prometheus.GaugeVec
	transitions  *prometheus.CounterVec
	mixedSeconds prometheus.ObserverVec
	findings     *prometheus.CounterVec
	recoveries   *prometheus.CounterVec
	stalled      prometheus.Gauge
}

// New returns the mutator of an instance; data is nil for an instance
// without data, inst nil for one without a template.
func New(in config.Instance, cfg *config.Config, kube kubernetes.Interface, rfs versioned.Interface, o *observer.Observer, data Data, a *auth.Source, lock *global.Lock, inst *instances.Instance, m *metrics.Metrics, log *slog.Logger) *Mutator {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	mu := &Mutator{
		in:              in,
		cfg:             cfg.Mutation,
		operator:        cfg.Operator,
		timeout:         cfg.Probe.Timeout.Duration,
		convergeTimeout: cfg.Observer.ConvergenceTimeout.Duration,
		observerCfg:     cfg.Observer,
		sentinelPort:    sentinelPort,
		versions:        cfg,
		kube:            kube,
		rfs:             rfs,
		observer:        o,
		hold:            func(d time.Duration, f func(context.Context) error) holdHandle { return o.Hold(d, f) },
		data:            data,
		auth:            a,
		lock:            lock,
		instance:        inst,
		log:             log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode, "seed", cfg.Mutation.Seed),
		total:           m.MutationTotal.MustCurryWith(labels),
		converge:        m.MutationConverge.MustCurryWith(labels),
		recreated:       m.PodsRecreated.MustCurryWith(labels),
		inProgress:      m.MutationInProgress.MustCurryWith(labels),
		transitions:     m.VersionTransition.MustCurryWith(labels),
		mixedSeconds:    m.VersionMixed.MustCurryWith(labels),
		findings:        m.Findings.MustCurryWith(labels),
		recoveries:      m.NoMasterRecovery.MustCurryWith(labels),
		stalled:         m.MutationStalled.With(labels),
		grace:           stuckGrace,
	}
	mu.logs = func(ctx context.Context, pod string, opts *corev1.PodLogOptions) ([]byte, error) {
		return kube.CoreV1().Pods(in.Namespace).GetLogs(pod, opts).DoRaw(ctx)
	}
	// Alerts use the increase of mutation_total, which does not show a series
	// that starts at 1.
	for _, k := range in.MutationKinds() {
		mu.inProgress.WithLabelValues(string(k)).Set(0)
		if k == config.SentinelResetKillMaster {
			for _, path := range recoveryPaths {
				mu.recoveries.WithLabelValues(path)
			}
		}
		for _, r := range []string{resultConverged, resultTimeout, resultRejected, resultSkipped} {
			mu.total.WithLabelValues(string(k), r)
		}
		if restores(k) {
			mu.total.WithLabelValues(string(k), resultStuckRestored)
		}
	}
	if in.Chain != nil {
		mu.findings.WithLabelValues(invVersionTransition)
	}
	if inst != nil {
		mu.findings.WithLabelValues(invResetIncomplete)
	}
	return mu
}

// Current returns the kind of the mutation running, "" if none is.
func (m *Mutator) Current() string {
	if k := m.current.Load(); k != nil {
		return string(*k)
	}
	return ""
}

// Allowed returns what the running mutation may do to the redis pods. ok is
// false while no mutation runs. A kind without its own rule may do all of it.
func (m *Mutator) Allowed() (a autoscale.Allow, ok bool) {
	if m.current.Load() == nil {
		return autoscale.Allow{}, false
	}
	if a := m.allowed.Load(); a != nil {
		return *a, true
	}
	return autoscale.AllowAll, true
}

// Run waits for the data fill, then runs one mutation at a time until ctx is
// done or mutation.stopAfter passes.
func (m *Mutator) Run(ctx context.Context) {
	started := time.Now()
	stop := started.Add(m.cfg.StopAfter.Duration)
	if !m.waitFilled(ctx) {
		return
	}
	m.adopt(ctx, started)
	for step := 1; ; step++ {
		if !m.waitQuiet(ctx) {
			return
		}
		if m.cfg.StopAfter.Duration > 0 && time.Now().After(stop) {
			m.log.Info("mutator stopped", "steps", step-1)
			return
		}
		r := stepRand(m.cfg.Seed, m.in, step)
		kind := pickKind(r, m.in.Mutations)
		unlock := m.lock.Shared()
		if config.Exclusive(kind) {
			unlock()
			m.log.Info("waiting for every other mutation to finish", "kind", kind, "step", step)
			unlock = m.lock.Exclusive()
		}
		// Another instance's exclusive mutation may have run meanwhile.
		if !m.observer.Quiet() {
			unlock()
			step--
			continue
		}
		next := m.mutate(ctx, step, r, kind)
		// Reset a version change that did not converge immediately, while its
		// window is still held.
		for next != "" && ctx.Err() == nil {
			step++
			next = m.mutate(ctx, step, stepRand(m.cfg.Seed, m.in, step), next)
		}
		unlock()
		d := m.cfg.Interval.Duration + time.Duration(r.Int64N(int64(m.cfg.Jitter.Duration)+1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// waitFilled waits until the data reached its target once, so mutations
// act on a filled instance.
func (m *Mutator) waitFilled(ctx context.Context) bool {
	if m.data != nil && !waitFor(ctx, m.data.Filled) {
		return false
	}
	m.log.Info("data filled, mutating")
	return true
}

// waitQuiet waits until no convergence window is open and every invariant
// holds. Its gauge shows how long it waits.
func (m *Mutator) waitQuiet(ctx context.Context) bool {
	start := time.Now()
	defer m.stalled.Set(0)
	return waitFor(ctx, func() bool {
		if m.observer.Quiet() {
			return true
		}
		m.stalled.Set(time.Since(start).Seconds())
		return false
	})
}

// waitFor checks ok every second, and reports whether it held before ctx
// was done.
func waitFor(ctx context.Context, ok func() bool) bool {
	return poll.Until(ctx, time.Second, 0, func(context.Context) error {
		if !ok() {
			return errors.New("not yet")
		}
		return nil
	}) == nil
}

// expectation is a mutation step whose expected result did not occur within
// its limit: the mutation timed out.
type expectation struct{ error }

// mutate applies one mutation and waits until it converged. It returns
// config.Reset after a version change that did not converge, because the
// instance must be reset next.
func (m *Mutator) mutate(ctx context.Context, step int, r *rand.Rand, kind config.Kind) config.Kind {
	s, err := m.fetch(ctx, fetchOpts{})
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("reading the instance for a mutation", "step", step, "error", err.Error())
		}
		return ""
	}
	p := m.plan(r, kind, s)
	kind = p.kind
	log := m.log.With("kind", kind, "step", step)
	if p.skip != "" {
		m.total.WithLabelValues(string(kind), resultSkipped).Inc()
		log.Info("mutation skipped", "result", resultSkipped, "reason", p.skip)
		return ""
	}
	return m.execute(ctx, step, s, p, log)
}

// execute applies the plan p of a mutation and waits until it converged. It
// returns what mutate returns.
func (m *Mutator) execute(ctx context.Context, step int, s state, p plan, log *slog.Logger) config.Kind {
	kind := p.kind
	timeout := m.cfg.Timeout(kind, m.observerCfg, s.rf.Spec.Redis.Replicas)
	log = log.With("params", p.params, "redis_replicas", s.rf.Spec.Redis.Replicas)
	before := uids(s.redis)

	inProgress := m.inProgress.WithLabelValues(string(kind))
	inProgress.Set(1)
	m.allowed.Store(p.allow)
	m.current.Store(&kind)
	defer func() {
		inProgress.Set(0)
		m.current.Store(nil)
		m.allowed.Store(nil)
	}()
	bound, hold := m.window(p, timeout)
	// A rejected mutation changed nothing, so its window only waits for
	// the invariants.
	var applied, rejected atomic.Bool
	h := m.hold(hold, m.windowCheck(p, &applied, &rejected))
	if m.data != nil {
		m.data.Begin()
	}
	log.Info("mutating", "timeout_seconds", timeout.Seconds())
	start := time.Now()
	applyErr := m.apply(ctx, p, log)
	if applyErr != nil {
		rejected.Store(true)
	}
	applied.Store(true)
	h.Applied()
	appliedAt := time.Now()
	var watch *transition
	if bound > 0 {
		watch = p.edge
	}
	converged, ok := m.await(ctx, h, bound, watch)
	if !ok {
		return ""
	}
	restored := false
	if !converged && applyErr == nil && p.restore != nil {
		if restored, ok = m.restorePassword(ctx, p.restore, timeout, log); !ok {
			return ""
		}
	}
	d := time.Since(start)
	if kind == config.Reset && !converged && time.Since(appliedAt) >= bound {
		m.findings.WithLabelValues(invResetIncomplete).Inc()
		log.Warn("reset did not complete", "finding", true, "bound_seconds", bound.Seconds())
	}

	var result string
	result, log = m.result(p, applyErr, converged, restored, d, appliedAt, log)
	log = m.recordRecovery(ctx, p, result, log)
	if !judged(p.edge, applyErr, converged) {
		m.total.WithLabelValues(string(kind), result).Inc()
	}

	recreated := m.recordRecreated(ctx, kind, before)
	level := slog.LevelInfo
	if result != resultConverged {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "mutation done", "result", result, "duration_seconds", d.Seconds(), "pods_recreated", recreated)
	return m.verifyAndJudge(ctx, step, s, p, applyErr, converged, log)
}

// window opens the mixed window of a version change, and returns how long
// execute waits for the mutation, 0 without a bound, and how long the
// convergence window holds.
func (m *Mutator) window(p plan, timeout time.Duration) (bound, hold time.Duration) {
	// An edge that may fail is observed for its timeout only, or until it
	// is stuck, and its window held on, for the reset that follows.
	hold = timeout
	if p.flip != nil {
		m.mixed = &mixedWindow{t: p.flip}
	}
	if p.edge != nil {
		m.mixed = &mixedWindow{t: p.edge}
		if p.edge.edge.Expect != config.ExpectOK {
			bound, hold = timeout, 2*timeout
		}
	}
	// A reset that failed can leave no RedisFailover. Then the observer
	// does not end the window.
	if p.kind == config.Reset {
		bound = 2 * timeout
	}
	return bound, hold
}

// windowCheck returns the convergence check of the window of the plan p.
func (m *Mutator) windowCheck(p plan, applied, rejected *atomic.Bool) func(context.Context) error {
	return func(ctx context.Context) error {
		switch {
		case rejected.Load():
			return nil
		case !applied.Load():
			return errors.New("still being applied")
		}
		if p.probe != nil {
			if err := p.probe(ctx); err != nil {
				return err
			}
		}
		s, err := m.fetch(ctx, p.fetch)
		if err != nil {
			return err
		}
		return p.converged(s)
	}
}

// result returns the result of a mutation, and log with its reason or
// error. It records the convergence time of a converged mutation.
func (m *Mutator) result(p plan, applyErr error, converged, restored bool, d time.Duration, appliedAt time.Time, log *slog.Logger) (string, *slog.Logger) {
	result := resultTimeout
	var exp expectation
	var na notAppliedError
	switch {
	case errors.As(applyErr, &na):
		result = resultSkipped
		log = log.With("reason", applyErr.Error())
	case errors.As(applyErr, &exp):
		log = log.With("error", applyErr.Error())
	case applyErr != nil:
		result = resultRejected
		log = log.With("error", applyErr.Error())
	case converged:
		result = resultConverged
		m.converge.WithLabelValues(string(p.kind)).Observe(d.Seconds())
		if p.offline != nil {
			log.Info("scenario phase done", "phase", p.offline.phases+1, "phase_name", "expect the rotation to converge",
				"duration_seconds", time.Since(appliedAt).Seconds())
		}
	case restored:
		result = resultStuckRestored
	}
	return result, log
}

// recordRecreated counts the redis pods that the mutation replaced, -1 if
// the instance cannot be read.
func (m *Mutator) recordRecreated(ctx context.Context, kind config.Kind, before map[string]types.UID) int {
	recreated := -1
	if s, err := m.fetch(ctx, fetchOpts{}); err == nil {
		recreated = countRecreated(before, s.redis)
		m.recreated.WithLabelValues(string(kind)).Add(float64(recreated))
	}
	return recreated
}

// verifyAndJudge verifies the data after the mutation, and judges a version
// change. It returns config.Reset if the instance must be reset next.
func (m *Mutator) verifyAndJudge(ctx context.Context, step int, s state, p plan, applyErr error, converged bool, log *slog.Logger) config.Kind {
	event := string(p.kind)
	if p.reset {
		event = config.EventReset
	}
	vctx, cancel := context.WithTimeout(ctx, verifyBound)
	lost, verr := m.verify(vctx, event, step, m.lossless(p, s))
	cancel()
	if p.edge == nil || applyErr != nil {
		if verr != nil && ctx.Err() == nil {
			log.Warn("data not verified", "error", verr.Error())
		}
		return ""
	}
	if m.judge(ctx, p.edge, converged, lost, verr, log) != transitionOK && m.instance != nil {
		m.resetWhy = "after " + p.edge.edge.String() + " didn't converge"
		return config.Reset
	}
	return ""
}

// restores reports whether planAuth can plan a restore for a kind.
func restores(k config.Kind) bool {
	return k == config.PasswordRotate || k == config.AuthRemove || k == config.PasswordRotateOffline
}

// judged reports whether judge counts a version change in place of its
// mutation result. A change that can fail does not converge by design, so it
// is not a timeout.
func judged(edge *transition, applyErr error, converged bool) bool {
	return edge != nil && applyErr == nil && !converged
}

// verifyBound bounds the verification after a mutation, as a mutation may
// leave the instance without a master. The mutator holds the global lock
// during the verification.
const verifyBound = 2 * time.Minute

func (m *Mutator) verify(ctx context.Context, event string, step int, lossless bool) (int, error) {
	if m.data == nil {
		return 0, nil
	}
	return m.data.Verify(ctx, event, step, lossless)
}

// lossless reports whether a mutation must lose no acknowledged write. Auth
// and Sentinel changes stop each redis pod gracefully at most, and Redis waits
// for its replicas on SIGTERM. A graceful master kill, a scale-down or a
// rollover along an ok edge also keep the data on a volume. A forced kill and
// a failover can lose writes to asynchronous replication. So can
// sentinel_reset_kill_master. If its shutdown script gets no failover, the
// master accepts writes until Redis gets SIGTERM. Redis 7 and later then wait
// for the replicas only up to shutdown-timeout. Redis 6.2 does not wait, so
// each kind that stops a redis pod is lossless only on a server that waits.
// A Sentinel image flip stops no redis pod.
func (m *Mutator) lossless(p plan, s state) bool {
	volumes := s.rf.Spec.Redis.Storage.PersistentVolumeClaim != nil
	waits := m.versions.WaitsForReplicas(s.rf.Spec.Redis.Image)
	switch p.kind {
	case config.SentinelImageFlip:
		return true
	case config.PasswordRotate, config.AuthAdd, config.AuthRemove, config.PasswordRotateOffline,
		config.PasswordRotateOfflineForget, config.SentinelToggle:
		return waits
	case config.KillMaster, config.RedisReplicas:
		return volumes && waits
	case config.ImageUpgrade:
		return volumes && waits && p.edge != nil && p.edge.edge.Expect == config.ExpectOK
	case config.AutoscaleKillMaster:
		return volumes && waits
	case config.AutoscaleLoad, config.AutoscaleLimit, config.AutoscaleMemoryMax, config.AutoscaleMode,
		config.AutoscaleStateLost, config.AutoscaleKillReplica:
		// A user path that may not recreate a pod stops none.
		return p.allow != nil && !p.allow.Recreate
	case config.SentinelReplicas, config.RedisResources, config.KillMasterForce, config.KillReplica,
		config.KillSentinel, config.RedisMemory, config.MaxMemoryPolicy, config.MaxMemoryPercent,
		config.FillBurst, config.SentinelImageUpgrade, config.Reset, config.SentinelResetKillMaster:
		return false
	}
	return false
}

func (m *Mutator) plan(r *rand.Rand, kind config.Kind, s state) plan {
	switch kind {
	case config.ImageUpgrade:
		return m.planImage(r, s)
	case config.SentinelImageUpgrade:
		return m.planSentinelImage(r, s)
	case config.SentinelImageFlip:
		return m.planSentinelFlip(r, s)
	case config.SentinelResetKillMaster:
		return m.planNoMaster(s, m.observer.Master(), m.sentinelPort)
	case config.AutoscaleLoad, config.AutoscaleLimit, config.AutoscaleMemoryMax, config.AutoscaleMode,
		config.AutoscaleStateLost, config.AutoscaleKillMaster, config.AutoscaleKillReplica:
		return m.planAutoscale(r, kind, s)
	case config.Reset:
		why := "picked"
		if m.resetWhy != "" {
			why, m.resetWhy = m.resetWhy, ""
		}
		return m.planReset(s, why)
	case config.RedisReplicas, config.SentinelReplicas, config.RedisResources, config.KillMaster,
		config.KillMasterForce, config.KillReplica, config.KillSentinel, config.RedisMemory,
		config.MaxMemoryPolicy, config.MaxMemoryPercent, config.FillBurst, config.PasswordRotate,
		config.AuthAdd, config.AuthRemove, config.SentinelToggle, config.PasswordRotateOffline,
		config.PasswordRotateOfflineForget:
		// newPlan plans these kinds.
	}
	return newPlan(r, kind, m.in.Mutations, s, m.observer.Master(), m.data)
}

// stuckGrace is how long the mutator still observes a change along an edge
// that can fail, after a pod on the new version could not load the data. It
// shows what the operator does next.
const stuckGrace = time.Minute

// await waits until the window closes, or for a maximum of bound if bound is
// set, and samples the mixed window. A watched change also ends stuckGrace
// after a pod on its new version could not load the data. It reports whether
// the mutation converged; ok is false if ctx is done.
func (m *Mutator) await(ctx context.Context, h holdHandle, bound time.Duration, watch *transition) (converged, ok bool) {
	var limit <-chan time.Time
	if bound > 0 {
		t := time.NewTimer(bound)
		defer t.Stop()
		limit = t.C
	}
	tick := time.NewTicker(m.observerCfg.Interval.Duration)
	defer tick.Stop()
	var stuck time.Time
	for {
		select {
		case <-ctx.Done():
			return false, false
		case converged = <-h.Done():
			m.sample(ctx, nil)
			return converged, true
		case <-limit:
			m.sample(ctx, nil)
			return false, true
		case <-tick.C:
			if !stuck.IsZero() {
				m.sample(ctx, nil)
				if time.Since(stuck) >= stuckGrace {
					return false, true
				}
				continue
			}
			if pod, line := m.sample(ctx, watch); line != "" {
				stuck = time.Now()
				m.log.Info("rollout stuck", "from", watch.edge.From, "to", watch.edge.To, "pod", pod, "log", line,
					"grace_seconds", stuckGrace.Seconds())
			}
		}
	}
}

// sample follows the open mixed window, and records it when it ended. For a
// watched change, it returns a pod on the new version whose log says that it
// could not load the data, and that log line.
func (m *Mutator) sample(ctx context.Context, watch *transition) (pod, line string) {
	if m.mixed == nil && watch == nil {
		return "", ""
	}
	s, err := m.fetch(ctx, fetchOpts{servers: true})
	if err != nil {
		return "", ""
	}
	if m.mixed != nil {
		if d, ended := m.mixed.observe(time.Now(), podImages(m.mixed.t, s)); ended {
			m.recordMixed(d)
		}
	}
	if watch == nil {
		return "", ""
	}
	pods := s.redis
	if watch.sentinel {
		pods = s.sentinels
	}
	for i := range pods {
		p := &pods[i]
		if containerImage(p, watch.container()) != watch.to.Image {
			continue
		}
		if line := m.loadError(ctx, p, watch.container()); line != "" {
			return p.Name, line
		}
	}
	return "", ""
}

func (m *Mutator) recordMixed(d time.Duration) {
	t := m.mixed.t
	m.mixed = nil
	m.mixedSeconds.WithLabelValues(t.edge.From, t.edge.To, t.container()).Observe(d.Seconds())
	m.log.Info("mixed versions", "from", t.edge.From, "to", t.edge.To, "sentinel", t.sentinel, "duration_seconds", d.Seconds())
}

func (m *Mutator) apply(ctx context.Context, p plan, log *slog.Logger) error {
	if p.secret != nil {
		if err := m.setPassword(ctx, p.secret.name, p.secret.password); err != nil {
			return err
		}
	}
	switch {
	case p.offline != nil:
		return m.rotateOffline(ctx, p.offline, log)
	case p.action != nil:
		return p.action(ctx)
	case p.patch != nil:
		return m.patch(ctx, p.patch)
	case p.pod != "":
		return m.deletePod(ctx, p.pod, p.uid, p.force)
	}
	return nil
}

// patch applies a JSON merge patch to the RedisFailover.
func (m *Mutator) patch(ctx context.Context, body []byte) error {
	rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Patch(ctx, m.in.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err == nil {
		m.auth.Update(rf)
	}
	return err
}

// deletePod deletes the pod if it is still the pod with this UID.
func (m *Mutator) deletePod(ctx context.Context, name string, uid types.UID, force bool) error {
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if force {
		opts.GracePeriodSeconds = new(int64)
	}
	return m.kube.CoreV1().Pods(m.in.Namespace).Delete(ctx, name, opts)
}

// setPassword sets the password in the Secret name of the instance. It creates
// the Secret if the Secret does not exist.
func (m *Mutator) setPassword(ctx context.Context, name, password string) error {
	secrets := m.kube.CoreV1().Secrets(m.in.Namespace)
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, auth.Secret(nil, m.in.Namespace, name, password), metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	_, err = secrets.Update(ctx, auth.Secret(secret, m.in.Namespace, name, password), metav1.UpdateOptions{})
	return err
}

func uids(pods []corev1.Pod) map[string]types.UID {
	u := map[string]types.UID{}
	for _, p := range pods {
		u[p.Name] = p.UID
	}
	return u
}

// countRecreated counts the pods that were replaced by a new pod of the
// same name.
func countRecreated(before map[string]types.UID, after []corev1.Pod) int {
	n := 0
	for _, p := range after {
		if uid, ok := before[p.Name]; ok && uid != p.UID {
			n++
		}
	}
	return n
}
