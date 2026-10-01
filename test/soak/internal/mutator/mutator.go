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
	// Begin and Verify bracket a mutation: Verify checks the data after
	// it. event is the mutation's kind, or config.EventReset.
	Begin()
	Verify(ctx context.Context, event string, step int)
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
	kube            kubernetes.Interface
	rfs             versioned.Interface
	observer        *observer.Observer
	data            Data
	auth            *auth.Source
	lock            *global.Lock
	log             *slog.Logger

	total      *prometheus.CounterVec
	converge   prometheus.ObserverVec
	recreated  *prometheus.CounterVec
	inProgress *prometheus.GaugeVec
}

// New returns the mutator of an instance; data is nil for an instance
// without data.
func New(in config.Instance, cfg *config.Config, kube kubernetes.Interface, rfs versioned.Interface, o *observer.Observer, data Data, a *auth.Source, lock *global.Lock, m *metrics.Metrics, log *slog.Logger) *Mutator {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	mu := &Mutator{
		in:              in,
		cfg:             cfg.Mutation,
		operator:        cfg.Operator,
		timeout:         cfg.Probe.Timeout.Duration,
		convergeTimeout: cfg.Observer.ConvergenceTimeout.Duration,
		kube:            kube,
		rfs:             rfs,
		observer:        o,
		data:            data,
		auth:            a,
		lock:            lock,
		log:             log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode, "seed", cfg.Mutation.Seed),
		total:           m.MutationTotal.MustCurryWith(labels),
		converge:        m.MutationConverge.MustCurryWith(labels),
		recreated:       m.PodsRecreated.MustCurryWith(labels),
		inProgress:      m.MutationInProgress.MustCurryWith(labels),
	}
	for _, k := range in.Mutations.Sorted() {
		mu.inProgress.WithLabelValues(string(k)).Set(0)
	}
	return mu
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
		m.mutate(ctx, step, r, kind)
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

// expectation is a mutation step whose expected outcome didn't come
// within its bound: the mutation timed out.
type expectation struct{ error }

func (m *Mutator) mutate(ctx context.Context, step int, r *rand.Rand, kind config.Kind) {
	s, err := m.fetch(ctx, fetchOpts{})
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("reading the instance for a mutation", "step", step, "error", err.Error())
		}
		return
	}
	p := newPlan(r, kind, m.in.Mutations, s, m.observer.Master(), m.data)
	log := m.log.With("kind", kind, "step", step)
	if p.skip != "" {
		m.total.WithLabelValues(string(kind), resultSkipped).Inc()
		log.Info("mutation skipped", "result", resultSkipped, "reason", p.skip)
		return
	}
	log = log.With("params", p.params, "redis_replicas", s.rf.Spec.Redis.Replicas)
	before := uids(s.redis)

	inProgress := m.inProgress.WithLabelValues(string(kind))
	inProgress.Set(1)
	defer inProgress.Set(0)
	// A rejected mutation changed nothing, so its window only waits for
	// the invariants.
	var applied, rejected atomic.Bool
	done := m.observer.Hold(func(ctx context.Context) error {
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
	log.Info("mutating")
	start := time.Now()
	applyErr := m.apply(ctx, p, log)
	if applyErr != nil {
		rejected.Store(true)
	}
	applied.Store(true)
	appliedAt := time.Now()
	var converged bool
	select {
	case <-ctx.Done():
		return
	case converged = <-done:
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
	if m.data != nil {
		event := string(kind)
		if p.reset {
			event = config.EventReset
		}
		m.data.Verify(ctx, event, step)
	}
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

// setPassword sets the password of the instance's Secret name, creating
// the Secret if it doesn't exist.
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
}

func (m *Mutator) fetch(ctx context.Context, opts fetchOpts) (state, error) {
	rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Get(ctx, m.in.Name, metav1.GetOptions{})
	if err != nil {
		return state{}, err
	}
	m.auth.Update(rf)
	// Validate fills in the operator's defaults.
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
	return redis.NewClient(&redis.Options{
		Addr:                  net.JoinHostPort(ip, strconv.Itoa(port)),
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
