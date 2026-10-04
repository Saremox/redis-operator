package k8s

import (
	"context"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/saremox/redis-operator/operator/redisfailover/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// StatefulSet the StatefulSet service that knows how to interact with k8s to manage them
type StatefulSet interface {
	GetStatefulSet(namespace, name string) (*appsv1.StatefulSet, error)
	GetStatefulSetPods(namespace, name string) (*corev1.PodList, error)
	CreateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error
	UpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error
	CreateOrUpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error
	DeleteStatefulSet(namespace string, name string) error
	ListStatefulSets(namespace string) (*appsv1.StatefulSetList, error)
	GetControllerRevision(namespace, name string) (*appsv1.ControllerRevision, error)
}

// StatefulSetService is the statefulSet service implementation using API calls to kubernetes.
type StatefulSetService struct {
	kubeClient      kubernetes.Interface
	logger          log.Logger
	metricsRecorder metrics.Recorder
}

// NewStatefulSetService returns a new StatefulSet KubeService.
func NewStatefulSetService(kubeClient kubernetes.Interface, logger log.Logger, metricsRecorder metrics.Recorder) *StatefulSetService {
	logger = logger.With("service", "k8s.statefulSet")
	return &StatefulSetService{
		kubeClient:      kubeClient,
		logger:          logger,
		metricsRecorder: metricsRecorder,
	}
}

// GetStatefulSet will retrieve the requested statefulset based on namespace and name
func (s *StatefulSetService) GetStatefulSet(namespace, name string) (*appsv1.StatefulSet, error) {
	statefulSet, err := s.kubeClient.AppsV1().StatefulSets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "StatefulSet", name, "GET", err, s.metricsRecorder)
	if err != nil {
		return nil, err
	}
	return statefulSet, err
}

// GetStatefulSetPods will give a list of pods that are managed by the statefulset.
// Other pods can carry the selector labels, so a pod needs the StatefulSet as
// controller and a name of the form <name>-<ordinal>.
func (s *StatefulSetService) GetStatefulSetPods(namespace, name string) (*corev1.PodList, error) {
	statefulSet, err := s.GetStatefulSet(namespace, name)
	if err != nil {
		return nil, err
	}
	selector := labels.Set(statefulSet.Spec.Selector.MatchLabels).String()
	pods, err := s.kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	owned := pods.Items[:0]
	for _, pod := range pods.Items {
		if metav1.IsControlledBy(&pod, statefulSet) && hasOrdinalName(pod.Name, statefulSet.Name) {
			owned = append(owned, pod)
		}
	}
	pods.Items = owned
	return pods, nil
}

// hasOrdinalName reports whether name is <prefix>-<ordinal>, the form that the
// StatefulSet controller uses for pods and for PVCs. The ordinal must be the
// canonical number of an int32: the controller ignores a name that has another
// form, such as a leading zero or a number that is too large.
func hasOrdinalName(name, prefix string) bool {
	ordinal, ok := strings.CutPrefix(name, prefix+"-")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(ordinal, 10, 32)
	return err == nil && n >= 0 && strconv.FormatInt(n, 10) == ordinal
}

// CreateStatefulSet will create the given statefulset
func (s *StatefulSetService) CreateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	_, err := s.kubeClient.AppsV1().StatefulSets(namespace).Create(context.TODO(), statefulSet, metav1.CreateOptions{})
	recordMetrics(namespace, "StatefulSet", statefulSet.GetName(), "CREATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("statefulSet", statefulSet.ObjectMeta.Name).Debugf("statefulSet created")
	return err
}

// UpdateStatefulSet will update the given statefulset
func (s *StatefulSetService) UpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	_, err := s.kubeClient.AppsV1().StatefulSets(namespace).Update(context.TODO(), statefulSet, metav1.UpdateOptions{})
	recordMetrics(namespace, "StatefulSet", statefulSet.GetName(), "UPDATE", err, s.metricsRecorder)
	if err != nil {
		return err
	}
	s.logger.WithField("namespace", namespace).WithField("statefulSet", statefulSet.ObjectMeta.Name).Debugf("statefulSet updated")
	return err
}

// CreateOrUpdateStatefulSet will update the statefulset or create it if does not exist
func (s *StatefulSetService) CreateOrUpdateStatefulSet(namespace string, statefulSet *appsv1.StatefulSet) error {
	storedStatefulSet, err := s.GetStatefulSet(namespace, statefulSet.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return s.CreateStatefulSet(namespace, statefulSet)
		}
		return err
	}

	// With the stored resource version, the update fails when the object
	// changed after the read.
	statefulSet.ResourceVersion = storedStatefulSet.ResourceVersion
	if len(statefulSet.Spec.VolumeClaimTemplates) != 0 {
		if err := s.updateStatefulSetPVCs(namespace, storedStatefulSet, statefulSet); err != nil {
			return err
		}
	}
	// The volume claim templates of a StatefulSet cannot be changed, so the
	// update keeps the stored ones.
	statefulSet.Spec.VolumeClaimTemplates = storedStatefulSet.Spec.VolumeClaimTemplates
	statefulSet.Annotations = util.MergeAnnotations(storedStatefulSet.Annotations, statefulSet.Annotations)

	if statefulSetUpToDate(storedStatefulSet, statefulSet) {
		s.logger.WithField("namespace", namespace).WithField("statefulSet", statefulSet.Name).Debugf("statefulset already up to date, skipping update")
		return nil
	}

	return s.UpdateStatefulSet(namespace, statefulSet)
}

// updateStatefulSetPVCs changes the existing PVCs, because the volume claim
// template of a StatefulSet cannot change. It does two changes only. It
// removes the owners that the template does not reference, so that the
// garbage collector does not delete the PVCs. It increases a storage request
// that is smaller than the template request.
func (s *StatefulSetService) updateStatefulSetPVCs(namespace string, storedStatefulSet, statefulSet *appsv1.StatefulSet) error {
	template := statefulSet.Spec.VolumeClaimTemplates[0]
	desiredStorage := template.Spec.Resources.Requests.Storage()
	removedOwners := map[types.UID]bool{}
	for _, owner := range statefulSet.OwnerReferences {
		if !hasOwnerReference(template.OwnerReferences, owner.UID) {
			removedOwners[owner.UID] = true
		}
	}

	// The StatefulSet controller adds the selector labels of the StatefulSet to each PVC.
	selector := labels.Set(storedStatefulSet.Spec.Selector.MatchLabels).String()
	pvcs, err := s.kubeClient.CoreV1().PersistentVolumeClaims(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !isStatefulSetPVC(pvc.Name, storedStatefulSet) {
			continue
		}
		// The owner change is its own update and its error stops the
		// reconcile: a rejected resize must not keep an owner reference that
		// lets the garbage collector delete the PVC.
		if len(withoutOwners(pvc.OwnerReferences, removedOwners)) != len(pvc.OwnerReferences) {
			// Other controllers also write the PVC, so a conflict is retried on
			// the current PVC instead of a failed reconcile.
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current, err := s.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				current.OwnerReferences = withoutOwners(current.OwnerReferences, removedOwners)
				updated, err := s.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Update(context.TODO(), current, metav1.UpdateOptions{})
				if err != nil {
					return err
				}
				pvc = updated
				return nil
			})
			if err != nil {
				return err
			}
		}
		// A storage class can refuse the expansion, so a failed resize is
		// only logged and does not block the reconcile.
		if pvc.Spec.Resources.Requests.Storage().Cmp(*desiredStorage) < 0 {
			pvc.Spec.Resources.Requests = template.Spec.Resources.Requests
			if _, err := s.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Update(context.TODO(), pvc, metav1.UpdateOptions{}); err != nil {
				s.logger.WithField("namespace", namespace).WithField("pvc", pvc.Name).Warningf("resize pvc failed: %s", err.Error())
				continue
			}
			s.logger.WithField("namespace", namespace).WithField("pvc", pvc.Name).Infof("pvc resized")
		}
	}
	return nil
}

// isStatefulSetPVC checks the name, because other PVCs can carry the selector labels.
func isStatefulSetPVC(name string, statefulSet *appsv1.StatefulSet) bool {
	for _, template := range statefulSet.Spec.VolumeClaimTemplates {
		if hasOrdinalName(name, template.Name+"-"+statefulSet.Name) {
			return true
		}
	}
	return false
}

func withoutOwners(ownerReferences []metav1.OwnerReference, removed map[types.UID]bool) []metav1.OwnerReference {
	kept := []metav1.OwnerReference{}
	for _, owner := range ownerReferences {
		if !removed[owner.UID] {
			kept = append(kept, owner)
		}
	}
	return kept
}

func hasOwnerReference(ownerReferences []metav1.OwnerReference, uid types.UID) bool {
	for _, owner := range ownerReferences {
		if owner.UID == uid {
			return true
		}
	}
	return false
}

// DeleteStatefulSet will delete the statefulset
func (s *StatefulSetService) DeleteStatefulSet(namespace, name string) error {
	propagation := metav1.DeletePropagationForeground
	err := s.kubeClient.AppsV1().StatefulSets(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	recordMetrics(namespace, "StatefulSet", name, "DELETE", err, s.metricsRecorder)
	return err
}

// ListStatefulSets will retrieve a list of statefulset in the given namespace
func (s *StatefulSetService) ListStatefulSets(namespace string) (*appsv1.StatefulSetList, error) {
	stsList, err := s.kubeClient.AppsV1().StatefulSets(namespace).List(context.TODO(), metav1.ListOptions{})
	recordMetrics(namespace, "StatefulSet", metrics.NOT_APPLICABLE, "LIST", err, s.metricsRecorder)
	return stsList, err
}

// GetControllerRevision returns the named ControllerRevision.
func (s *StatefulSetService) GetControllerRevision(namespace, name string) (*appsv1.ControllerRevision, error) {
	revision, err := s.kubeClient.AppsV1().ControllerRevisions(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	recordMetrics(namespace, "ControllerRevision", name, "GET", err, s.metricsRecorder)
	return revision, err
}
