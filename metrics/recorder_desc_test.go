package metrics

import (
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// descRegisterer keeps the descriptions of the registered collectors, in the
// order of the registration.
type descRegisterer struct {
	descs []string
}

func (d *descRegisterer) Register(c prometheus.Collector) error {
	ch := make(chan *prometheus.Desc, 16)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	for desc := range ch {
		d.descs = append(d.descs, desc.String())
	}
	return nil
}

func (d *descRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		_ = d.Register(c)
	}
}

func (d *descRegisterer) Unregister(prometheus.Collector) bool { return false }

// TestNewRecorderRegistersTheMetrics pins the name, the help text and the
// labels of each metric of NewRecorder, and the order of the registration.
func TestNewRecorderRegistersTheMetrics(t *testing.T) {
	reg := &descRegisterer{}
	NewRecorder("desc_test", reg)

	want, err := os.ReadFile("testdata/recorder_descs.txt")
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(want)), strings.Join(reg.descs, "\n"))
}
