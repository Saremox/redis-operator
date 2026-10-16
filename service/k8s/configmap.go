package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	ListConfigMaps(namespace string) (*corev1.ConfigMapList, error)
}

// ConfigMapService is the configMap service implementation using API calls to kubernetes.
type ConfigMapService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewConfigMapService returns a new ConfigMap KubeService.
func NewConfigMapService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *ConfigMapService {
	logger = logger.With("service", "k8s.configMap")
	return &ConfigMapService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetConfigMap returns the named ConfigMap.
func (p *ConfigMapService) GetConfigMap(namespace, name string) (*corev1.ConfigMap, error) {
	configMap, err := p.kubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	// The name can come from the RedisFailover spec, so it is not a metric label.
	recordMetrics(namespace, "ConfigMap", metrics.NOT_APPLICABLE, "GET", err, p.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}
	return configMap, nil
}

// CreateConfigMap creates the given ConfigMap.
func (p *ConfigMapService) CreateConfigMap(namespace string, configMap *corev1.ConfigMap) error {
	_, err := p.kubeClient.CoreV1().ConfigMaps(namespace).Create(context.TODO(), configMap, metav1.CreateOptions{})
	recordMetrics(namespace, "ConfigMap", configMap.GetName(), "CREATE", err, p.metricsRecorder)
	if err != nil {
		return fmt.Errorf("create configmap %s/%s: %w", namespace, configMap.Name, err)
	}
	p.logger.WithField("namespace", namespace).WithField("configMap", configMap.Name).Debugf("configMap created")
	return nil
}

// UpdateConfigMap replaces the stored ConfigMap with the given ConfigMap.
func (p *ConfigMapService) UpdateConfigMap(namespace string, configMap *corev1.ConfigMap) error {
	_, err := p.kubeClient.CoreV1().ConfigMaps(namespace).Update(context.TODO(), configMap, metav1.UpdateOptions{})
	recordMetrics(namespace, "ConfigMap", configMap.GetName(), "UPDATE", err, p.metricsRecorder)
	if err != nil {
		return fmt.Errorf("update configmap %s/%s: %w", namespace, configMap.Name, err)
	}
	p.logger.WithField("namespace", namespace).WithField("configMap", configMap.Name).Debugf("configMap updated")
	return nil
}

// CreateOrUpdateConfigMap creates the ConfigMap, or updates the stored
// ConfigMap when it is not up to date. It returns a ControlledByOtherError
// when another controller controls the stored ConfigMap.
func (p *ConfigMapService) CreateOrUpdateConfigMap(namespace string, configMap *corev1.ConfigMap) error {
	storedConfigMap, err := p.GetConfigMap(namespace, configMap.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return p.CreateConfigMap(namespace, configMap)
		}
		return err
	}

	if err := checkNotControlledByOther("ConfigMap", storedConfigMap, configMap); err != nil {
		return err
	}

	// With the stored resource version, the update fails when the object
	// changed after the read.
	configMap.ResourceVersion = storedConfigMap.ResourceVersion

	if configMapUpToDate(storedConfigMap, configMap) {
		p.logger.WithField("namespace", namespace).WithField("configMap", configMap.Name).Debugf("configmap already up to date, skipping update")
		return nil
	}

	return p.UpdateConfigMap(namespace, configMap)
}

// DeleteConfigMap deletes the named ConfigMap.
func (p *ConfigMapService) DeleteConfigMap(namespace, name string) error {
	err := p.kubeClient.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	recordMetrics(namespace, "ConfigMap", name, "DELETE", err, p.metricsRecorder)
	if err != nil {
		return fmt.Errorf("delete configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ListConfigMaps returns all the ConfigMaps of the namespace.
func (p *ConfigMapService) ListConfigMaps(namespace string) (*corev1.ConfigMapList, error) {
	objects, err := p.kubeClient.CoreV1().ConfigMaps(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "ConfigMap", metrics.NOT_APPLICABLE, "LIST", err, p.metricsRecorder)
	if err != nil {
		return objects, fmt.Errorf("list configmaps in %s: %w", namespace, err)
	}
	return objects, nil
}
