//nolint:dupl // Each kind has its own interface, and the Services interface and the mocks need these names.
package k8s

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// ServiceAccount the ServiceAccount service that knows how to interact with k8s to manage them.
type ServiceAccount interface {
	GetServiceAccount(namespace string, name string) (*corev1.ServiceAccount, error)
	CreateOrUpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error
	DeleteServiceAccount(namespace string, name string) error
}

// ServiceAccountService is the serviceaccount service implementation using API calls to kubernetes.
type ServiceAccountService struct {
	kindClient[*corev1.ServiceAccount]
}

// NewServiceAccountService returns a new ServiceAccount KubeService.
func NewServiceAccountService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *ServiceAccountService {
	logger = logger.With("service", "k8s.serviceAccount")
	return &ServiceAccountService{
		kindClient: kindClient[*corev1.ServiceAccount]{
			kind: "ServiceAccount", noun: "serviceaccount", field: "serviceAccount",
			api: func(namespace string) namespacedAPI[*corev1.ServiceAccount] {
				return kubeClient.CoreV1().ServiceAccounts(namespace)
			},
			upToDate: serviceAccountUpToDate,
			logger:   logger, metrics: metricsRecorder,
		},
	}
}

// GetServiceAccount returns the named ServiceAccount.
func (s *ServiceAccountService) GetServiceAccount(namespace, name string) (*corev1.ServiceAccount, error) {
	return s.get(namespace, name)
}

// CreateOrUpdateServiceAccount creates the ServiceAccount, or updates the stored ServiceAccount when
// it is not up to date. It returns a ControlledByOtherError when another
// controller controls the stored ServiceAccount.
func (s *ServiceAccountService) CreateOrUpdateServiceAccount(namespace string, object *corev1.ServiceAccount) error {
	return s.createOrUpdate(namespace, object)
}

// DeleteServiceAccount deletes the named ServiceAccount.
func (s *ServiceAccountService) DeleteServiceAccount(namespace, name string) error {
	return s.delete(namespace, name)
}
