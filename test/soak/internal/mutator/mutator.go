// Package mutator keeps changing an instance, one mutation at a time, and
// measures how the operator converges after each.
package mutator

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/global"
	"github.com/saremox/redis-operator/test/soak/internal/instances"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// Results, the values of the result label.
const (
	resultConverged = "converged"
	resultTimeout   = "timeout"
	resultRejected  = "rejected"
	resultSkipped   = "skipped"
)

// Data is what the mutator needs of an instance's data.
type Data interface {
	Filled() bool
	// Refill makes Filled false until the recreated instance is filled.
	Refill()
	// Begin and Verify bracket a mutation: Verify checks the data after
	// it, and returns the writes it lost. event is the mutation's kind, or
	// config.EventReset.
	Begin()
	Verify(ctx context.Context, event string, step int) (int, error)
	Burst(ctx context.Context, hold time.Duration) (string, error)
	Writable(ctx context.Context) error
}

type Mutator struct {
	in       config.Instance
	cfg      config.Mutation
	operator config.Operator
	timeout  time.Duration
	// converge bounds each step of a mutation that waits on the operator
	// itself.
	convergeTimeout time.Duration
	observerCfg     config.Observer
	// versions holds the version catalogue and the transition graph.
	versions *config.Config
	kube     kubernetes.Interface
	rfs      versioned.Interface
	observer *observer.Observer
	data     Data
	auth     *auth.Source
	lock     *global.Lock
	// instance recreates the instance from its template, nil without one.
	instance *instances.Instance
	log      *slog.Logger

	// resets counts the resets, which take a chain's starts in turn;
	// mixed is the version change whose mixed window is open, and
	// resetWhy why the next mutation is a reset.
	resets   int
	mixed    *mixedWindow
	resetWhy string
	// current is the kind of the mutation running, "" between mutations.
	current atomic.Pointer[config.Kind]

	total        *prometheus.CounterVec
	converge     prometheus.ObserverVec
	recreated    *prometheus.CounterVec
	inProgress   *prometheus.GaugeVec
	transitions  *prometheus.CounterVec
	mixedSeconds prometheus.ObserverVec
	findings     *prometheus.CounterVec
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
		versions:        cfg,
		kube:            kube,
		rfs:             rfs,
		observer:        o,
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
	}
	// Alerts use the increase of mutation_total, which does not show a series
	// that starts at 1.
	for _, k := range in.MutationKinds() {
		mu.inProgress.WithLabelValues(string(k)).Set(0)
		for _, r := range []string{resultConverged, resultTimeout, resultRejected, resultSkipped} {
			mu.total.WithLabelValues(string(k), r)
		}
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

func (m *Mutator) Run(ctx context.Context) {
	stop := time.Now().Add(m.cfg.StopAfter.Duration)
	if !m.waitFilled(ctx) {
		return
	}
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
	if m.data == nil {
		return true
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for !m.data.Filled() {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
	m.log.Info("data filled, mutating")
	return true
}

// waitQuiet waits until no convergence window is open and every invariant
// holds.
func (m *Mutator) waitQuiet(ctx context.Context) bool {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for !m.observer.Quiet() {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
	return true
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
	timeout := m.cfg.Timeout(kind, m.observerCfg, s.rf.Spec.Redis.Replicas)
	log = log.With("params", p.params, "redis_replicas", s.rf.Spec.Redis.Replicas)
	before := uids(s.redis)

	inProgress := m.inProgress.WithLabelValues(string(kind))
	inProgress.Set(1)
	m.current.Store(&kind)
	defer func() {
		inProgress.Set(0)
		m.current.Store(nil)
	}()
	// An edge that may fail is observed for its timeout only, or until it
	// is stuck, and its window held on, for the reset that follows.
	bound := time.Duration(0)
	hold := timeout
	if p.flip != nil {
		m.mixed = &mixedWindow{t: p.flip}
	}
	if p.edge != nil {
		m.mixed = &mixedWindow{t: p.edge}
		if d := p.edge.edge.Timeout.Duration; d > 0 {
			timeout = d
		}
		if p.edge.edge.Expect != config.ExpectOK {
			bound, hold = timeout, 2*timeout
		}
	}
	// A rejected mutation changed nothing, so its window only waits for
	// the invariants.
	var applied, rejected atomic.Bool
	h := m.observer.Hold(hold, func(ctx context.Context) error {
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
	})
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
	d := time.Since(start)

	result := resultTimeout
	var exp expectation
	switch {
	case errors.As(applyErr, &exp):
		log = log.With("error", applyErr.Error())
	case applyErr != nil:
		result = resultRejected
		log = log.With("error", applyErr.Error())
	case converged:
		result = resultConverged
		m.converge.WithLabelValues(string(kind)).Observe(d.Seconds())
		if p.offline != nil {
			log.Info("scenario phase done", "phase", 8, "phase_name", "expect the rotation to converge",
				"duration_seconds", time.Since(appliedAt).Seconds())
		}
	}
	m.total.WithLabelValues(string(kind), result).Inc()

	recreated := -1
	if s, err := m.fetch(ctx, fetchOpts{}); err == nil {
		recreated = countRecreated(before, s.redis)
		m.recreated.WithLabelValues(string(kind)).Add(float64(recreated))
	}
	level := slog.LevelInfo
	if result != resultConverged {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "mutation done", "result", result, "duration_seconds", d.Seconds(), "pods_recreated", recreated)
	event := string(kind)
	if p.reset {
		event = config.EventReset
	}
	if p.edge == nil || applyErr != nil {
		if m.data != nil {
			_, _ = m.data.Verify(ctx, event, step)
		}
		return ""
	}
	vctx, cancel := context.WithTimeout(ctx, verifyBound)
	lost, verr := m.verify(vctx, event, step)
	cancel()
	if m.judge(ctx, p.edge, converged, lost, verr, log) != transitionOK && m.instance != nil {
		m.resetWhy = "after " + p.edge.edge.String() + " didn't converge"
		return config.Reset
	}
	return ""
}

// verifyBound bounds the verification a version change is judged by, as
// a change that failed may leave the instance without a master.
const verifyBound = 2 * time.Minute

func (m *Mutator) verify(ctx context.Context, event string, step int) (int, error) {
	if m.data == nil {
		return 0, nil
	}
	return m.data.Verify(ctx, event, step)
}

func (m *Mutator) plan(r *rand.Rand, kind config.Kind, s state) plan {
	switch kind {
	case config.ImageUpgrade:
		return m.planImage(r, s)
	case config.SentinelImageUpgrade:
		return m.planSentinelImage(r, s)
	case config.SentinelImageFlip:
		return m.planSentinelFlip(r, s)
	case config.Reset:
		why := "picked"
		if m.resetWhy != "" {
			why, m.resetWhy = m.resetWhy, ""
		}
		return m.planReset(s, why)
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
func (m *Mutator) await(ctx context.Context, h *observer.Hold, bound time.Duration, watch *transition) (converged, ok bool) {
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
		rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Patch(ctx, m.in.Name, types.MergePatchType, p.patch, metav1.PatchOptions{})
		if err == nil {
			m.auth.Update(rf)
		}
		return err
	case p.pod != "":
		opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.uid}}
		if p.force {
			opts.GracePeriodSeconds = new(int64)
		}
		return m.kube.CoreV1().Pods(m.in.Namespace).Delete(ctx, p.pod, opts)
	}
	return nil
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

// fetchOpts are what a convergence check reads beyond the objects.
type fetchOpts struct {
	// password is checked on every redis pod.
	password *string
	// sentinelMaster reads SENTINEL MASTER mymaster from every Sentinel.
	sentinelMaster bool
	// sentinelObjects reads the Sentinel Deployment, Service and ConfigMap
	// whatever the mode.
	sentinelObjects bool
	// servers reads INFO server from every redis and Sentinel pod.
	servers bool
}

func (m *Mutator) fetch(ctx context.Context, opts fetchOpts) (state, error) {
	rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Get(ctx, m.in.Name, metav1.GetOptions{})
	if err != nil {
		return state{}, err
	}
	m.auth.Update(rf)
	// Validate applies the operator defaults.
	_ = rf.Validate()
	s := state{rf: rf, password: m.auth.Password(), authSecret: m.in.AuthSecret}
	pods, err := m.kube.CoreV1().Pods(m.in.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/part-of=redis-failover,app.kubernetes.io/name=" + m.in.Name,
	})
	if err != nil {
		return state{}, err
	}
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for _, p := range pods.Items {
		switch p.Labels["app.kubernetes.io/component"] {
		case "redis":
			s.redis = append(s.redis, p)
		case "sentinel":
			s.sentinels = append(s.sentinels, p)
		}
	}
	port := int(rf.Spec.Redis.Port)
	if rf.Spec.Redis.MaxMemory != nil {
		s.config = m.redisConfig(ctx, s.redis, port)
	}
	if opts.password != nil {
		s.auth = m.checkPassword(ctx, s.redis, port, *opts.password)
	}
	if opts.sentinelMaster {
		s.sentinelMasters = m.sentinelMasters(ctx, s.sentinels)
	}
	if opts.servers {
		s.servers = m.servers(ctx, s.redis, s.sentinels, port)
	}
	s.sts, err = m.kube.AppsV1().StatefulSets(m.in.Namespace).Get(ctx, "rfr-"+m.in.Name, metav1.GetOptions{})
	if err != nil {
		return state{}, err
	}
	if rf.SentinelEnabled() || opts.sentinelObjects {
		name := "rfs-" + m.in.Name
		s.sentinel, err = m.kube.AppsV1().Deployments(m.in.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			s.sentinel, err = nil, nil
		}
		if err != nil {
			return state{}, err
		}
		if opts.sentinelObjects {
			if s.sentinelService, err = exists(m.kube.CoreV1().Services(m.in.Namespace).Get(ctx, name, metav1.GetOptions{})); err != nil {
				return state{}, err
			}
			if s.sentinelConfigMap, err = exists(m.kube.CoreV1().ConfigMaps(m.in.Namespace).Get(ctx, name, metav1.GetOptions{})); err != nil {
				return state{}, err
			}
		}
	}
	return s, nil
}

func exists[T any](_ T, err error) (bool, error) {
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// podClient connects to a redis or Sentinel pod with credentials.
func (m *Mutator) podClient(ip string, port int, credentials func() (string, string)) *redis.Client {
	return m.podClientAddr(net.JoinHostPort(ip, strconv.Itoa(port)), credentials)
}

func (m *Mutator) podClientAddr(addr string, credentials func() (string, string)) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  addr,
		CredentialsProvider:   credentials,
		DialTimeout:           m.timeout,
		ReadTimeout:           m.timeout,
		WriteTimeout:          m.timeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
	})
}

// eachPod runs f on a client of every pod with an IP, in parallel, and
// returns its results by pod name.
func eachPod[T any](ctx context.Context, m *Mutator, pods []corev1.Pod, port int, credentials func() (string, string), f func(context.Context, *redis.Client) (T, error)) (map[string]T, map[string]error) {
	var mu sync.Mutex
	vals, errs := map[string]T{}, map[string]error{}
	var wg sync.WaitGroup
	for i := range pods {
		p := &pods[i]
		if p.Status.PodIP == "" {
			errs[p.Name] = errors.New("no pod IP")
			continue
		}
		wg.Go(func() {
			c := m.podClient(p.Status.PodIP, port, credentials)
			defer func() { _ = c.Close() }()
			cctx, cancel := context.WithTimeout(ctx, m.timeout)
			defer cancel()
			v, err := f(cctx, c)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[p.Name] = err
			} else {
				vals[p.Name] = v
			}
		})
	}
	wg.Wait()
	return vals, errs
}

// checkPassword reports whether every redis pod accepts password; the empty
// password is whether it needs none.
func (m *Mutator) checkPassword(ctx context.Context, pods []corev1.Pod, port int, password string) map[string]error {
	_, errs := eachPod(ctx, m, pods, port, auth.Fixed(password), func(ctx context.Context, c *redis.Client) (string, error) {
		return c.Ping(ctx).Result()
	})
	out := map[string]error{}
	for _, p := range pods {
		out[p.Name] = errs[p.Name]
	}
	return out
}

// servers reads INFO server, and INFO replication, from every redis and
// Sentinel pod.
func (m *Mutator) servers(ctx context.Context, redisPods, sentinels []corev1.Pod, port int) map[string]server {
	out := map[string]server{}
	for _, set := range []struct {
		pods     []corev1.Pod
		port     int
		creds    func() (string, string)
		sections []string
	}{
		{redisPods, port, m.auth.Provider(), []string{"server", "replication"}},
		{sentinels, sentinelPort, auth.Fixed(""), []string{"server"}},
	} {
		vals, errs := eachPod(ctx, m, set.pods, set.port, set.creds, func(ctx context.Context, c *redis.Client) (string, error) {
			return c.Info(ctx, set.sections...).Result()
		})
		for _, p := range set.pods {
			if err, ok := errs[p.Name]; ok {
				out[p.Name] = server{err: err}
				continue
			}
			name, version, fields := observer.ServerOf(vals[p.Name])
			out[p.Name] = server{name: name, version: version, fields: fields}
		}
	}
	return out
}

func (m *Mutator) sentinelMasters(ctx context.Context, pods []corev1.Pod) map[string]sentinelMaster {
	vals, errs := eachPod(ctx, m, pods, sentinelPort, auth.Fixed(""), observer.SentinelMaster)
	out := map[string]sentinelMaster{}
	for _, p := range pods {
		out[p.Name] = sentinelMaster{fields: vals[p.Name], err: errs[p.Name]}
	}
	return out
}

// redisConfig reads maxmemory and maxmemory-policy from every redis pod
// with an IP, the pods the operator configures.
func (m *Mutator) redisConfig(ctx context.Context, pods []corev1.Pod, port int) []maxmem.Pod {
	vals, errs := eachPod(ctx, m, pods, port, m.auth.Provider(), func(ctx context.Context, c *redis.Client) (map[string]string, error) {
		return c.ConfigGet(ctx, "maxmemory*").Result()
	})
	out := make([]maxmem.Pod, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		if p.Status.PodIP == "" {
			continue
		}
		mp := maxmem.Pod{Name: p.Name, Limit: maxmem.RedisLimit(p), Err: errs[p.Name]}
		if mp.Err == nil {
			cfg := vals[p.Name]
			mp.MaxMemory, mp.Err = strconv.ParseInt(cfg["maxmemory"], 10, 64)
			mp.Policy = cfg["maxmemory-policy"]
		}
		out = append(out, mp)
	}
	return out
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
