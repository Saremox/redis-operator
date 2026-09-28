// Package metrics defines the tester's Prometheus metrics and serves them.
package metrics

import (
	"net/http"

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
}

func New(reg prometheus.Registerer) *Metrics {
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
			Help:      "Time from the first failed probe to the next successful one.",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800},
		}, labels("path", "client")),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Always 1. The operator version is the image tag of its Deployment.",
		}, []string{"operator_version", "tester_version"}),
	}
	reg.MustRegister(
		m.ProbeTotal, m.ProbeDuration, m.Writable, m.Readable, m.LastSuccess, m.OutageDuration, m.BuildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
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
