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
	CreateOrUpdateDeployment(namespace string, deployment *appsv1.Deployment) error
	DeleteDeployment(namespace string, name string) error
}

// DeploymentService is the deployment service implementation using API calls to kubernetes.
type DeploymentService struct {
	kubeClient kubernetes.Interface
	kindClient[*appsv1.Deployment]
}

func NewDeploymentService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *DeploymentService {
	foreground := metav1.DeletePropagationForeground
	return &DeploymentService{
		kubeClient: kubeClient,
		kindClient: kindClient[*appsv1.Deployment]{
			kind: "Deployment", noun: "deployment", field: "deployment",
			deleteOpts: metav1.DeleteOptions{PropagationPolicy: &foreground},
			api: func(namespace string) namespacedAPI[*appsv1.Deployment] {
				return kubeClient.AppsV1().Deployments(namespace)
			},
			upToDate: deploymentUpToDate,
			// The API server increments the generation also when the annotations
			// change. An update without the stored annotations removes
			// deployment.kubernetes.io/revision, and a further generation restarts
			// the wait of the master replacement for the Sentinel rollout.
			keep: func(stored, desired *appsv1.Deployment) {
				if len(stored.Annotations) != 0 {
					desired.Annotations = util.MergeAnnotations(stored.Annotations, desired.Annotations)
				}
			},
			logger:  logger.With("service", "k8s.deployment"),
			metrics: metricsRecorder,
		},
	}
}

func (d *DeploymentService) GetDeployment(namespace, name string) (*appsv1.Deployment, error) {
	return d.get(namespace, name)
}

// GetDeploymentPods will retrieve the pods managed by a given deployment.
// Other pods can carry the selector labels, so a pod needs the ReplicaSet of
// the deployment as controller. The operator cannot read ReplicaSets, so the
// check compares the name of the ReplicaSet (<name>-<pod-template-hash>) and
// not its UID.
func (d *DeploymentService) GetDeploymentPods(namespace, name string) (*corev1.PodList, error) {
	deployment, err := d.kubeClient.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "Deployment", name, "GET", err, d.metrics)
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
	setPodIPs(metrics.KIND_SENTINEL, pods.Items, d.metrics)
	return pods, nil
}

// CreateOrUpdateDeployment creates the Deployment, or updates the stored
// Deployment when it is not up to date.
func (d *DeploymentService) CreateOrUpdateDeployment(namespace string, deployment *appsv1.Deployment) error {
	return d.createOrUpdate(namespace, deployment)
}

// DeleteDeployment deletes the Deployment and waits for its pods in the
// foreground.
func (d *DeploymentService) DeleteDeployment(namespace, name string) error {
	return d.delete(namespace, name)
}
