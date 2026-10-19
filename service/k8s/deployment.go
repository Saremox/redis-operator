package k8s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

// Deployment the Deployment service that knows how to interact with k8s to manage them.
type Deployment interface {
	GetDeployment(namespace, name string) (*appsv1.Deployment, error)
	GetDeploymentPods(namespace, name string) (*corev1.PodList, error)
	CreateDeployment(namespace string, deployment *appsv1.Deployment) error
	UpdateDeployment(namespace string, deployment *appsv1.Deployment) error
	CreateOrUpdateDeployment(namespace string, deployment *appsv1.Deployment) error
	DeleteDeployment(namespace string, name string) error
	ListDeployments(namespace string) (*appsv1.DeploymentList, error)
}

// DeploymentService is the deployment service implementation using API calls to kubernetes.
type DeploymentService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewDeploymentService returns a new Deployment KubeService.
func NewDeploymentService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *DeploymentService {
	logger = logger.With("service", "k8s.deployment")
	return &DeploymentService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetDeployment will retrieve the requested deployment based on namespace and name.
func (d *DeploymentService) GetDeployment(namespace, name string) (*appsv1.Deployment, error) {
	deployment, err := d.kubeClient.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Deployment", name, "GET", err, d.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	return deployment, nil
}

// GetDeploymentPods will retrieve the pods managed by a given deployment.
// Other pods can carry the selector labels, so a pod needs the ReplicaSet of
// the deployment as controller. The operator cannot read ReplicaSets, so the
// check compares the name of the ReplicaSet (<name>-<pod-template-hash>) and
// not its UID.
func (d *DeploymentService) GetDeploymentPods(namespace, name string) (*corev1.PodList, error) {
	deployment, err := d.kubeClient.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Deployment", name, "GET", err, d.metricsRecorder)
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	selector := labels.Set(deployment.Spec.Selector.MatchLabels).String()
	pods, err := d.kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list the pods of deployment %s/%s: %w", namespace, name, err)
	}
	owned := pods.Items[:0]
	for _, pod := range pods.Items {
		hash := pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
		controller := metav1.GetControllerOf(&pod)
		if hash != "" && controller != nil && controller.APIVersion == appsv1.SchemeGroupVersion.String() && controller.Kind == "ReplicaSet" && controller.Name == deployment.Name+"-"+hash {
			owned = append(owned, pod)
		}
	}
	pods.Items = owned
	// The sentinel pods are the only Deployment pods that the operator lists.
	setPodIPs(metrics.KIND_SENTINEL, pods.Items, d.metricsRecorder)
	return pods, nil
}

// CreateDeployment will create the given deployment.
func (d *DeploymentService) CreateDeployment(namespace string, deployment *appsv1.Deployment) error {
	_, err := d.kubeClient.AppsV1().Deployments(namespace).Create(context.TODO(), deployment, metav1.CreateOptions{})
	recordMetrics(namespace, "Deployment", deployment.GetName(), "CREATE", err, d.metricsRecorder)
	return writeResult(err, d.logger, namespace, "deployment", deployment.Name, "create deployment", "deployment created")
}

// UpdateDeployment will update the given deployment.
func (d *DeploymentService) UpdateDeployment(namespace string, deployment *appsv1.Deployment) error {
	_, err := d.kubeClient.AppsV1().Deployments(namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
	recordMetrics(namespace, "Deployment", deployment.GetName(), "UPDATE", err, d.metricsRecorder)
	return writeResult(err, d.logger, namespace, "deployment", deployment.Name, "update deployment", "deployment updated")
}

// CreateOrUpdateDeployment will update the given deployment or create it if does not exist.
func (d *DeploymentService) CreateOrUpdateDeployment(namespace string, deployment *appsv1.Deployment) error {
	return createOrUpdate(namespace, deployment, objectCalls[*appsv1.Deployment]{
		kind: "Deployment", get: d.GetDeployment, create: d.CreateDeployment, update: d.UpdateDeployment, upToDate: deploymentUpToDate,
		// The API server increments the generation also when the annotations
		// change. An update without the stored annotations removes
		// deployment.kubernetes.io/revision, and a further generation restarts
		// the wait of the master replacement for the Sentinel rollout.
		keep: func(stored, desired *appsv1.Deployment) {
			if len(stored.Annotations) != 0 {
				desired.Annotations = util.MergeAnnotations(stored.Annotations, desired.Annotations)
			}
		},
		skipped: func() {
			d.logger.WithField("namespace", namespace).WithField("deployment", deployment.Name).Debugf("deployment already up to date, skipping update")
		},
	})
}

// DeleteDeployment will delete the given deployment.
func (d *DeploymentService) DeleteDeployment(namespace, name string) error {
	propagation := metav1.DeletePropagationForeground
	err := d.kubeClient.AppsV1().Deployments(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	recordMetrics(namespace, "Deployment", name, "DELETE", err, d.metricsRecorder)
	if err != nil {
		return fmt.Errorf("delete deployment %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ListDeployments will give all the deployments on a given namespace.
func (d *DeploymentService) ListDeployments(namespace string) (*appsv1.DeploymentList, error) {
	deployments, err := d.kubeClient.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "Deployment", metrics.NOT_APPLICABLE, "LIST", err, d.metricsRecorder)
	if err != nil {
		return deployments, fmt.Errorf("list deployments in %s: %w", namespace, err)
	}
	return deployments, nil
}
