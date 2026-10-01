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
	generation int64
	master     pod
	lagPods    map[string]bool
	servers    map[string][2]string

	ok         *prometheus.GaugeVec
	violation  prometheus.ObserverVec
	findings   *prometheus.CounterVec
	masters    prometheus.Gauge
	failovers  prometheus.Counter
	lag        *prometheus.GaugeVec
	rfHealthy  prometheus.Gauge
	serverInfo *prometheus.GaugeVec
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
		tracker:    newTracker(cfg.Observer.ConvergenceTimeout.Duration),
		lagPods:    map[string]bool{},
		servers:    map[string][2]string{},
		ok:         m.InvariantOK.MustCurryWith(labels),
		violation:  m.InvariantViolation.MustCurryWith(labels),
		findings:   m.Findings.MustCurryWith(labels),
		masters:    m.Masters.With(labels),
		failovers:  m.Failovers.With(labels),
		lag:        m.ReplicationLag.MustCurryWith(labels),
		rfHealthy:  m.RFHealthy.With(labels),
		serverInfo: m.ServerInfo.MustCurryWith(labels),
	}
}

func (o *Observer) Run(ctx context.Context) {
	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		s, generation, err := o.collect(ctx)
		if err != nil && ctx.Err() == nil {
			o.log.Warn("observing", "error", err.Error())
		} else if err == nil {
			o.apply(time.Now(), s, generation)
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
			s.redis = append(s.redis, redisPod{pod: newPod(p)})
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
	for i := range s.redis {
		p := &s.redis[i]
		wg.Go(func() { p.info, p.err = o.redisInfo(ctx, p.IP, s.port) })
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
		Name:  p.Name,
		UID:   string(p.UID),
		IP:    p.Status.PodIP,
		Ready: ready && p.DeletionTimestamp == nil,
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

func (o *Observer) redisInfo(ctx context.Context, ip string, port int) (info, error) {
	if ip == "" {
		return nil, errNoIP
	}
	c := o.client(ip, port)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s, err := c.Info(ctx).Result()
	if err != nil {
		return nil, err
	}
	return parseInfo(s), nil
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

func (o *Observer) apply(now time.Time, s snapshot, generation int64) {
	if generation != o.generation {
		o.tracker.openWindow(now)
		o.log.Info("convergence window opened", "generation", generation, "previous_generation", o.generation)
		o.generation = generation
	}
	checks := evaluate(s)
	for _, c := range checks {
		o.ok.WithLabelValues(c.invariant).Set(gauge(c.err == nil))
	}
	for _, e := range o.tracker.update(now, checks) {
		o.record(e)
	}
	o.rfHealthy.Set(gauge(s.state == redisfailoverv1.HealthyState))
	o.observeMaster(s)
	o.observeServers(s)
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
	case evWindowTimedOut:
		o.log.Warn("convergence window timed out", "duration_seconds", e.duration.Seconds())
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
