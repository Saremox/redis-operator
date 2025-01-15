package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// ServiceAccount the ServiceAccount service that knows how to interact with k8s to manage them
type ServiceAccount interface {
	GetServiceAccount(namespace string, name string) (*corev1.ServiceAccount, error)
	CreateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error
	UpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error
	CreateOrUpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error
	DeleteServiceAccount(namespace string, name string) error
	ListServiceAccounts(namespace string) (*corev1.ServiceAccountList, error)
}

// ServiceAccountService is the serviceAccount service implementation using API calls to kubernetes.
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

func (s *ServiceAccountService) GetServiceAccount(namespace string, name string) (*corev1.ServiceAccount, error) {
	serviceAccount, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "ServiceAccount", name, "GET", err, s.metricsRecorder)
	if err != nil {
		return nil, err
	}
	return serviceAccount, err
}

func (s *ServiceAccountService) CreateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error {
	_, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Create(context.TODO(), serviceAccount, metav1.CreateOptions{})
	recordMetrics(namespace, "ServiceAccount", serviceAccount.GetName(), "CREATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("serviceAccountName", serviceAccount.Name).Debugf("serviceAccount created")
	return nil
}

func (s *ServiceAccountService) UpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error {
	_, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Update(context.TODO(), serviceAccount, metav1.UpdateOptions{})
	recordMetrics(namespace, "ServiceAccount", serviceAccount.GetName(), "UPDATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("serviceAccountName", serviceAccount.Name).Debugf("serviceAccount updated")
	return nil
}

func (s *ServiceAccountService) CreateOrUpdateServiceAccount(namespace string, serviceAccount *corev1.ServiceAccount) error {
	storedServiceAccount, err := s.GetServiceAccount(namespace, serviceAccount.Name)
	if err != nil {
		// If no resource we need to create.
		if errors.IsNotFound(err) {
			return s.CreateServiceAccount(namespace, serviceAccount)
		}
		return err
	}

	// Already exists, need to Update.
	// Set the correct resource version to ensure we are on the latest version. This way the only valid
	// namespace is our spec(https://github.com/kubernetes/community/blob/master/contributors/devel/api-conventions.md#concurrency-control-and-consistency),
	// we will replace the current namespace state.
	serviceAccount.ResourceVersion = storedServiceAccount.ResourceVersion
	return s.UpdateServiceAccount(namespace, serviceAccount)
}

func (s *ServiceAccountService) DeleteServiceAccount(namespace string, name string) error {
	err := s.kubeClient.CoreV1().ServiceAccounts(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	recordMetrics(namespace, "ServiceAccount", name, "DELETE", err, s.metricsRecorder)
	return err
}

func (s *ServiceAccountService) ListServiceAccounts(namespace string) (*corev1.ServiceAccountList, error) {
	serviceAccountList, err := s.kubeClient.CoreV1().ServiceAccounts(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "ServiceAccount", metrics.NOT_APPLICABLE, "LIST", err, s.metricsRecorder)
	return serviceAccountList, err
}
