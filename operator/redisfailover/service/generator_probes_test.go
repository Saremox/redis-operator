package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func execProbe(delay, timeout, failures, period int32, command ...string) *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: delay, TimeoutSeconds: timeout, FailureThreshold: failures, PeriodSeconds: period,
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}},
	}
}

// probes are the three probes of the first container.
type probes struct{ liveness, readiness, startup *corev1.Probe }

// generatedProbes returns the probes of the first container that the
// operator generates for the component.
type probeComponent struct {
	name string
	// set gives the custom probes to the RedisFailover spec of the component.
	set func(rf *redisfailoverv1.RedisFailover, p probes)
	// generate returns the first container that the operator writes.
	generate func(t *testing.T, rf *redisfailoverv1.RedisFailover) corev1.Container
	// defaults are the probes without custom probes.
	defaults probes
}

func probeComponents() []probeComponent {
	return []probeComponent{
		{
			name: "redis",
			set: func(rf *redisfailoverv1.RedisFailover, p probes) {
				rf.Spec.Redis.Port = 6379
				rf.Spec.Redis.CustomLivenessProbe, rf.Spec.Redis.CustomReadinessProbe, rf.Spec.Redis.CustomStartupProbe = p.liveness, p.readiness, p.startup
			},
			generate: func(t *testing.T, rf *redisfailoverv1.RedisFailover) corev1.Container {
				t.Helper()
				var container corev1.Container
				ms := &mK8SService.Services{}
				ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
				ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
					container = args.Get(1).(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0]
				}).Return(nil)
				client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
				require.NoError(t, client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{}))
				return container
			},
			defaults: probes{
				liveness:  execProbe(30, 5, 6, 15, "sh", "-c", "t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p 6379 --user pinger --pass pingpass --no-auth-warning ping | grep -qE '^(PONG|LOADING)'"),
				readiness: execProbe(30, 5, 0, 0, "/bin/sh", "/redis-readiness/ready.sh"),
			},
		},
		{
			name: "sentinel",
			set: func(rf *redisfailoverv1.RedisFailover, p probes) {
				rf.Spec.Sentinel.CustomLivenessProbe, rf.Spec.Sentinel.CustomReadinessProbe, rf.Spec.Sentinel.CustomStartupProbe = p.liveness, p.readiness, p.startup
			},
			generate: func(t *testing.T, rf *redisfailoverv1.RedisFailover) corev1.Container {
				t.Helper()
				var container corev1.Container
				ms := &mK8SService.Services{}
				ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
				ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
				ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
					container = args.Get(1).(*appsv1.Deployment).Spec.Template.Spec.Containers[0]
				}).Return(nil)
				client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
				require.NoError(t, client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{}))
				return container
			},
			defaults: probes{
				liveness:  execProbe(30, 5, 0, 0, "sh", "-c", "t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p 26379 ping"),
				readiness: execProbe(30, 5, 0, 0, "sh", "-c", "t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name mymaster | head -n 1 | grep -vq '127.0.0.1' && $t redis-cli -h $(hostname) -p 26379 sentinel ckquorum mymaster | grep -q '^OK'"),
			},
		},
	}
}

// A custom probe replaces the default probe of its own kind, and no other.
func TestCustomProbes(t *testing.T) {
	custom := probes{
		liveness:  execProbe(30, 10, 10, 25, "custom-liveness"),
		readiness: execProbe(31, 11, 11, 26, "custom-readiness"),
		startup:   execProbe(32, 12, 12, 27, "custom-startup"),
	}
	for _, component := range probeComponents() {
		for name, want := range map[string]probes{"custom probes": custom, "default probes": component.defaults} {
			t.Run(component.name+" with "+name, func(t *testing.T) {
				rf := generateRF()
				given := probes{}
				if name == "custom probes" {
					given = custom
				}
				component.set(rf, given)
				container := component.generate(t, rf)

				assert.Equal(t, want.liveness, container.LivenessProbe)
				assert.Equal(t, want.readiness, container.ReadinessProbe)
				assert.Equal(t, want.startup, container.StartupProbe)
			})
		}
	}
}
