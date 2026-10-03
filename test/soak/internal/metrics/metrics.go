// Package metrics defines the tester's Prometheus metrics and serves them.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "redis_soak"

// instanceLabels start every per-instance label set.
var instanceLabels = []string{"rf", "namespace", "mode"}

type Metrics struct {
	ProbeTotal     *prometheus.CounterVec
	ProbeDuration  *prometheus.HistogramVec
	Writable       *prometheus.GaugeVec
	Readable       *prometheus.GaugeVec
	LastSuccess    *prometheus.GaugeVec
	OutageDuration *prometheus.HistogramVec
	BuildInfo      *prometheus.GaugeVec

	InvariantOK        *prometheus.GaugeVec
	InvariantViolation *prometheus.HistogramVec
	Findings           *prometheus.CounterVec
	Masters            *prometheus.GaugeVec
	Failovers          *prometheus.CounterVec
	ReplicationLag     *prometheus.GaugeVec
	RFHealthy          *prometheus.GaugeVec
	WindowOpen         *prometheus.GaugeVec

	MutationTotal      *prometheus.CounterVec
	MutationConverge   *prometheus.HistogramVec
	PodsRecreated      *prometheus.CounterVec
	MutationInProgress *prometheus.GaugeVec

	WaitAckedReplicas *prometheus.GaugeVec
	OOMRejections     *prometheus.CounterVec
	LostWrites        *prometheus.CounterVec
	UnexpectedLost    *prometheus.CounterVec
	LedgerVerified    *prometheus.CounterVec

	VersionTransition *prometheus.CounterVec
	VersionMixed      *prometheus.HistogramVec

	ChaosTotal           *prometheus.CounterVec
	ChaosConverge        *prometheus.HistogramVec
	ChaosInProgress      *prometheus.GaugeVec
	ChaosOperatorDown    *prometheus.HistogramVec
	ChaosEvictionBlocked prometheus.Histogram
}

// New registers the metrics. Convergence histograms reach up to
// convergenceTimeout.
func New(reg prometheus.Registerer, convergenceTimeout time.Duration) *Metrics {
	labels := func(extra ...string) []string {
		return append(append([]string{}, instanceLabels...), extra...)
	}
	m := &Metrics{
		ProbeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "probe_total",
			Help:      "Probe commands by path, client style, operation and classified result.",
		}, labels("path", "client", "op", "result")),
		ProbeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "probe_duration_seconds",
			Help:      "Probe command latency, including the dial for fresh clients.",
			Buckets:   prometheus.ExponentialBuckets(0.0005, 2, 14),
		}, labels("path", "client", "op")),
		Writable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "writable",
			Help:      "1 if the last write probe succeeded.",
		}, labels("path", "client")),
		Readable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "readable",
			Help:      "1 if the last read probe succeeded.",
		}, labels("path", "client")),
		LastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "last_success_timestamp_seconds",
			Help:      "Unix time of the last successful probe command.",
		}, labels("path", "client", "op")),
		OutageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "outage_duration_seconds",
			Help:      "Time from the first failed probe to the next successful one, by the chaos kind or the instance's mutation kind running when it started, or none.",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800},
		}, labels("path", "client", "event")),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Always 1. The operator version is the image tag of its Deployment.",
		}, []string{"operator_version", "tester_version"}),
		InvariantOK: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "invariant_ok",
			Help:      "1 if the invariant held at the last check.",
		}, labels("invariant")),
		InvariantViolation: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "invariant_violation_seconds",
			Help:      "How long an invariant stayed violated.",
			Buckets:   []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800, 3600},
		}, labels("invariant")),
		Findings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "findings_total",
			Help:      "Invariant violations outside a convergence window.",
		}, labels("invariant")),
		Masters: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "masters",
			Help:      "Redis pods reporting the master role. 1 is right.",
		}, labels()),
		Failovers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "failovers_total",
			Help:      "Changes of the master's identity.",
		}, labels()),
		ReplicationLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "replication_lag_bytes",
			Help:      "The master's replication offset minus the replica's.",
		}, labels("pod")),
		RFHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "rf_healthy",
			Help:      "1 if the RedisFailover's status.state is Healthy.",
		}, labels()),
		WindowOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "window_open",
			Help:      "1 while a convergence window is open: violations are expected, not findings.",
		}, labels()),
		MutationTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "mutation_total",
			Help:      "Mutations by kind and result: converged, timeout, rejected or skipped.",
		}, labels("kind", "result")),
		MutationConverge: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "mutation_converge_seconds",
			Help:      "Time from applying a mutation to its convergence window closing.",
			Buckets:   convergeBuckets(convergenceTimeout),
		}, labels("kind")),
		PodsRecreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "pods_recreated_total",
			Help:      "Redis pods a mutation replaced by a new pod of the same name.",
		}, labels("kind")),
		MutationInProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "mutation_in_progress",
			Help:      "1 while a mutation of the kind is converging.",
		}, labels("kind")),
		WaitAckedReplicas: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "wait_acked_replicas",
			Help:      "Replicas that acknowledged the last sampled probe write within the WAIT timeout.",
		}, labels()),
		OOMRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "oom_rejections_total",
			Help:      "Fill and ledger writes rejected with OOM.",
		}, labels()),
		LostWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "lost_writes_total",
			Help:      "Acknowledged writes found missing or wrong, by the mutation kind or failover they were verified after.",
		}, labels("event")),
		UnexpectedLost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "unexpected_lost_writes_total",
			Help:      "Lost writes of an event that must lose none, and source writes that a caught-up bootstrap pod misses.",
		}, labels("event")),
		LedgerVerified: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "ledger_verified_total",
			Help:      "Verifications of the acknowledged writes, by the mutation kind or failover they ran after.",
		}, labels("event")),
		VersionTransition: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "version_transition_total",
			Help:      "Version changes by the configured names of the versions, the edge's expectation and the result: ok, failed_safe or failed_unsafe.",
		}, labels("from", "to", "expect", "result")),
		VersionMixed: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "version_mixed_seconds",
			Help:      "How long an instance ran two versions of its redis or sentinel component: from the first pod on the new version until the last pod on the old one was gone.",
			Buckets:   []float64{10, 20, 30, 45, 60, 90, 120, 180, 300, 600, 900, 1200, 1800, 3600},
		}, labels("from", "to", "component")),
		ChaosTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "chaos_total",
			Help:      "Chaos actions by kind and result: converged, timeout, failed or skipped.",
		}, []string{"kind", "result"}),
		ChaosConverge: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "chaos_converge_seconds",
			Help:      "Time from a chaos action's disruption until every instance converged: a restart's delete, each upgrade's helm upgrade, a drain's cordon.",
			Buckets:   convergeBuckets(convergenceTimeout),
		}, []string{"kind"}),
		ChaosInProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "chaos_in_progress",
			Help:      "1 while a chaos action of the kind runs.",
		}, []string{"kind"}),
		ChaosOperatorDown: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "chaos_operator_down_seconds",
			Help:      "Time without a leading operator: from the old leader's deletion until a new one acquired the lease.",
			Buckets:   []float64{1, 2, 5, 10, 15, 20, 30, 45, 60, 90, 120, 300},
		}, []string{"kind"}),
		ChaosEvictionBlocked: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "chaos_eviction_blocked_seconds",
			Help:      "How long a PodDisruptionBudget blocked a drain's eviction of a pod.",
			Buckets:   []float64{1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
		}),
	}
	reg.MustRegister(
		m.ProbeTotal, m.ProbeDuration, m.Writable, m.Readable, m.LastSuccess, m.OutageDuration, m.BuildInfo,
		m.InvariantOK, m.InvariantViolation, m.Findings, m.Masters, m.Failovers, m.ReplicationLag, m.RFHealthy, m.WindowOpen,
		m.MutationTotal, m.MutationConverge, m.PodsRecreated, m.MutationInProgress,
		m.WaitAckedReplicas, m.OOMRejections, m.LostWrites, m.UnexpectedLost, m.LedgerVerified,
		m.VersionTransition, m.VersionMixed,
		m.ChaosTotal, m.ChaosConverge, m.ChaosInProgress, m.ChaosOperatorDown, m.ChaosEvictionBlocked,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func convergeBuckets(timeout time.Duration) []float64 {
	var buckets []float64
	for _, b := range []float64{1, 2, 5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300, 600, 1200, 1800, 3600} {
		if b >= timeout.Seconds() {
			break
		}
		buckets = append(buckets, b)
	}
	return append(buckets, timeout.Seconds())
}

// DeletePath deletes the probe series of an instance's path.
func (m *Metrics) DeletePath(rf, namespace, path string) {
	labels := prometheus.Labels{"rf": rf, "namespace": namespace, "path": path}
	for _, v := range []interface{ DeletePartialMatch(prometheus.Labels) int }{
		m.ProbeTotal, m.ProbeDuration, m.Writable, m.Readable, m.LastSuccess, m.OutageDuration,
	} {
		v.DeletePartialMatch(labels)
	}
}

// Handler serves /metrics and /healthz.
func Handler(g prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
