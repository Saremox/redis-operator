// Package observer checks every few seconds that an instance is in the
// state its spec asks for, and reports each invariant and its violations.
package observer

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/autoscale"
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

// Observer checks the invariants of one instance.
type Observer struct {
	in       config.Instance
	versions *config.Config
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

	tracker *tracker
	holds   chan *Hold
	hold    *Hold
	// mutating is set from the call of Hold until the window of the
	// mutation ends, before the round that takes the hold.
	mutating   atomic.Bool
	holdErr    error
	view       atomic.Pointer[view]
	generation int64
	rfUID      string
	// recreated is set when the RedisFailover was recreated, until the
	// failover that follows.
	recreated bool
	master    pod
	// synced are the redis pods, by UID, that were the master or a replica
	// with its link up: they hold the data.
	synced  map[string]bool
	lagPods map[string]bool
	oomSeen map[string]bool
	// started is when the tester started. An earlier run counted the OOM
	// kills before it.
	started    time.Time
	failoverCh chan string
	// evaluated are the invariants of the last round.
	evaluated []string
	// external is why a window is held open from outside the instance;
	// sourceMaster is the source's master a bootstrapping instance last
	// saw.
	external     string
	sourceMaster string
	sentinelUp   bool
	// reportOnly are the invariants that are never findings. noted is when
	// each of them was violated.
	reportOnly map[string]bool
	noted      map[string]time.Time
	// judge follows the autoscaler steps of an instance with autoscale.
	// mutation tells what the running mutation may do to the pods.
	judge    *autoscale.Judge
	mutation func() (autoscale.Allow, bool)
	// fullSince is when the master became full under noeviction, zero when it
	// is not, and fullFound whether that became a finding after fullLimit.
	// maxmemory is the last maxmemory of each redis pod, by UID.
	fullSince time.Time
	fullFound bool
	fullLimit time.Duration
	maxmemory map[string]int64
	// now times the rounds and the reads. dial replaces the dialer of the
	// pod clients when it is set.
	now  func() time.Time
	dial func(context.Context, string, string) (net.Conn, error)

	ok         *prometheus.GaugeVec
	violation  prometheus.ObserverVec
	findings   *prometheus.CounterVec
	masters    prometheus.Gauge
	failovers  prometheus.Counter
	lag        *prometheus.GaugeVec
	rfHealthy  prometheus.Gauge
	windowOpen prometheus.Gauge
	steps      *prometheus.CounterVec
	roundTime  prometheus.Observer
	lateness   prometheus.Observer
}

// New returns the Observer of an instance.
func New(in config.Instance, cfg *config.Config, kube kubernetes.Interface, rfs versioned.Interface, a *auth.Source, lock *global.Lock, m *metrics.Metrics, log *slog.Logger) *Observer {
	labels := prometheus.Labels{"rf": in.Name, "namespace": in.Namespace, "mode": string(in.Mode)}
	o := &Observer{
		in:         in,
		versions:   cfg,
		interval:   cfg.Observer.Interval.Duration,
		timeout:    cfg.Probe.Timeout.Duration,
		kube:       kube,
		rfs:        rfs,
		auth:       a,
		lock:       lock,
		log:        log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
		tracker:    newTracker(cfg.Observer.ConvergenceTimeout.Duration),
		fullLimit:  cfg.Observer.NoevictionFullLimit.Duration,
		holds:      make(chan *Hold, 1),
		lagPods:    map[string]bool{},
		failoverCh: make(chan string, 1),
		reportOnly: map[string]bool{invReplicaReadyWithoutData: !cfg.Observer.ReplicaReadyWithoutDataFinding()},
		noted:      map[string]time.Time{},
		started:    time.Now().Truncate(time.Second),
		now:        time.Now,
		ok:         m.InvariantOK.MustCurryWith(labels),
		violation:  m.InvariantViolation.MustCurryWith(labels),
		findings:   m.Findings.MustCurryWith(labels),
		masters:    m.Masters.With(labels),
		failovers:  m.Failovers.With(labels),
		lag:        m.ReplicationLag.MustCurryWith(labels),
		rfHealthy:  m.RFHealthy.With(labels),
		windowOpen: m.WindowOpen.With(labels),
		steps:      m.AutoscaleSteps.MustCurryWith(labels),
		roundTime:  m.ObserverRound.With(labels),
		lateness:   m.ObserverLateness.With(labels),
	}
	// Alerts use the increase of findings_total, which does not show a series
	// that starts at 1. A change by hand can give each instance maxMemory, so
	// the series of the maxmemory judges exist for each instance.
	for _, inv := range []string{invOOMKilled, invNoevictionFull, invMaxmemoryBelowUsed} {
		o.findings.WithLabelValues(inv)
	}
	if in.Autoscale != nil {
		o.judge = autoscale.NewJudge()
		for _, j := range autoscale.Judges {
			o.findings.WithLabelValues(j)
		}
		for _, r := range autoscale.Results {
			o.steps.WithLabelValues(r)
		}
	}
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

// Applied starts the timeout of the window. Call it when the mutation is
// applied.
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
	failing []string
	// ephemeral is an instance whose only redis pod has no volume.
	ephemeral bool
	volumes   bool
	waits     bool
	master    string
	masterIP  string
	// sentinelPath is whether the instance runs Sentinels that are Ready
	// and agree on the master, so it can be probed through them.
	sentinelPath bool
	redis        []PodAddr
	// noevictionFull is a master that refuses writes by design, see
	// NoevictionFull.
	noevictionFull bool
}

// PodAddr is a redis pod's address.
type PodAddr struct {
	Name string
	Addr string
}

// Hold opens a convergence window for a mutation before the mutation is
// applied. The window closes when converged returns nil, all invariants hold
// and the minimum dwell passed after Applied, or at timeout after Applied.
// Only one hold can wait at a time. A new hold takes the window of the
// previous hold, whose Done then never receives.
func (o *Observer) Hold(timeout time.Duration, converged func(context.Context) error) *Hold {
	h := &Hold{since: time.Now(), timeout: timeout, converged: converged, done: make(chan bool, 1)}
	o.mutating.Store(true)
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
	// Quiet is true when no convergence window is open and all invariants
	// hold.
	Quiet bool
	// Failing are the violated invariants that may be findings.
	Failing []string
	// Ephemeral is an instance whose only redis pod has no volume: losing
	// the pod loses the data by design, a reset.
	Ephemeral bool
	// Volumes is an instance whose redis pods keep their data on volumes.
	Volumes bool
	// Waits is an instance whose redis server waits for its replicas on
	// SIGTERM.
	Waits bool
}

// Report returns what the last round found, a zero Report before the first.
func (o *Observer) Report() Report {
	v := o.view.Load()
	if v == nil {
		return Report{}
	}
	return Report{At: v.at, Quiet: v.quiet, Failing: v.failing, Ephemeral: v.ephemeral, Volumes: v.volumes, Waits: v.waits}
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

// Run collects and checks a snapshot of the instance at each
// observer.interval, until ctx is done.
func (o *Observer) Run(ctx context.Context) {
	t := time.NewTicker(o.interval)
	defer t.Stop()
	due := o.now()
	for {
		end := o.round(ctx, due)
		select {
		case <-ctx.Done():
			return
		case due = <-t.C:
		}
		// After a round longer than the interval, the ticker gives the
		// first tick that the round missed. The round itself caused that
		// delay.
		if end.After(due) {
			due = end
		}
	}
}

// round collects and checks one snapshot, and returns when it ended. due is
// when the round was due.
func (o *Observer) round(ctx context.Context, due time.Time) time.Time {
	start := o.now()
	s, generation, err := o.collect(ctx)
	collected := o.now()
	if err != nil && ctx.Err() == nil {
		o.log.Warn("observing", "error", err.Error())
	} else if err == nil {
		// A hold is taken only after the snapshot is collected, so the
		// window is open before any snapshot of the mutation is judged.
		o.takeHold()
		o.apply(time.Now(), s, generation, o.converged(ctx))
	}
	end := o.now()
	o.timeRound(ctx, due, start, collected, end, s)
	return end
}

func (o *Observer) collect(ctx context.Context) (snapshot, int64, error) {
	start := o.now()
	// A failed read of the API keeps its time, because a slow API is a cause
	// of a slow round.
	failed := func(err error) (snapshot, int64, error) {
		return snapshot{kubeTime: o.now().Sub(start)}, 0, err
	}
	rf, err := o.rfs.DatabasesV1().RedisFailovers(o.in.Namespace).Get(ctx, o.in.Name, metav1.GetOptions{})
	if err != nil {
		return failed(err)
	}
	o.auth.Update(rf)
	// Validate applies the operator defaults.
	spec := rf.DeepCopy()
	_ = spec.Validate()
	s := snapshot{
		rf:               spec,
		uid:              string(rf.UID),
		bootstrap:        spec.Spec.BootstrapNode,
		pvc:              spec.Spec.Redis.Storage.PersistentVolumeClaim != nil,
		waits:            o.versions.WaitsForReplicas(spec.Spec.Redis.Image),
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
		return failed(err)
	}
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	s.redis, s.sentinels = snapshotPods(pods.Items)

	eps, err := o.kube.DiscoveryV1().EndpointSlices(o.in.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=rfrm-" + o.in.Name,
	})
	if err != nil {
		return failed(err)
	}
	s.endpoints = readyAddresses(eps.Items)
	read := o.now()
	s.kubeTime = read.Sub(start)
	o.readPods(ctx, &s)
	s.readTime = o.now().Sub(read)
	return s, rf.Generation, nil
}

// snapshotPods returns the redis pods and the Sentinel pods, in the order of
// pods.
func snapshotPods(pods []corev1.Pod) (redisPods []redisPod, sentinels []sentinelPod) {
	for i := range pods {
		p := &pods[i]
		switch p.Labels["app.kubernetes.io/component"] {
		case "redis":
			redisPods = append(redisPods, redisPod{pod: newPod(p), limit: maxmem.RedisLimit(p), restarts: restarts(p, "redis")})
		case "sentinel":
			sentinels = append(sentinels, sentinelPod{pod: newPod(p)})
		}
	}
	return redisPods, sentinels
}

// readyAddresses returns each address of the ready endpoints once, in the
// order of the slices.
func readyAddresses(items []discoveryv1.EndpointSlice) []string {
	var addrs []string
	for _, slice := range items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				if !slices.Contains(addrs, addr) {
					addrs = append(addrs, addr)
				}
			}
		}
	}
	return addrs
}

// readPods asks every pod of s, and the master of the source of a
// bootstrapping instance, in parallel.
func (o *Observer) readPods(ctx context.Context, s *snapshot) {
	var wg sync.WaitGroup
	keys := customconfig.RedisKeys(s.rf)
	if s.rf.Spec.Redis.MaxMemory != nil {
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
		wg.Go(func() { s.sourceReplID, s.sourceOffset, s.sourceErr = o.masterPosition(ctx) })
	}
	wg.Wait()
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
	changed := generation != o.generation
	// The first round, for example after a restart of the tester, sees the
	// generation but no change of it.
	userChange := changed && o.generation != 0
	if changed {
		o.tracker.openWindow(now)
		o.log.Info("convergence window opened", "generation", generation, "previous_generation", o.generation)
		o.generation = generation
	}
	o.externalWindow(now)
	o.judgeAutoscale(now, s, generation, userChange)
	all := evaluate(s)
	o.dropInvariants(all)
	checks := o.findingChecks(now, all)
	// A recreated RedisFailover has new Sentinels behind a new Service: its
	// Sentinel path is dropped, with its clients, for a round at least.
	recreated := o.rfUID != "" && o.rfUID != s.uid
	if recreated {
		o.sentinelUp = false
	}
	failing, allOK := o.recordChecks(s, checks, recreated)
	o.sentinelUp = o.sentinelUp && s.sentinel
	for _, e := range o.tracker.update(now, checks, converged) {
		o.record(e)
	}
	o.windowOpen.Set(gauge(o.tracker.windowOpen()))
	o.rfHealthy.Set(gauge(s.state == redisfailoverv1.HealthyState))
	o.observeMaster(s)
	o.observeOOMKills(s)
	o.judgeMaxmemory(s)
	v := &view{
		at:           now,
		failing:      failing,
		ephemeral:    s.bootstrap == nil && s.redisReplicas == 1 && !s.pvc,
		volumes:      s.pvc,
		waits:        s.waits,
		quiet:        allOK && !o.tracker.windowOpen(),
		windowOpen:   o.tracker.windowOpen(),
		master:       o.master.Name,
		sentinelPath: o.sentinelUp,
	}
	v.noevictionFull = o.judgeNoeviction(now, s)
	if _, err := s.master(); err == nil {
		v.masterIP = o.master.IP
	}
	v.redis = redisAddrs(s)
	o.view.Store(v)
}

// findingChecks notes the checks of the report-only invariants, and returns
// the others, whose violations are findings.
func (o *Observer) findingChecks(now time.Time, all []check) []check {
	var checks []check
	for _, c := range all {
		if o.reportOnly[c.invariant] {
			o.note(now, c)
			continue
		}
		o.findings.WithLabelValues(c.invariant)
		checks = append(checks, c)
	}
	return checks
}

// recordChecks sets the gauge of each check, and the Sentinel path once the
// Sentinels agree. It returns the failing invariants, and whether all hold.
func (o *Observer) recordChecks(s snapshot, checks []check, recreated bool) (failing []string, allOK bool) {
	allOK = true
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
	return failing, allOK
}

// redisAddrs returns the address of each redis pod with an IP.
func redisAddrs(s snapshot) []PodAddr {
	var addrs []PodAddr
	for _, p := range s.redis {
		if p.IP != "" {
			addrs = append(addrs, PodAddr{Name: p.Name, Addr: net.JoinHostPort(p.IP, strconv.Itoa(s.port))})
		}
	}
	return addrs
}

// externalWindow keeps a window open while a disturbance outside the instance
// continues: a mutation stopped the operator, a chaos action runs, or the
// source of a bootstrapping instance is mutated, converges or has no master. A change of
// the source master also opens a window for a bootstrapping instance. Its pods
// connect to the new master only after their link to the old master breaks,
// which can be after the source converged. The window closes when all pods
// replicate the stream of the new master.
func (o *Observer) externalWindow(now time.Time) {
	reason := ""
	switch {
	case o.lock.Disturbance() != "":
		reason = o.lock.Disturbance()
	case o.source != nil:
		v := o.source.view.Load()
		switch {
		case v == nil || v.windowOpen || v.masterIP == "" || o.source.mutating.Load():
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
		o.mutating.Store(false)
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
		o.followMaster(s, master)
		lags = s.lags(master)
	}
	o.synced = synced(s, o.synced)
	o.setLags(lags)
}

// followMaster takes the master of the round, and reports a failover when it
// is another pod.
func (o *Observer) followMaster(s snapshot, master *redisPod) {
	if o.master.UID != "" && o.master.UID != master.UID {
		o.failovers.Inc()
		event := config.EventFailover
		// A new RedisFailover starts empty. So does a new only pod without
		// a volume that replaced the master: it never held the data. A pod
		// that replicated from the master, for example the pod that a
		// scale-down left, has the data.
		if o.recreated || s.redisReplicas == 1 && !s.pvc && !o.synced[master.UID] {
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
}

// setLags sets the replication lag of each pod in lags, and removes the
// series of the other pods.
func (o *Observer) setLags(lags map[string]int64) {
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

// synced returns the pods of the snapshot that held the data in this round
// or an earlier one: the master, or a replica with its link up.
func synced(s snapshot, before map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, r := range s.redis {
		if before[r.UID] || r.info.role() == roleMaster || r.info.role() == roleReplica && r.info["master_link_status"] == "up" {
			out[r.UID] = true
		}
	}
	return out
}

func gauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
