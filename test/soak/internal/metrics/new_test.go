package metrics

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// describe returns, for each field of m in order, its descriptions and, for a
// histogram, its buckets.
func describe(t *testing.T, m *Metrics) string {
	t.Helper()
	var b strings.Builder
	labelsRE := regexp.MustCompile(`variableLabels: \{([^}]*)\}`)
	v := reflect.ValueOf(m).Elem()
	for i := range v.NumField() {
		c, ok := reflect.TypeAssert[prometheus.Collector](v.Field(i))
		if !ok {
			t.Fatalf("field %s is not a collector", v.Type().Field(i).Name)
		}
		descs := make(chan *prometheus.Desc, 4)
		c.Describe(descs)
		close(descs)
		for d := range descs {
			fmt.Fprintf(&b, "%s: %s\n", v.Type().Field(i).Name, d)
			n := 0
			if l := labelsRE.FindStringSubmatch(d.String()); len(l) > 1 && l[1] != "" {
				n = len(strings.Split(l[1], ","))
			}
			values := make([]string, n)
			var h prometheus.Observer
			switch x := c.(type) {
			case *prometheus.HistogramVec:
				h = x.WithLabelValues(values...)
			case prometheus.Histogram:
				h = x
			}
			if h == nil {
				continue
			}
			h.Observe(0)
			ch := make(chan prometheus.Metric, 1)
			c.Collect(ch)
			var out dto.Metric
			if err := (<-ch).Write(&out); err != nil {
				t.Fatal(err)
			}
			bounds := make([]string, 0, len(out.GetHistogram().GetBucket()))
			for _, bk := range out.GetHistogram().GetBucket() {
				bounds = append(bounds, fmt.Sprint(bk.GetUpperBound()))
			}
			fmt.Fprintf(&b, "  buckets: %s\n", strings.Join(bounds, " "))
		}
	}
	return b.String()
}

// New registers the same metrics with the same names, help texts, labels and
// buckets. testdata/new.golden holds them.
func TestNewDescriptions(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, 7*time.Minute)
	got := describe(t, m)
	const golden = "testdata/new.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("descriptions differ from %s:\n%s", golden, got)
	}
	// New registers each field.
	v := reflect.ValueOf(m).Elem()
	for i := range v.NumField() {
		var already prometheus.AlreadyRegisteredError
		if err := reg.Register(v.Field(i).Interface().(prometheus.Collector)); !errors.As(err, &already) {
			t.Errorf("%s: %v", v.Type().Field(i).Name, err)
		}
	}
}

// New registers the runtime metrics that show a stall of the tester process:
// the GC pauses, the scheduler latencies, the memory classes, the goroutines
// and the process CPU time.
func TestNewRuntimeMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	New(reg, time.Minute)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}
	for _, name := range []string{
		"go_gc_pauses_seconds", "go_sched_latencies_seconds", "go_memory_classes_heap_objects_bytes",
		"go_sched_goroutines_goroutines", "go_goroutines", "process_cpu_seconds_total",
	} {
		if !got[name] {
			t.Errorf("%s is not registered", name)
		}
	}
}
