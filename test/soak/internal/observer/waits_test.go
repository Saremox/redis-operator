package observer

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/metrics"
)

// The report says whether the redis server waits for its replicas on SIGTERM,
// from the image of the RedisFailover. Redis 6.2 does not. An image that is no
// configured version does.
func TestReportWaits(t *testing.T) {
	cfg, err := config.Parse([]byte(`
versions:
  - {name: redis-6.2, image: "redis:6.2.24-alpine"}
  - {name: redis-7.2, image: "redis:7.2.16-alpine"}
  - {name: valkey-8, image: "valkey/valkey:8.1.10-alpine"}
instances: [{name: x, namespace: ns}]
`))
	if err != nil {
		t.Fatal(err)
	}
	for image, want := range map[string]bool{
		"redis:6.2.24-alpine":         false,
		"redis:7.2.16-alpine":         true,
		"valkey/valkey:8.1.10-alpine": true,
		"redis:6-alpine":              true,
	} {
		rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
		rf.Spec.Redis.Image = image
		o := New(cfg.Instances[0], cfg, fake.NewClientset(), rffake.NewSimpleClientset(rf),
			auth.New(nil), nil, metrics.New(prometheus.NewRegistry(), time.Minute), slog.New(slog.NewTextHandler(io.Discard, nil)))
		s, _, err := o.collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		o.apply(at(0), s, 1, false)
		if got := o.Report().Waits; got != want {
			t.Errorf("%s: Waits %t, want %t", image, got, want)
		}
	}
}
