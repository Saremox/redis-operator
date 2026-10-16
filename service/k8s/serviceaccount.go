package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// ServiceAccount the ServiceAccount service that knows how to interact with k8s to manage them.
type ServiceAccount interface {
	GetServiceAccount(namespace string, name string) (*corev1.ServiceAccount, error)
	CreateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error
	UpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error
	CreateOrUpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error
	DeleteServiceAccount(namespace string, name string) error
	ListServiceAccounts(namespace string) (*corev1.ServiceAccountList, error)
}

// ServiceAccountService is the service account service implementation using API calls to kubernetes.
type ServiceAccountService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewServiceAccountService returns a new ServiceAccount KubeService.
func NewServiceAccountService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *ServiceAccountService {
	logger = logger.With("service", "k8s.serviceAccount")
	return &ServiceAccountService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetServiceAccount returns the named ServiceAccount.
func (s *ServiceAccountService) GetServiceAccount(namespace, name string) (*corev1.ServiceAccount, error) {
	serviceAccount, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "ServiceAccount", name, "GET", err, s.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("get serviceaccount %s/%s: %w", namespace, name, err)
	}
	return serviceAccount, nil
}

// CreateServiceAccount creates the given ServiceAccount.
func (s *ServiceAccountService) CreateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error {
	_, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Create(context.TODO(), serviceAccount, metav1.CreateOptions{})
	recordMetrics(namespace, "ServiceAccount", serviceAccount.GetName(), "CREATE", err, s.metricsRecorder)
	return writeResult(err, s.logger, namespace, "serviceAccount", serviceAccount.Name, "create serviceaccount", "serviceAccount created")
}

// UpdateServiceAccount replaces the stored ServiceAccount with the given
// ServiceAccount.
func (s *ServiceAccountService) UpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error {
	_, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Update(context.TODO(), serviceAccount, metav1.UpdateOptions{})
	recordMetrics(namespace, "ServiceAccount", serviceAccount.GetName(), "UPDATE", err, s.metricsRecorder)
	return writeResult(err, s.logger, namespace, "serviceAccount", serviceAccount.Name, "update serviceaccount", "serviceAccount updated")
}

// CreateOrUpdateServiceAccount creates the ServiceAccount, or updates the
// stored ServiceAccount when it is not up to date. It returns a
// ControlledByOtherError when another controller controls the stored
// ServiceAccount.
func (s *ServiceAccountService) CreateOrUpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error {
	return createOrUpdate(namespace, serviceAccount, objectCalls[*corev1.ServiceAccount]{
		kind: "ServiceAccount", get: s.GetServiceAccount, create: s.CreateServiceAccount, update: s.UpdateServiceAccount, upToDate: serviceAccountUpToDate,
		skipped: func() {
			s.logger.WithField("namespace", namespace).WithField("serviceAccount", serviceAccount.Name).Debugf("service account already up to date, skipping update")
		},
	})
}

// DeleteServiceAccount deletes the named ServiceAccount.
func (s *ServiceAccountService) DeleteServiceAccount(namespace, name string) error {
	err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	recordMetrics(namespace, "ServiceAccount", name, "DELETE", err, s.metricsRecorder)
	if err != nil {
		return fmt.Errorf("delete serviceaccount %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ListServiceAccounts returns all the ServiceAccounts of the namespace.
func (s *ServiceAccountService) ListServiceAccounts(namespace string) (*corev1.ServiceAccountList, error) {
	objects, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "ServiceAccount", metrics.NOT_APPLICABLE, "LIST", err, s.metricsRecorder)
	if err != nil {
		return objects, fmt.Errorf("list serviceaccounts in %s: %w", namespace, err)
	}
	return objects, nil
}
