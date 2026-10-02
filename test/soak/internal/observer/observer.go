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
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/customconfig"
	"github.com/saremox/redis-operator/test/soak/internal/global"
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
	auth     *auth.Source
	lock     *global.Lock
	// source is the observer of the instance a bootstrapping one
	// replicates from.
	source *Observer
	log    *slog.Logger

	tracker    *tracker
	holds      chan *Hold
	hold       *Hold
	holdErr    error
	view       atomic.Pointer[view]
	generation int64
	rfUID      string
	// recreated is set when the RedisFailover was recreated, until the
	// failover that follows.
	recreated  bool
	master     pod
	lagPods    map[string]bool
	servers    map[string][2]string
	oomSeen    map[string]bool
	evictions  evictions
	failoverCh chan string
	// evaluated are the invariants of the last round.
	evaluated []string
	// external is why a window is held open from outside the instance;
	// sourceMaster is the source's master a bootstrapping instance last
	// saw.
	external     string
	sourceMaster string
	sentinelUp   bool
	// reportOnly are the invariants that are reported but never findings,
	// and noted when each of them was violated.
	reportOnly map[string]bool
	noted      map[string]time.Time

	ok         *prometheus.GaugeVec
	violation  prometheus.ObserverVec
	findings   *prometheus.CounterVec
	masters    prometheus.Gauge
	failovers  prometheus.Counter
	lag        *prometheus.GaugeVec
	rfHealthy  prometheus.Gauge
	serverInfo *prometheus.GaugeVec
	windowOpen prometheus.Gauge

	datasetKeys prometheus.Gauge
	usedMemory  prometheus.Gauge
	maxMemory   prometheus.Gauge
	evicted     prometheus.Counter
}

func New(in config.Instance, cfg *config.Config, kube kubernetes.Interface, rfs versioned.Interface, a *auth.Source, lock *global.Lock, m *metrics.Metrics, log *slog.Logger) *Observer {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	o := &Observer{
		in:         in,
		interval:   cfg.Observer.Interval.Duration,
		timeout:    cfg.Probe.Timeout.Duration,
		kube:       kube,
		rfs:        rfs,
		auth:       a,
		lock:       lock,
		log:        log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
		tracker:    newTracker(cfg.Observer.ConvergenceTimeout.Duration, cfg.Mutation.MinDwell.Duration),
		holds:      make(chan *Hold, 1),
		lagPods:    map[string]bool{},
		servers:    map[string][2]string{},
		failoverCh: make(chan string, 1),
		reportOnly: map[string]bool{invReplicaReadyWithoutData: !cfg.Observer.ReplicaReadyWithoutDataFinding()},
		noted:      map[string]time.Time{},
		ok:         m.InvariantOK.MustCurryWith(labels),
		violation:  m.InvariantViolation.MustCurryWith(labels),
		findings:   m.Findings.MustCurryWith(labels),
		masters:    m.Masters.With(labels),
		failovers:  m.Failovers.With(labels),
		lag:        m.ReplicationLag.MustCurryWith(labels),
		rfHealthy:  m.RFHealthy.With(labels),
		serverInfo: m.ServerInfo.MustCurryWith(labels),
		windowOpen: m.WindowOpen.With(labels),

		datasetKeys: m.DatasetKeys.With(labels),
		usedMemory:  m.UsedMemory.With(labels),
		maxMemory:   m.MaxMemory.With(labels),
		evicted:     m.EvictedKeys.With(labels),
	}
	// Alerts take the increase of findings_total, which a series that
	// first appears at 1 wouldn't show.
	o.findings.WithLabelValues(invOOMKilled)
	return o
}

// Hold is a convergence window a mutation holds open.
type Hold struct {
	since     time.Time
	timeout   time.Duration
	converged func(context.Context) error
	done      chan bool
	applied   atomic.Int64
}

// Applied starts the window's timeout: the mutation was applied now.
func (h *Hold) Applied() {
	h.applied.Store(time.Now().UnixNano())
}

// Done receives true when the window closed and false when it timed out.
func (h *Hold) Done() <-chan bool {
	return h.done
}

func (h *Hold) appliedAt() time.Time {
	if n := h.applied.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// SetSource makes a bootstrapping instance's observer follow the instance
// it replicates from: the source's master offset is what its pods lag
// behind, and the source's convergence windows are its own.
func (o *Observer) SetSource(source *Observer) {
	o.source = source
}

// view is what a mutator reads of the observer's last round.
type view struct {
	at         time.Time
	quiet      bool
	windowOpen bool
	// failing are the invariants violated that may be findings.
	failing  []string
	master   string
	masterIP string
	// sentinelPath is whether the instance runs Sentinels that are Ready
	// and agree on the master, so it can be probed through them.
	sentinelPath bool
	redis        []PodAddr
}

// PodAddr is a redis pod's address.
type PodAddr struct {
	Name string
	Addr string
}

// Hold opens a convergence window for a mutation that is about to be
// applied. The window stays open until converged returns nil, every
// invariant holds and the minimum dwell has passed since the mutation was
// applied, or until timeout after that. Only one hold may be pending at a
// time; a new one takes over the window of the previous one, which is then
// never done.
func (o *Observer) Hold(timeout time.Duration, converged func(context.Context) error) *Hold {
	h := &Hold{since: time.Now(), timeout: timeout, converged: converged, done: make(chan bool, 1)}
	o.holds <- h
	return h
}

// Quiet reports whether, at the last check, no convergence window was open
// and every invariant held.
func (o *Observer) Quiet() bool {
	v := o.view.Load()
	return v != nil && v.quiet
}

// Report is what the observer's last round found.
type Report struct {
	At time.Time
	// Quiet is no convergence window open and every invariant holding.
	Quiet bool
	// Failing are the violated invariants that may be findings.
	Failing []string
}

// Report returns what the last round found, a zero Report before the first.
func (o *Observer) Report() Report {
	v := o.view.Load()
	if v == nil {
		return Report{}
	}
	return Report{At: v.at, Quiet: v.quiet, Failing: v.failing}
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

// SentinelPath reports whether the instance's Sentinels were all Ready and
// agreed on the master since its mode last changed to Sentinel, or it was
// recreated.
func (o *Observer) SentinelPath() bool {
	v := o.view.Load()
	return v != nil && v.sentinelPath
}

// RedisAddrs returns the address of every redis pod with an IP.
func (o *Observer) RedisAddrs() []PodAddr {
	if v := o.view.Load(); v != nil {
		return v.redis
	}
	return nil
}

// Failovers receives an event after the master's identity changed:
// config.EventFailover, or config.EventReset where the change lost the data
// by design.
func (o *Observer) Failovers() <-chan string {
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
	o.auth.Update(rf)
	// Validate fills in the operator's defaults.
	spec := rf.DeepCopy()
	_ = spec.Validate()
	s := snapshot{
		rf:               spec,
		uid:              string(rf.UID),
		bootstrap:        spec.Spec.BootstrapNode,
		pvc:              spec.Spec.Redis.Storage.PersistentVolumeClaim != nil,
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
	keys := customconfig.RedisKeys(spec)
	if spec.Spec.Redis.MaxMemory != nil {
		keys = append(keys, "maxmemory", "maxmemory-policy")
	}
	for i := range s.redis {
		p := &s.redis[i]
		wg.Go(func() { o.redisInfo(ctx, p, s.port, keys) })
	}
	for i := range s.sentinels {
		p := &s.sentinels[i]
		wg.Go(func() { o.sentinelInfo(ctx, p) })
	}
	if s.bootstrap != nil && o.source != nil {
		wg.Go(func() { s.sourceReplID, s.sourceOffset, s.sourceErr = o.source.masterPosition(ctx) })
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

// client connects to a pod, authenticating with a's current password, or
// with none for a nil a.
func (o *Observer) client(ip string, port int, a *auth.Source) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  net.JoinHostPort(ip, strconv.Itoa(port)),
		CredentialsProvider:   a.Provider(),
		DialTimeout:           o.timeout,
		ReadTimeout:           o.timeout,
		WriteTimeout:          o.timeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
	})
}

// redisInfo sends INFO to the pod, and CONFIG GET keys.
func (o *Observer) redisInfo(ctx context.Context, p *redisPod, port int, keys []string) {
	if p.IP == "" {
		p.err, p.configErr = errNoIP, errNoIP
		return
	}
	c := o.client(p.IP, port, o.auth)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s, err := c.Info(ctx).Result()
	if err == nil {
		p.info = parseInfo(s)
	}
	p.err = err
	if len(keys) > 0 {
		p.config, p.configErr = ConfigGet(ctx, c, keys)
	}
}

// ConfigGet reads several keys with one CONFIG GET, which Redis and Valkey
// accept from 7.0 on.
func ConfigGet(ctx context.Context, c *redis.Client, keys []string) (map[string]string, error) {
	args := []any{"config", "get"}
	for _, k := range keys {
		args = append(args, k)
	}
	cmd := redis.NewMapStringStringCmd(ctx, args...)
	_ = c.Process(ctx, cmd)
	return cmd.Result()
}

// sentinelInfo asks a Sentinel for the master's address, as clients do,
// and for SENTINEL MASTER mymaster's fields.
func (o *Observer) sentinelInfo(ctx context.Context, p *sentinelPod) {
	if p.IP == "" {
		p.err = errNoIP
		return
	}
	c := o.client(p.IP, sentinelPort, nil)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	addr, err := c.Do(ctx, "SENTINEL", "get-master-addr-by-name", sentinelMasterName).StringSlice()
	if err == nil && len(addr) != 2 {
		err = errors.New("no master known")
	}
	if err != nil {
		p.err = err
		return
	}
	p.master = net.JoinHostPort(addr[0], addr[1])
	p.fields, p.err = SentinelMaster(ctx, c)
}

// SentinelMaster returns SENTINEL MASTER mymaster's fields.
func SentinelMaster(ctx context.Context, c *redis.Client) (map[string]string, error) {
	cmd := redis.NewMapStringStringCmd(ctx, "SENTINEL", "MASTER", sentinelMasterName)
	_ = c.Process(ctx, cmd)
	return cmd.Result()
}

// masterPosition returns the master's replication ID and offset.
func (o *Observer) masterPosition(ctx context.Context) (string, int64, error) {
	v := o.view.Load()
	if v == nil || v.masterIP == "" {
		return "", 0, errNoMaster
	}
	c := o.client(v.masterIP, o.in.Port, o.auth)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s, err := c.Info(ctx, "replication").Result()
	if err != nil {
		return "", 0, err
	}
	i := parseInfo(s)
	return i["master_replid"], i.int("master_repl_offset"), nil
}

func (o *Observer) takeHold() {
	select {
	case h := <-o.holds:
		o.hold, o.holdErr = h, nil
		o.tracker.hold(h.since, h.timeout)
		o.log.Info("convergence window opened", "by", "mutation", "timeout_seconds", h.timeout.Seconds())
	default:
	}
	if o.hold != nil {
		if at := o.hold.appliedAt(); !at.IsZero() {
			o.tracker.apply(at)
		}
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
	o.externalWindow(now)
	all := evaluate(s)
	o.dropInvariants(all)
	var checks []check
	for _, c := range all {
		if o.reportOnly[c.invariant] {
			o.note(now, c)
			continue
		}
		o.findings.WithLabelValues(c.invariant)
		checks = append(checks, c)
	}
	var failing []string
	// A recreated RedisFailover has new Sentinels behind a new Service: its
	// Sentinel path is dropped, with its clients, for a round at least.
	recreated := o.rfUID != "" && o.rfUID != s.uid
	if recreated {
		o.sentinelUp = false
	}
	allOK := true
	for _, c := range checks {
		o.ok.WithLabelValues(c.invariant).Set(gauge(c.err == nil))
		allOK = allOK && c.err == nil
		if c.err != nil {
			failing = append(failing, c.invariant)
		}
		// The Sentinel Service routes to Ready pods only.
		if c.invariant == invSentinelAgreement && c.err == nil && !recreated &&
			podsReady("sentinel", sentinelPods(s), s.sentinelReplicas) == nil {
			o.sentinelUp = true
		}
	}
	o.sentinelUp = o.sentinelUp && s.sentinel
	for _, e := range o.tracker.update(now, checks, converged) {
		o.record(e)
	}
	o.windowOpen.Set(gauge(o.tracker.windowOpen()))
	o.rfHealthy.Set(gauge(s.state == redisfailoverv1.HealthyState))
	o.observeMaster(s)
	o.observeServers(s)
	o.observeOOMKills(s)
	o.observeData(s)
	v := &view{
		at:           now,
		failing:      failing,
		quiet:        allOK && !o.tracker.windowOpen(),
		windowOpen:   o.tracker.windowOpen(),
		master:       o.master.Name,
		sentinelPath: o.sentinelUp,
	}
	if _, err := s.master(); err == nil {
		v.masterIP = o.master.IP
	}
	for _, p := range s.redis {
		if p.IP != "" {
			v.redis = append(v.redis, PodAddr{Name: p.Name, Addr: net.JoinHostPort(p.IP, strconv.Itoa(s.port))})
		}
	}
	o.view.Store(v)
}

// externalWindow holds a window open while something outside the instance
// is expected to disturb it: a mutation stopped the operator, a chaos
// action runs, or, for a bootstrapping instance, the source is converging or without a master.
// A bootstrapping instance's window also opens when the source's master
// changes, as its pods only reach the new one after their link to the old
// one broke, which may be after the source converged; it then closes once
// they all replicate the new master's stream.
func (o *Observer) externalWindow(now time.Time) {
	reason := ""
	switch {
	case o.lock.Disturbance() != "":
		reason = o.lock.Disturbance()
	case o.source != nil:
		v := o.source.view.Load()
		switch {
		case v == nil || v.windowOpen || v.masterIP == "":
			reason = "source converging"
		case v.masterIP != o.sourceMaster:
			if o.sourceMaster != "" {
				o.tracker.extend(now)
				o.log.Info("convergence window opened", "by", "source failover")
			}
			o.sourceMaster = v.masterIP
		}
	}
	if reason != "" {
		if reason != o.external {
			o.log.Info("convergence window opened", "by", reason)
		}
		o.tracker.extend(now)
	}
	o.external = reason
}

// note follows an invariant that is reported but never a finding.
func (o *Observer) note(now time.Time, c check) {
	o.ok.WithLabelValues(c.invariant).Set(gauge(c.err == nil))
	since, violated := o.noted[c.invariant]
	log := o.log.With("invariant", c.invariant, "finding", false)
	switch {
	case c.err != nil && !violated:
		o.noted[c.invariant] = now
		log.Warn("invariant violated", "reason", c.err.Error())
	case c.err == nil && violated:
		delete(o.noted, c.invariant)
		d := now.Sub(since)
		o.violation.WithLabelValues(c.invariant).Observe(d.Seconds())
		log.Info("invariant restored", "duration_seconds", d.Seconds())
	}
}

// dropInvariants forgets the invariants no longer evaluated, like
// sentinel_agreement after Sentinel was switched off.
func (o *Observer) dropInvariants(checks []check) {
	names := make([]string, len(checks))
	for i, c := range checks {
		names[i] = c.invariant
	}
	for _, name := range o.evaluated {
		if slices.Contains(names, name) {
			continue
		}
		o.ok.DeleteLabelValues(name)
		delete(o.noted, name)
		if o.tracker.drop(name) {
			o.log.Info("invariant dropped while violated", "invariant", name)
		}
	}
	o.evaluated = names
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
	if o.rfUID != "" && o.rfUID != s.uid {
		o.recreated = true
	}
	o.rfUID = s.uid
	master, err := s.master()
	lags := map[string]int64{}
	switch {
	case s.bootstrap != nil:
		if s.sourceErr == nil {
			lags = s.bootstrapLags()
		}
	case err == nil:
		if o.master.UID != "" && o.master.UID != master.UID {
			o.failovers.Inc()
			event := config.EventFailover
			// The only pod without a volume, or a new RedisFailover, starts
			// empty.
			if o.recreated || s.redisReplicas == 1 && !s.pvc {
				event = config.EventReset
			}
			o.recreated = false
			o.log.Warn("failover", "from", o.master.Name, "to", master.Name, "event", event)
			select {
			case o.failoverCh <- event:
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
