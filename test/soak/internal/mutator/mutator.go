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
	"github.com/saremox/redis-operator/test/soak/internal/config"
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
	// it.
	Begin()
	Verify(ctx context.Context, kind config.Kind, step int)
	Burst(ctx context.Context, hold time.Duration) (string, error)
	Writable(ctx context.Context) error
}

type Mutator struct {
	in       config.Instance
	cfg      config.Mutation
	timeout  time.Duration
	kube     kubernetes.Interface
	rfs      versioned.Interface
	observer *observer.Observer
	data     Data
	log      *slog.Logger

	total      *prometheus.CounterVec
	converge   prometheus.ObserverVec
	recreated  *prometheus.CounterVec
	inProgress *prometheus.GaugeVec
}

// New returns the mutator of an instance; data is nil for an instance
// without data.
func New(in config.Instance, cfg *config.Config, kube kubernetes.Interface, rfs versioned.Interface, o *observer.Observer, data Data, m *metrics.Metrics, log *slog.Logger) *Mutator {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	mu := &Mutator{
		in:         in,
		cfg:        cfg.Mutation,
		timeout:    cfg.Probe.Timeout.Duration,
		kube:       kube,
		rfs:        rfs,
		observer:   o,
		data:       data,
		log:        log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode, "seed", cfg.Mutation.Seed),
		total:      m.MutationTotal.MustCurryWith(labels),
		converge:   m.MutationConverge.MustCurryWith(labels),
		recreated:  m.PodsRecreated.MustCurryWith(labels),
		inProgress: m.MutationInProgress.MustCurryWith(labels),
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
		m.mutate(ctx, step, r)
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

func (m *Mutator) mutate(ctx context.Context, step int, r *rand.Rand) {
	s, err := m.fetch(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("reading the instance for a mutation", "step", step, "error", err.Error())
		}
		return
	}
	kind := pickKind(r, m.in.Mutations)
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
		s, err := m.fetch(ctx)
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
	applyErr := m.apply(ctx, p)
	if applyErr != nil {
		rejected.Store(true)
	}
	applied.Store(true)
	var converged bool
	select {
	case <-ctx.Done():
		return
	case converged = <-done:
	}
	d := time.Since(start)

	result := resultTimeout
	switch {
	case applyErr != nil:
		result = resultRejected
		log = log.With("error", applyErr.Error())
	case converged:
		result = resultConverged
		m.converge.WithLabelValues(string(kind)).Observe(d.Seconds())
	}
	m.total.WithLabelValues(string(kind), result).Inc()

	recreated := -1
	if s, err := m.fetch(ctx); err == nil {
		recreated = countRecreated(before, s.redis)
		m.recreated.WithLabelValues(string(kind)).Add(float64(recreated))
	}
	level := slog.LevelInfo
	if result != resultConverged {
		level = slog.LevelWarn
	}
	log.Log(ctx, level, "mutation done", "result", result, "duration_seconds", d.Seconds(), "pods_recreated", recreated)
	if m.data != nil {
		m.data.Verify(ctx, kind, step)
	}
}

func (m *Mutator) apply(ctx context.Context, p plan) error {
	if p.action != nil {
		return p.action(ctx)
	}
	if p.patch != nil {
		_, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Patch(ctx, m.in.Name, types.MergePatchType, p.patch, metav1.PatchOptions{})
		return err
	}
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.uid}}
	if p.force {
		opts.GracePeriodSeconds = new(int64)
	}
	return m.kube.CoreV1().Pods(m.in.Namespace).Delete(ctx, p.pod, opts)
}

func (m *Mutator) fetch(ctx context.Context) (state, error) {
	rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Get(ctx, m.in.Name, metav1.GetOptions{})
	if err != nil {
		return state{}, err
	}
	// Validate fills in the operator's defaults.
	_ = rf.Validate()
	s := state{rf: rf}
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
	if rf.Spec.Redis.MaxMemory != nil {
		s.config = m.redisConfig(ctx, s.redis, int(rf.Spec.Redis.Port))
	}
	s.sts, err = m.kube.AppsV1().StatefulSets(m.in.Namespace).Get(ctx, "rfr-"+m.in.Name, metav1.GetOptions{})
	if err != nil {
		return state{}, err
	}
	if rf.SentinelEnabled() {
		s.sentinel, err = m.kube.AppsV1().Deployments(m.in.Namespace).Get(ctx, "rfs-"+m.in.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			s.sentinel, err = nil, nil
		}
		if err != nil {
			return state{}, err
		}
	}
	return s, nil
}

// redisConfig reads maxmemory and maxmemory-policy from every redis pod
// with an IP, the pods the operator configures.
func (m *Mutator) redisConfig(ctx context.Context, pods []corev1.Pod, port int) []maxmem.Pod {
	out := make([]maxmem.Pod, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		if p.Status.PodIP == "" {
			continue
		}
		mp := maxmem.Pod{Name: p.Name, Limit: maxmem.RedisLimit(p)}
		c := redis.NewClient(&redis.Options{
			Addr:                  net.JoinHostPort(p.Status.PodIP, strconv.Itoa(port)),
			DialTimeout:           m.timeout,
			ReadTimeout:           m.timeout,
			WriteTimeout:          m.timeout,
			ContextTimeoutEnabled: true,
			MaxRetries:            -1,
		})
		cctx, cancel := context.WithTimeout(ctx, m.timeout)
		cfg, err := c.ConfigGet(cctx, "maxmemory*").Result()
		cancel()
		_ = c.Close()
		if err == nil {
			mp.MaxMemory, err = strconv.ParseInt(cfg["maxmemory"], 10, 64)
			mp.Policy = cfg["maxmemory-policy"]
		}
		mp.Err = err
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
