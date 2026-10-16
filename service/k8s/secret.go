package k8s

import (
	"context"
	"fmt"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Secret interacts with k8s to get secrets.
type Secret interface {
	GetSecret(namespace, name string) (*corev1.Secret, error)
	CreateOrUpdateSecret(namespace string, secret *corev1.Secret) error
}

// SecretService is the secret service implementation using API calls to kubernetes.
type SecretService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewSecretService returns a new Secret KubeService.
func NewSecretService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *SecretService {

	logger = logger.With("service", "k8s.secret")
	return &SecretService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetSecret returns the named Secret.
func (s *SecretService) GetSecret(namespace, name string) (*corev1.Secret, error) {

	secret, err := s.kubeClient.CoreV1().Secrets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	// The Secret name comes from the RedisFailover spec, so it is not a metric label.
	recordMetrics(namespace, "Secret", metrics.NOT_APPLICABLE, "GET", err, s.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("get secret %s/%s: %w", namespace, name, err)
	}

	return secret, nil
}

// CreateOrUpdateSecret creates the Secret, or updates a stored Secret with the
// same controller. It returns a ControlledByOtherError for any other Secret.
func (s *SecretService) CreateOrUpdateSecret(namespace string, secret *corev1.Secret) error {
	stored, err := s.GetSecret(namespace, secret.Name)
	if errors.IsNotFound(err) {
		_, err := s.kubeClient.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
		recordMetrics(namespace, "Secret", secret.Name, "CREATE", err, s.metricsRecorder)
		if err != nil {
			return fmt.Errorf("create secret %s/%s: %w", namespace, secret.Name, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := CheckSameController("Secret", stored, secret); err != nil {
		return err
	}
	secret.ResourceVersion = stored.ResourceVersion
	_, err = s.kubeClient.CoreV1().Secrets(namespace).Update(context.TODO(), secret, metav1.UpdateOptions{})
	recordMetrics(namespace, "Secret", secret.Name, "UPDATE", err, s.metricsRecorder)
	if err != nil {
		return fmt.Errorf("update secret %s/%s: %w", namespace, secret.Name, err)
	}
	return nil
}
