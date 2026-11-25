//nolint:dupl // Each kind has its own interface, and the Services interface and the mocks need these names.
package k8s

import (
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// PodDisruptionBudget the PodDisruptionBudget service that knows how to interact with k8s to manage them.
type PodDisruptionBudget interface {
	GetPodDisruptionBudget(namespace string, name string) (*policyv1.PodDisruptionBudget, error)
	CreateOrUpdatePodDisruptionBudget(namespace string, podDisruptionBudget *policyv1.PodDisruptionBudget) error
	DeletePodDisruptionBudget(namespace string, name string) error
}

// PodDisruptionBudgetService is the poddisruptionbudget service implementation using API calls to kubernetes.
type PodDisruptionBudgetService struct {
	kindClient[*policyv1.PodDisruptionBudget]
}

func NewPodDisruptionBudgetService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *PodDisruptionBudgetService {
	logger = logger.With("service", "k8s.podDisruptionBudget")
	return &PodDisruptionBudgetService{
		kindClient: kindClient[*policyv1.PodDisruptionBudget]{
			kind: "PodDisruptionBudget", noun: "poddisruptionbudget", field: "podDisruptionBudget",
			api: func(namespace string) namespacedAPI[*policyv1.PodDisruptionBudget] {
				return kubeClient.PolicyV1().PodDisruptionBudgets(namespace)
			},
			upToDate: podDisruptionBudgetUpToDate,
			logger:   logger, metrics: metricsRecorder,
		},
	}
}

func (s *PodDisruptionBudgetService) GetPodDisruptionBudget(namespace, name string) (*policyv1.PodDisruptionBudget, error) {
	return s.get(namespace, name)
}

// CreateOrUpdatePodDisruptionBudget creates the PodDisruptionBudget, or updates the stored PodDisruptionBudget when
// it is not up to date. It returns a ControlledByOtherError when another
// controller controls the stored PodDisruptionBudget.
func (s *PodDisruptionBudgetService) CreateOrUpdatePodDisruptionBudget(namespace string, object *policyv1.PodDisruptionBudget) error {
	return s.createOrUpdate(namespace, object)
}

func (s *PodDisruptionBudgetService) DeletePodDisruptionBudget(namespace, name string) error {
	return s.delete(namespace, name)
}
