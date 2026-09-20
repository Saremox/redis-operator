package k8s

import (
	"k8s.io/client-go/kubernetes"

	redisfailoverclientset "github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// Service is the K8s service entrypoint.
type Services interface {
	ConfigMap
	Secret
	Pod
	PodDisruptionBudget
	RedisFailover
	Service
	Deployment
	StatefulSet
	ServiceAccount
}

type services struct {
	ConfigMap
	Secret
	Pod
	PodDisruptionBudget
	RedisFailover
	Service
	Deployment
	StatefulSet
	ServiceAccount
}

// New returns a new Kubernetes service.
func New(kubecli kubernetes.Interface, crdcli redisfailoverclientset.Interface, logger log.Logger, metricsRecorder metrics.Recorder) Services {
	return &services{
		ConfigMap:           NewConfigMapService(kubecli, logger, metricsRecorder),
		Secret:              NewSecretService(kubecli, logger, metricsRecorder),
		Pod:                 NewPodService(kubecli, logger, metricsRecorder),
		PodDisruptionBudget: NewPodDisruptionBudgetService(kubecli, logger, metricsRecorder),
		RedisFailover:       NewRedisFailoverService(crdcli, logger, metricsRecorder),
		Service:             NewServiceService(kubecli, logger, metricsRecorder),
		Deployment:          NewDeploymentService(kubecli, logger, metricsRecorder),
		StatefulSet:         NewStatefulSetService(kubecli, logger, metricsRecorder),
		ServiceAccount:      NewServiceAccountService(kubecli, logger, metricsRecorder),
	}
}
