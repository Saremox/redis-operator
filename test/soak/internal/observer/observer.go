// Package observer checks every few seconds that an instance is in the
// state its spec asks for, and reports each invariant and its violations.
package observer

import (
	"context"
	"errors"
	"log/slog"
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
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

const (
	sentinelPort       = 26379
	sentinelMasterName = "mymaster"
)

type Observer struct {
	in       config.Instance
	interval time.Duration
	timeout  time.Duration
	kube     kubernetes.Interface
	rfs      versioned.Interface
	log      *slog.Logger

	tracker    *tracker
	holds      chan *hold
	hold       *hold
	holdErr    error
	view       atomic.Pointer[view]
	generation int64
	master     pod
	lagPods    map[string]bool
	servers    map[string][2]string
	oomSeen    map[string]bool
	evictions  evictions
	failoverCh chan struct{}

	ok         *prometheus.GaugeVec
	violation  prometheus.ObserverVec
	findings   *prometheus.CounterVec
	masters    prometheus.Gauge
	failovers  prometheus.Counter
	lag        *prometheus.GaugeVec
	rfHealthy  prometheus.Gauge
	serverInfo *prometheus.GaugeVec

	datasetKeys prometheus.Gauge
	usedMemory  prometheus.Gauge
	maxMemory   prometheus.Gauge
	evicted     prometheus.Counter
}

func New(in config.Instance, cfg *config.Config, kube kubernetes.Interface, rfs versioned.Interface, m *metrics.Metrics, log *slog.Logger) *Observer {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	return &Observer{
		in:         in,
		interval:   cfg.Observer.Interval.Duration,
		timeout:    cfg.Probe.Timeout.Duration,
		kube:       kube,
		rfs:        rfs,
		log:        log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
		tracker:    newTracker(cfg.Observer.ConvergenceTimeout.Duration, cfg.Mutation.MinDwell.Duration),
		holds:      make(chan *hold, 1),
		lagPods:    map[string]bool{},
		servers:    map[string][2]string{},
		failoverCh: make(chan struct{}, 1),
		ok:         m.InvariantOK.MustCurryWith(labels),
		violation:  m.InvariantViolation.MustCurryWith(labels),
		findings:   m.Findings.MustCurryWith(labels),
		masters:    m.Masters.With(labels),
		failovers:  m.Failovers.With(labels),
		lag:        m.ReplicationLag.MustCurryWith(labels),
		rfHealthy:  m.RFHealthy.With(labels),
		serverInfo: m.ServerInfo.MustCurryWith(labels),

		datasetKeys: m.DatasetKeys.With(labels),
		usedMemory:  m.UsedMemory.With(labels),
		maxMemory:   m.MaxMemory.With(labels),
		evicted:     m.EvictedKeys.With(labels),
	}
}

// hold is a convergence window a mutation holds open.
type hold struct {
	since     time.Time
	converged func(context.Context) error
	done      chan bool
}

// view is what a mutator reads of the observer's last round.
type view struct {
	quiet    bool
	master   string
	masterIP string
}

// Hold opens a convergence window for a mutation that is about to be
// applied. The window stays open until converged returns nil, every
// invariant holds and the minimum dwell has passed, or until the
// convergence timeout. The channel receives true when it closed and false
// when it timed out. Only one hold may be pending at a time.
func (o *Observer) Hold(converged func(context.Context) error) <-chan bool {
	h := &hold{since: time.Now(), converged: converged, done: make(chan bool, 1)}
	o.holds <- h
	return h.done
}

// Quiet reports whether, at the last check, no convergence window was open
// and every invariant held.
func (o *Observer) Quiet() bool {
	v := o.view.Load()
	return v != nil && v.quiet
}

// Master returns the name of the pod that last was the single master.
func (o *Observer) Master() string {
	if v := o.view.Load(); v != nil {
		return v.master
	}
	return ""
}

// MasterAddr returns the address of the pod that last was the single
// master.
func (o *Observer) MasterAddr() string {
	if v := o.view.Load(); v != nil && v.masterIP != "" {
		return net.JoinHostPort(v.masterIP, strconv.Itoa(o.in.Port))
	}
	return ""
}

// Failovers receives after the master's identity changed.
func (o *Observer) Failovers() <-chan struct{} {
	return o.failoverCh
}

func (o *Observer) Run(ctx context.Context) {
	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		s, generation, err := o.collect(ctx)
		if err != nil && ctx.Err() == nil {
			o.log.Warn("observing", "error", err.Error())
		} else if err == nil {
			// A hold is taken only after the snapshot is collected, so the
			// window is open before any snapshot of the mutation is judged.
			o.takeHold()
			o.apply(time.Now(), s, generation, o.converged(ctx))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (o *Observer) collect(ctx context.Context) (snapshot, int64, error) {
	rf, err := o.rfs.DatabasesV1().RedisFailovers(o.in.Namespace).Get(ctx, o.in.Name, metav1.GetOptions{})
	if err != nil {
		return snapshot{}, 0, err
	}
	// Validate fills in the operator's defaults.
	spec := rf.DeepCopy()
	_ = spec.Validate()
	s := snapshot{
		rf:               spec,
		sentinel:         spec.SentinelEnabled(),
		redisReplicas:    spec.Spec.Redis.Replicas,
		sentinelReplicas: spec.Spec.Sentinel.Replicas,
		port:             int(spec.Spec.Redis.Port),
		state:            rf.Status.State,
		message:          rf.Status.Message,
	}

	pods, err := o.kube.CoreV1().Pods(o.in.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/part-of=redis-failover,app.kubernetes.io/name=" + o.in.Name,
	})
	if err != nil {
		return snapshot{}, 0, err
	}
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for i := range pods.Items {
		p := &pods.Items[i]
		switch p.Labels["app.kubernetes.io/component"] {
		case "redis":
			s.redis = append(s.redis, redisPod{pod: newPod(p), limit: maxmem.RedisLimit(p)})
		case "sentinel":
			s.sentinels = append(s.sentinels, sentinelPod{pod: newPod(p)})
		}
	}

	eps, err := o.kube.DiscoveryV1().EndpointSlices(o.in.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=rfrm-" + o.in.Name,
	})
	if err != nil {
		return snapshot{}, 0, err
	}
	for _, slice := range eps.Items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				if !slices.Contains(s.endpoints, addr) {
					s.endpoints = append(s.endpoints, addr)
				}
			}
		}
	}

	var wg sync.WaitGroup
	withConfig := spec.Spec.Redis.MaxMemory != nil
	for i := range s.redis {
		p := &s.redis[i]
		wg.Go(func() { o.redisInfo(ctx, p, s.port, withConfig) })
	}
	for i := range s.sentinels {
		p := &s.sentinels[i]
		wg.Go(func() { p.master, p.err = o.sentinelMaster(ctx, p.IP) })
	}
	wg.Wait()
	return s, rf.Generation, nil
}

func newPod(p *corev1.Pod) pod {
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	return pod{
		Name:     p.Name,
		UID:      string(p.UID),
		IP:       p.Status.PodIP,
		Ready:    ready && p.DeletionTimestamp == nil,
		OOMKills: oomKills(p),
	}
}

var errNoIP = errors.New("no pod IP")

func (o *Observer) client(ip string, port int) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  net.JoinHostPort(ip, strconv.Itoa(port)),
		DialTimeout:           o.timeout,
		ReadTimeout:           o.timeout,
		WriteTimeout:          o.timeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
	})
}

// redisInfo sends INFO to the pod, and with withConfig CONFIG GET
// maxmemory*.
func (o *Observer) redisInfo(ctx context.Context, p *redisPod, port int, withConfig bool) {
	if p.IP == "" {
		p.err, p.configErr = errNoIP, errNoIP
		return
	}
	c := o.client(p.IP, port)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s, err := c.Info(ctx).Result()
	if err == nil {
		p.info = parseInfo(s)
	}
	p.err = err
	if withConfig {
		p.config, p.configErr = c.ConfigGet(ctx, "maxmemory*").Result()
	}
}

func (o *Observer) sentinelMaster(ctx context.Context, ip string) (string, error) {
	if ip == "" {
		return "", errNoIP
	}
	c := o.client(ip, sentinelPort)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	addr, err := c.Do(ctx, "SENTINEL", "get-master-addr-by-name", sentinelMasterName).StringSlice()
	if err != nil {
		return "", err
	}
	if len(addr) != 2 {
		return "", errors.New("no master known")
	}
	return net.JoinHostPort(addr[0], addr[1]), nil
}

func (o *Observer) takeHold() {
	select {
	case h := <-o.holds:
		o.hold, o.holdErr = h, nil
		o.tracker.hold(h.since)
		o.log.Info("convergence window opened", "by", "mutation")
	default:
	}
}

// converged reports whether the mutation holding the window has converged.
func (o *Observer) converged(ctx context.Context) bool {
	if o.hold == nil {
		return false
	}
	o.holdErr = o.hold.converged(ctx)
	return o.holdErr == nil
}

func (o *Observer) apply(now time.Time, s snapshot, generation int64, converged bool) {
	if generation != o.generation {
		o.tracker.openWindow(now)
		o.log.Info("convergence window opened", "generation", generation, "previous_generation", o.generation)
		o.generation = generation
	}
	checks := evaluate(s)
	allOK := true
	for _, c := range checks {
		o.ok.WithLabelValues(c.invariant).Set(gauge(c.err == nil))
		allOK = allOK && c.err == nil
	}
	for _, e := range o.tracker.update(now, checks, converged) {
		o.record(e)
	}
	o.rfHealthy.Set(gauge(s.state == redisfailoverv1.HealthyState))
	o.observeMaster(s)
	o.observeServers(s)
	o.observeOOMKills(s)
	o.observeData(s)
	v := &view{quiet: allOK && !o.tracker.windowOpen(), master: o.master.Name}
	if _, err := s.master(); err == nil {
		v.masterIP = o.master.IP
	}
	o.view.Store(v)
}

func (o *Observer) record(e event) {
	log := o.log.With("invariant", e.invariant, "finding", e.finding)
	switch e.kind {
	case evViolated:
		if e.finding {
			o.findings.WithLabelValues(e.invariant).Inc()
		}
		log.Warn("invariant violated", "reason", e.err.Error())
	case evRestored:
		o.violation.WithLabelValues(e.invariant).Observe(e.duration.Seconds())
		log.Info("invariant restored", "duration_seconds", e.duration.Seconds())
	case evFinding:
		o.findings.WithLabelValues(e.invariant).Inc()
		log.Warn("invariant still violated after the convergence timeout", "duration_seconds", e.duration.Seconds())
	case evWindowClosed:
		o.log.Info("convergence window closed", "duration_seconds", e.duration.Seconds())
		o.release(true)
	case evWindowTimedOut:
		log := o.log
		if o.hold != nil && o.holdErr != nil {
			log = log.With("mutation", o.holdErr.Error())
		}
		log.Warn("convergence window timed out", "duration_seconds", e.duration.Seconds())
		o.release(false)
	}
}

func (o *Observer) release(converged bool) {
	if o.hold != nil {
		o.hold.done <- converged
		o.hold = nil
	}
}

func (o *Observer) observeMaster(s snapshot) {
	o.masters.Set(float64(len(s.masters())))
	master, err := s.master()
	lags := map[string]int64{}
	if err == nil {
		if o.master.UID != "" && o.master.UID != master.UID {
			o.failovers.Inc()
			o.log.Warn("failover", "from", o.master.Name, "to", master.Name)
			select {
			case o.failoverCh <- struct{}{}:
			default:
			}
		}
		o.master = master.pod
		lags = s.lags(master)
	}
	for name := range o.lagPods {
		if _, ok := lags[name]; !ok {
			o.lag.DeleteLabelValues(name)
			delete(o.lagPods, name)
		}
	}
	for name, lag := range lags {
		o.lag.WithLabelValues(name).Set(float64(lag))
		o.lagPods[name] = true
	}
}

// observeServers exports the server and version of every pod that answered
// INFO, and drops the series of pods that didn't or are gone.
func (o *Observer) observeServers(s snapshot) {
	servers := map[string][2]string{}
	for _, p := range s.redis {
		if p.info != nil {
			name, version := p.info.server()
			servers[p.Name] = [2]string{name, version}
		}
	}
	for name, old := range o.servers {
		if servers[name] != old {
			o.serverInfo.DeleteLabelValues(name, old[0], old[1])
		}
	}
	for name, sv := range servers {
		o.serverInfo.WithLabelValues(name, sv[0], sv[1]).Set(1)
	}
	o.servers = servers
}

func gauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
