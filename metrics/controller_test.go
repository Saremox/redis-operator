package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerMetricsKeepTheirNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := NewRecorder("my_metrics", reg)
	ctx := context.Background()

	rec.IncResourceEventQueued(ctx, "redisfailover", false)
	rec.IncResourceEventQueued(ctx, "redisfailover", true)
	rec.ObserveResourceInQueueDuration(ctx, "redisfailover", time.Now())
	rec.ObserveResourceProcessingDuration(ctx, "redisfailover", true, time.Now())
	require.NoError(t, rec.RegisterResourceQueueLengthFunc("redisfailover", func(context.Context) int { return 4 }))

	expected := `
# HELP kooper_controller_event_queue_length Length of the controller resource queue.
# TYPE kooper_controller_event_queue_length gauge
kooper_controller_event_queue_length{controller="redisfailover"} 4
# HELP kooper_controller_queued_events_total Total number of events queued.
# TYPE kooper_controller_queued_events_total counter
kooper_controller_queued_events_total{controller="redisfailover",requeue="false"} 1
kooper_controller_queued_events_total{controller="redisfailover",requeue="true"} 1
`
	assert.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"kooper_controller_event_queue_length", "kooper_controller_queued_events_total"))

	families, err := reg.Gather()
	require.NoError(t, err)
	buckets := map[string][]float64{}
	labels := map[string][]string{}
	for _, mf := range families {
		if mf.GetType().String() != "HISTOGRAM" {
			continue
		}
		m := mf.GetMetric()[0]
		for _, b := range m.GetHistogram().GetBucket() {
			buckets[mf.GetName()] = append(buckets[mf.GetName()], b.GetUpperBound())
		}
		for _, l := range m.GetLabel() {
			labels[mf.GetName()] = append(labels[mf.GetName()], l.GetName()+"="+l.GetValue())
		}
	}
	assert.Equal(t, map[string][]float64{
		"kooper_controller_event_in_queue_duration_seconds":  {.01, .05, .1, .25, .5, 1, 3, 10, 20, 60, 150, 300},
		"kooper_controller_processed_event_duration_seconds": prometheus.DefBuckets,
	}, buckets)
	assert.Equal(t, map[string][]string{
		"kooper_controller_event_in_queue_duration_seconds":  {"controller=redisfailover"},
		"kooper_controller_processed_event_duration_seconds": {"controller=redisfailover", "success=true"},
	}, labels)

	assert.EqualError(t, rec.RegisterResourceQueueLengthFunc("redisfailover", func(context.Context) int { return 0 }),
		`could not register ResourceQueueLengthFunc metrics: duplicate metrics collector registration attempted`)
}

func TestDummyControllerRecorder(t *testing.T) {
	assert.NotPanics(t, func() {
		Dummy.IncResourceEventQueued(context.Background(), "c", false)
		Dummy.ObserveResourceInQueueDuration(context.Background(), "c", time.Now())
		Dummy.ObserveResourceProcessingDuration(context.Background(), "c", true, time.Now())
	})
	assert.NoError(t, Dummy.RegisterResourceQueueLengthFunc("c", func(context.Context) int { return 0 }))
}
