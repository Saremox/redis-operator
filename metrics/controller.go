package metrics

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The controller metrics keep the names they had when the operator was built
// on kooper, so existing dashboards and alerts keep working.
const controllerMetricsNamespace = "kooper"

// ControllerRecorder records the metrics of the operator's reconcile queue.
type ControllerRecorder interface {
	IncResourceEventQueued(ctx context.Context, controller string, isRequeue bool)
	ObserveResourceInQueueDuration(ctx context.Context, controller string, queuedAt time.Time)
	ObserveResourceProcessingDuration(ctx context.Context, controller string, success bool, startProcessingAt time.Time)
	RegisterResourceQueueLengthFunc(controller string, f func(context.Context) int) error
}

type controllerRecorder struct {
	reg                    prometheus.Registerer
	queuedEventsTotal      *prometheus.CounterVec
	inQueueEventDuration   *prometheus.HistogramVec
	processedEventDuration *prometheus.HistogramVec
}

func newControllerRecorder(reg prometheus.Registerer) *controllerRecorder {
	r := &controllerRecorder{
		reg: reg,
		queuedEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: controllerMetricsNamespace,
			Subsystem: promControllerSubsystem,
			Name:      "queued_events_total",
			Help:      "Total number of events queued.",
		}, []string{"controller", "requeue"}),
		inQueueEventDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: controllerMetricsNamespace,
			Subsystem: promControllerSubsystem,
			Name:      "event_in_queue_duration_seconds",
			Help:      "The duration of an event in the queue.",
			Buckets:   []float64{.01, .05, .1, .25, .5, 1, 3, 10, 20, 60, 150, 300},
		}, []string{"controller"}),
		processedEventDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: controllerMetricsNamespace,
			Subsystem: promControllerSubsystem,
			Name:      "processed_event_duration_seconds",
			Help:      "The duration for an event to be processed.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"controller", "success"}),
	}
	reg.MustRegister(r.queuedEventsTotal, r.inQueueEventDuration, r.processedEventDuration)
	return r
}

func (r *controllerRecorder) IncResourceEventQueued(_ context.Context, controller string, isRequeue bool) {
	r.queuedEventsTotal.WithLabelValues(controller, strconv.FormatBool(isRequeue)).Inc()
}

func (r *controllerRecorder) ObserveResourceInQueueDuration(_ context.Context, controller string, queuedAt time.Time) {
	r.inQueueEventDuration.WithLabelValues(controller).Observe(time.Since(queuedAt).Seconds())
}

func (r *controllerRecorder) ObserveResourceProcessingDuration(_ context.Context, controller string, success bool, startProcessingAt time.Time) {
	r.processedEventDuration.WithLabelValues(controller, strconv.FormatBool(success)).Observe(time.Since(startProcessingAt).Seconds())
}

func (r *controllerRecorder) RegisterResourceQueueLengthFunc(controller string, f func(context.Context) int) error {
	err := r.reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace:   controllerMetricsNamespace,
		Subsystem:   promControllerSubsystem,
		Name:        "event_queue_length",
		Help:        "Length of the controller resource queue.",
		ConstLabels: prometheus.Labels{"controller": controller},
	}, func() float64 { return float64(f(context.Background())) }))
	if err != nil {
		return fmt.Errorf("could not register ResourceQueueLengthFunc metrics: %w", err)
	}
	return nil
}

type dummyControllerRecorder struct{}

func (dummyControllerRecorder) IncResourceEventQueued(context.Context, string, bool)              {}
func (dummyControllerRecorder) ObserveResourceInQueueDuration(context.Context, string, time.Time) {}
func (dummyControllerRecorder) ObserveResourceProcessingDuration(context.Context, string, bool, time.Time) {
}
func (dummyControllerRecorder) RegisterResourceQueueLengthFunc(string, func(context.Context) int) error {
	return nil
}
