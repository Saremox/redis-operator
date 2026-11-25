package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// serviceKind describes one Service that the operator writes. The cases of
// TestGeneratedServices change the name, the namespace, the labels and the
// annotations the same way for each kind, so a table replaces one copy of the
// test for each kind.
type serviceKind struct {
	name string
	// defaults is the Service for a RedisFailover with the given name and namespace.
	defaults       func(rfName, ns string) corev1.Service
	ensure         func(c *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, labels map[string]string) error
	setAnnotations func(rf *redisfailoverv1.RedisFailover, annotations map[string]string)
}

func componentLabels(component, rfName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/component": component,
		"app.kubernetes.io/name":      rfName,
		"app.kubernetes.io/part-of":   "redis-failover",
	}
}

func roleLabels(rfName, role string) map[string]string {
	labels := componentLabels("redis", rfName)
	labels["redisfailovers-role"] = role
	return labels
}

var owners = []metav1.OwnerReference{{Name: "testing"}}

func serviceKinds() []serviceKind {
	redisAnnotations := func(rf *redisfailoverv1.RedisFailover, a map[string]string) { rf.Spec.Redis.ServiceAnnotations = a }
	roleService := func(prefix, role string) func(rfName, ns string) corev1.Service {
		return func(rfName, ns string) corev1.Service {
			return corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: prefix + rfName, Namespace: ns, Labels: roleLabels(rfName, role), OwnerReferences: owners},
				Spec: corev1.ServiceSpec{
					Type:     corev1.ServiceTypeClusterIP,
					Selector: roleLabels(rfName, role),
					Ports:    []corev1.ServicePort{{Name: "redis", Port: 6379, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("redis")}},
				},
			}
		}
	}
	return []serviceKind{
		{
			name: "redis",
			defaults: func(rfName, ns string) corev1.Service {
				return corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name: "rfr-" + rfName, Namespace: ns, Labels: componentLabels("redis", rfName), OwnerReferences: owners,
						Annotations: map[string]string{"prometheus.io/scrape": "true", "prometheus.io/path": "/metrics", "prometheus.io/port": "9121"},
					},
					Spec: corev1.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone, Selector: componentLabels("redis", rfName),
						Ports: []corev1.ServicePort{{Name: "http-metrics", Port: 9121, Protocol: corev1.ProtocolTCP}},
					},
				}
			},
			ensure: func(c *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, labels map[string]string) error {
				return c.EnsureRedisService(rf, labels, owners)
			},
			setAnnotations: redisAnnotations,
		},
		{
			name:     "replicas",
			defaults: roleService("rfrs-", "slave"),
			ensure: func(c *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, labels map[string]string) error {
				return c.EnsureRedisSlaveService(rf, labels, owners)
			},
			setAnnotations: redisAnnotations,
		},
		{
			name:     "master",
			defaults: roleService("rfrm-", "master"),
			ensure: func(c *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, labels map[string]string) error {
				return c.EnsureRedisMasterService(rf, labels, owners)
			},
			setAnnotations: redisAnnotations,
		},
		{
			name: "sentinel",
			defaults: func(rfName, ns string) corev1.Service {
				return corev1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "rfs-" + rfName, Namespace: ns, Labels: componentLabels("sentinel", rfName), OwnerReferences: owners},
					Spec: corev1.ServiceSpec{
						Selector: componentLabels("sentinel", rfName),
						Ports:    []corev1.ServicePort{{Name: "sentinel", Port: 26379, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(26379)}},
					},
				}
			},
			ensure: func(c *rfservice.RedisFailoverKubeClient, rf *redisfailoverv1.RedisFailover, labels map[string]string) error {
				return c.EnsureSentinelService(rf, labels, owners)
			},
			setAnnotations: func(rf *redisfailoverv1.RedisFailover, a map[string]string) { rf.Spec.Sentinel.ServiceAnnotations = a },
		},
	}
}

// generatedService returns the Service that ensure writes for rf.
func generatedService(t *testing.T, kind serviceKind, rf *redisfailoverv1.RedisFailover, labels map[string]string) corev1.Service {
	t.Helper()
	var got corev1.Service
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateService", rf.Namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		got = *args.Get(1).(*corev1.Service)
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, kind.ensure(client, rf, labels))
	return got
}

func TestGeneratedServices(t *testing.T) {
	cases := []struct {
		name        string
		rfName      string
		rfNamespace string
		labels      map[string]string
		annotations map[string]string
	}{
		{name: "with defaults", rfName: name, rfNamespace: namespace},
		{name: "with Name provided", rfName: "custom-name", rfNamespace: namespace},
		{name: "with Namespace provided", rfName: name, rfNamespace: "custom-namespace"},
		{name: "with Labels provided", rfName: name, rfNamespace: namespace, labels: map[string]string{"some": "label"}},
		{name: "with Annotations provided", rfName: name, rfNamespace: namespace, annotations: map[string]string{"some": "annotation"}},
	}
	for _, kind := range serviceKinds() {
		for _, test := range cases {
			t.Run(kind.name+" "+test.name, func(t *testing.T) {
				rf := generateRF()
				rf.Name, rf.Namespace = test.rfName, test.rfNamespace
				rf.Spec.Redis.Port = 6379
				kind.setAnnotations(rf, test.annotations)

				want := kind.defaults(test.rfName, test.rfNamespace)
				for k, v := range test.labels {
					want.Labels[k] = v
				}
				if len(test.annotations) > 0 && want.Annotations == nil {
					want.Annotations = map[string]string{}
				}
				for k, v := range test.annotations {
					want.Annotations[k] = v
				}

				assert.Equal(t, want, generatedService(t, kind, rf, test.labels))
			})
		}
	}
}

func TestRedisServiceUsesTheExporterPort(t *testing.T) {
	rf := generateRF()
	rf.Spec.Redis.Exporter.Port = 9200

	want := serviceKinds()[0].defaults(name, namespace)
	want.Annotations["prometheus.io/port"] = "9200"
	want.Spec.Ports[0].Port = 9200

	assert.Equal(t, want, generatedService(t, serviceKinds()[0], rf, nil))
}
