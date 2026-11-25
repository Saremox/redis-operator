package k8s

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// ConfigMap the ConfigMap service that knows how to interact with k8s to manage them.
type ConfigMap interface {
	GetConfigMap(namespace string, name string) (*corev1.ConfigMap, error)
	CreateConfigMap(namespace string, configMap *corev1.ConfigMap) error
	UpdateConfigMap(namespace string, configMap *corev1.ConfigMap) error
	CreateOrUpdateConfigMap(namespace string, np *corev1.ConfigMap) error
	DeleteConfigMap(namespace string, name string) error
}

// ConfigMapService is the configmap service implementation using API calls to kubernetes.
type ConfigMapService struct {
	kindClient[*corev1.ConfigMap]
}

func NewConfigMapService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *ConfigMapService {
	logger = logger.With("service", "k8s.configMap")
	return &ConfigMapService{
		kindClient: kindClient[*corev1.ConfigMap]{
			kind: "ConfigMap", noun: "configmap", field: "configMap", nameFromSpec: true,
			api: func(namespace string) namespacedAPI[*corev1.ConfigMap] {
				return kubeClient.CoreV1().ConfigMaps(namespace)
			},
			upToDate: configMapUpToDate,
			logger:   logger, metrics: metricsRecorder,
		},
	}
}

func (s *ConfigMapService) GetConfigMap(namespace, name string) (*corev1.ConfigMap, error) {
	return s.get(namespace, name)
}

func (s *ConfigMapService) CreateConfigMap(namespace string, object *corev1.ConfigMap) error {
	return s.create(namespace, object)
}

func (s *ConfigMapService) UpdateConfigMap(namespace string, object *corev1.ConfigMap) error {
	return s.update(namespace, object)
}

// CreateOrUpdateConfigMap creates the ConfigMap, or updates the stored ConfigMap when
// it is not up to date. It returns a ControlledByOtherError when another
// controller controls the stored ConfigMap.
func (s *ConfigMapService) CreateOrUpdateConfigMap(namespace string, object *corev1.ConfigMap) error {
	return s.createOrUpdate(namespace, object)
}

func (s *ConfigMapService) DeleteConfigMap(namespace, name string) error {
	return s.delete(namespace, name)
}
