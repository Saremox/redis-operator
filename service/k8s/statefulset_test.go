package k8s_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	v1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

var (
	statefulSetsGroup = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	pvcsGroup         = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
)

func newStatefulSetUpdateAction(ns string, statefulSet *appsv1.StatefulSet) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(statefulSetsGroup, ns, statefulSet)
}

func newStatefulSetGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(statefulSetsGroup, ns, name)
}

func newStatefulSetCreateAction(ns string, statefulSet *appsv1.StatefulSet) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(statefulSetsGroup, ns, statefulSet)
}

// ownedPod returns a pod that the StatefulSet controller owns.
func ownedPod(statefulSet *appsv1.StatefulSet, name string, labels map[string]string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: statefulSet.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(statefulSet, appsv1.SchemeGroupVersion.WithKind("StatefulSet")),
			},
		},
	}
}

func podNames(podList *v1.PodList) []string {
	names := []string{}
	for _, pod := range podList.Items {
		names = append(names, pod.Name)
	}
	return names
}

// TestStatefulSetServiceGetStatefulSetPods uses a fake clientset to get real
// label selector and namespace semantics. The pods must come from the
// StatefulSet: the labels are not enough.
func TestStatefulSetServiceGetStatefulSetPods(t *testing.T) {
	testns := "testns"
	otherns := "otherns"
	stsName := "teststatefulset"
	selectorLabels := map[string]string{"app": "redis", "component": "master"}

	testStatefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: testns, UID: "sts-uid"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels},
		},
	}
	otherStatefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testns, UID: "other-uid"},
	}

	// A scale-down keeps the pods with a high ordinal until the controller deletes them.
	podMaster := ownedPod(testStatefulSet, stsName+"-0", selectorLabels)
	podHighOrdinal := ownedPod(testStatefulSet, stsName+"-12", map[string]string{"app": "redis", "component": "master", "extra": "label"})

	// Same labels, no owner.
	labelsOnly := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: stsName + "-1", Namespace: testns, Labels: selectorLabels}}
	// Same labels, owned by another StatefulSet.
	otherOwner := ownedPod(otherStatefulSet, stsName+"-2", selectorLabels)
	// Same owner, name outside the form <name>-<ordinal>.
	for _, name := range []string{"custom", stsName + "-", stsName + "-x", stsName + "-1x", stsName + "-3-4", "x" + stsName + "-3"} {
		t.Run("drops the owned pod with the name "+name, func(t *testing.T) {
			mcli := kubernetes.NewClientset(testStatefulSet, podMaster, ownedPod(testStatefulSet, name, selectorLabels))
			service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)

			podList, err := service.GetStatefulSetPods(testns, stsName)
			assert.NoError(t, err)
			assert.Equal(t, []string{podMaster.Name}, podNames(podList))
		})
	}
	// Same owner reference, but the pod does not carry the selector labels.
	otherLabels := ownedPod(testStatefulSet, stsName+"-4", map[string]string{"app": "redis", "component": "slave"})
	// Right owner and labels in another namespace.
	otherNamespace := ownedPod(testStatefulSet, stsName+"-5", selectorLabels)
	otherNamespace.Namespace = otherns
	// Same name and kind, other UID: a StatefulSet that the operator replaced.
	staleOwner := ownedPod(testStatefulSet, stsName+"-6", selectorLabels)
	staleOwner.OwnerReferences[0].UID = "old-uid"
	// An owner reference without the controller flag.
	notController := ownedPod(testStatefulSet, stsName+"-7", selectorLabels)
	notController.OwnerReferences[0].Controller = ptr.To(false)

	mcli := kubernetes.NewClientset(
		testStatefulSet,
		otherStatefulSet,
		podMaster,
		podHighOrdinal,
		labelsOnly,
		otherOwner,
		otherLabels,
		otherNamespace,
		staleOwner,
		notController,
	)
	service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)

	t.Run("returns only the pods that the StatefulSet owns", func(t *testing.T) {
		podList, err := service.GetStatefulSetPods(testns, stsName)
		assert.NoError(t, err)
		assert.ElementsMatch(t, []string{podMaster.Name, podHighOrdinal.Name}, podNames(podList))
	})

	t.Run("propagates the error when the StatefulSet itself does not exist", func(t *testing.T) {
		podList, err := service.GetStatefulSetPods(testns, "does-not-exist")
		assert.Error(t, err)
		assert.Nil(t, podList)
	})

	t.Run("propagates the error of the pod list", func(t *testing.T) {
		failing := kubernetes.NewClientset(testStatefulSet)
		failing.PrependReactor("list", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})

		podList, err := k8s.NewStatefulSetService(failing, log.Dummy, metrics.Dummy).GetStatefulSetPods(testns, stsName)
		assert.Error(t, err)
		assert.Nil(t, podList)
	})

	t.Run("an empty selector lists every pod, and the owner check keeps only the owned pods", func(t *testing.T) {
		emptySelectorSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: testns, UID: "empty-uid"},
			Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{}}},
		}
		owned := ownedPod(emptySelectorSts, "empty-0", nil)
		mcli := kubernetes.NewClientset(emptySelectorSts, owned, podMaster, labelsOnly)

		podList, err := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy).GetStatefulSetPods(testns, "empty")
		assert.NoError(t, err)
		assert.Equal(t, []string{"empty-0"}, podNames(podList))
	})
}

func TestStatefulSetServiceGetCreateOrUpdate(t *testing.T) {
	testStatefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "teststatefulSet1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name                 string
		statefulSet          *appsv1.StatefulSet
		getStatefulSetResult *appsv1.StatefulSet
		errorOnGet           error
		errorOnCreation      error
		expActions           []kubetesting.Action
		expErr               bool
	}{
		{
			name:                 "A new statefulSet should create a new statefulSet.",
			statefulSet:          testStatefulSet,
			getStatefulSetResult: nil,
			errorOnGet:           kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:      nil,
			expActions: []kubetesting.Action{
				newStatefulSetGetAction(testns, testStatefulSet.Name),
				newStatefulSetCreateAction(testns, testStatefulSet),
			},
			expErr: false,
		},
		{
			name:                 "A new statefulSet should error when create a new statefulSet fails.",
			statefulSet:          testStatefulSet,
			getStatefulSetResult: nil,
			errorOnGet:           kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:      errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newStatefulSetGetAction(testns, testStatefulSet.Name),
				newStatefulSetCreateAction(testns, testStatefulSet),
			},
			expErr: true,
		},
		{
			// The stored and desired objects must actually differ here: an
			// identical desired object is now a no-op (see
			// TestStatefulSetServiceObjectUpToDate) and would issue no
			// Update action, defeating the point of this test.
			name: "An existent statefulSet should update the statefulSet.",
			statefulSet: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "teststatefulSet1"},
				Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))},
			},
			getStatefulSetResult: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "teststatefulSet1", ResourceVersion: "10"},
				Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
			},
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newStatefulSetGetAction(testns, "teststatefulSet1"),
				newStatefulSetUpdateAction(testns, &appsv1.StatefulSet{
					// util.MergeAnnotations always allocates a map, even
					// merging two nils, so the applied object's Annotations
					// is {} rather than nil.
					ObjectMeta: metav1.ObjectMeta{Name: "teststatefulSet1", ResourceVersion: "10", Annotations: map[string]string{}},
					Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))},
				}),
			},
			expErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "statefulsets", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getStatefulSetResult, test.errorOnGet
			})
			mcli.AddReactor("create", "statefulsets", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateStatefulSet(testns, test.statefulSet)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
				// Check calls to kubernetes.
				assertTest.Equal(test.expActions, mcli.Actions())
			}
		})
	}
	// test resize pvc
	{
		t.Run("test_Resize_Pvc", func(t *testing.T) {
			assertTest := assert.New(t)
			owners := []metav1.OwnerReference{rfOwnerReference}
			otherPVC := statefulSetPVC("data-rfr-other-0", "0.5Gi")
			otherPVC.Labels["app.kubernetes.io/name"] = "other"
			// Same labels, but not a PVC of the volume claim template.
			foreignPVC := statefulSetPVC("data-rfr-test-extra", "0.5Gi")
			wrongTemplatePVC := statefulSetPVC("cache-rfr-test-0", "0.5Gi")
			mcli := kubernetes.NewClientset(
				pvcStatefulSet("0.5Gi", owners),
				statefulSetPVC("data-rfr-test-0", "0.5Gi", rfOwnerReference),
				// resized already
				statefulSetPVC("data-rfr-test-1", "1Gi", rfOwnerReference),
				otherPVC,
				foreignPVC,
				wrongTemplatePVC,
			)
			service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateStatefulSet(testns, pvcStatefulSet("1Gi", owners))
			assertTest.NoError(err)
			assertPVCStorage(t, mcli, "data-rfr-test-0", "1Gi")
			assertPVCStorage(t, mcli, "data-rfr-test-1", "1Gi")
			assertPVCStorage(t, mcli, "data-rfr-other-0", "0.5Gi")
			assertPVCStorage(t, mcli, "data-rfr-test-extra", "0.5Gi")
			assertPVCStorage(t, mcli, "cache-rfr-test-0", "0.5Gi")
			// should not call update
			mcli.PrependReactor("update", "persistentvolumeclaims", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
				t.Error("shouldn't call update")
				return true, nil, nil
			})
			err = service.CreateOrUpdateStatefulSet(testns, pvcStatefulSet("1Gi", owners))
			assertTest.NoError(err)
		})
	}
}

var rfOwnerReference = metav1.OwnerReference{
	APIVersion: "databases.spotahome.com/v1",
	Kind:       "RedisFailover",
	Name:       "test",
	UID:        "rf-uid",
	Controller: ptr.To(true),
}

// pvcStatefulSet returns a StatefulSet with a volume claim template, shaped
// like what generateRedisStatefulSet builds for a RedisFailover with storage.
func pvcStatefulSet(storage string, templateOwners []metav1.OwnerReference) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "rfr-test",
			Namespace:       "testns",
			OwnerReferences: []metav1.OwnerReference{rfOwnerReference},
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/component": "redis",
				"app.kubernetes.io/name":      "test",
				"app.kubernetes.io/part-of":   "redis-failover",
			}},
			VolumeClaimTemplates: []v1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:            "data",
						OwnerReferences: templateOwners,
					},
					Spec: v1.PersistentVolumeClaimSpec{
						Resources: v1.VolumeResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceStorage: resource.MustParse(storage),
							},
						},
					},
				},
			},
		},
	}
}

// statefulSetPVC returns a PVC of the pvcStatefulSet, with the selector labels
// that the StatefulSet controller adds to each PVC.
func statefulSetPVC(name, storage string, owners ...metav1.OwnerReference) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "testns",
			ResourceVersion: "1",
			Labels: map[string]string{
				"app.kubernetes.io/component": "redis",
				"app.kubernetes.io/name":      "test",
				"app.kubernetes.io/part-of":   "redis-failover",
			},
			OwnerReferences: owners,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: resource.MustParse(storage),
				},
			},
		},
	}
}

// enforcePVCResourceVersion makes a PVC update fail with a conflict when its
// resourceVersion is old, as the API server does. The object tracker of the fake
// clientset does not check the resourceVersion.
func enforcePVCResourceVersion(mcli *kubernetes.Clientset) {
	mcli.PrependReactor("update", "persistentvolumeclaims", func(action kubetesting.Action) (bool, runtime.Object, error) {
		pvc := action.(kubetesting.UpdateAction).GetObject().(*v1.PersistentVolumeClaim)
		obj, err := mcli.Tracker().Get(pvcsGroup, action.GetNamespace(), pvc.Name)
		if err != nil {
			return true, nil, err
		}
		stored := obj.(*v1.PersistentVolumeClaim)
		if pvc.ResourceVersion != stored.ResourceVersion {
			return true, nil, kubeerrors.NewConflict(pvcsGroup.GroupResource(), pvc.Name, errors.New("the object has been modified"))
		}
		updated := pvc.DeepCopy()
		updated.ResourceVersion = nextResourceVersion(stored.ResourceVersion)
		if err := mcli.Tracker().Update(pvcsGroup, updated, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, updated.DeepCopy(), nil
	})
}

func nextResourceVersion(resourceVersion string) string {
	version, _ := strconv.Atoi(resourceVersion)
	return strconv.Itoa(version + 1)
}

func getPVC(t *testing.T, mcli *kubernetes.Clientset, name string) *v1.PersistentVolumeClaim {
	t.Helper()
	pvc, err := mcli.CoreV1().PersistentVolumeClaims("testns").Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc %s: %v", name, err)
	}
	return pvc
}

func assertPVCStorage(t *testing.T, mcli *kubernetes.Clientset, name, storage string) {
	t.Helper()
	got := getPVC(t, mcli, name).Spec.Resources.Requests.Storage()
	assert.Zero(t, got.Cmp(resource.MustParse(storage)), "pvc %s has %s, expected %s", name, got, storage)
}

func TestStatefulSetServiceCreateOrUpdatePVCs(t *testing.T) {
	otherOwner := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid"}

	tests := []struct {
		name                 string
		storedStorage        string
		storedAnnotations    map[string]string
		desiredStorage       string
		desiredOwners        []metav1.OwnerReference
		desiredReplicas      *int32
		pvcs                 []*v1.PersistentVolumeClaim
		errorOnList          error
		errorOnGet           error
		errorOnUpdate        error
		conflictOnUpdate     bool
		rejectResize         bool
		expErr               bool
		expStorage           map[string]string
		expOwners            map[string][]metav1.OwnerReference
		expAnnotations       map[string]map[string]string
		expStatefulSetUpdate bool
	}{
		{
			name: "A PVC that the StatefulSet created from its old template after a resize is resized.",
			// The template cannot be changed, so it keeps the size from before the resize.
			storedStorage:     "1Gi",
			storedAnnotations: map[string]string{"storageCapacity": "2147483648"},
			desiredStorage:    "2Gi",
			desiredOwners:     []metav1.OwnerReference{rfOwnerReference},
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "2Gi", rfOwnerReference),
				statefulSetPVC("data-rfr-test-1", "2Gi", rfOwnerReference),
				statefulSetPVC("data-rfr-test-2", "1Gi", rfOwnerReference),
			},
			expStorage: map[string]string{
				"data-rfr-test-0": "2Gi",
				"data-rfr-test-1": "2Gi",
				"data-rfr-test-2": "2Gi",
			},
		},
		{
			name:           "A PVC is not made smaller, because Kubernetes does not allow it.",
			storedStorage:  "2Gi",
			desiredStorage: "1Gi",
			desiredOwners:  []metav1.OwnerReference{rfOwnerReference},
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "2Gi", rfOwnerReference),
			},
			expStorage: map[string]string{"data-rfr-test-0": "2Gi"},
		},
		{
			name:           "keepAfterDeletion set on an existing RedisFailover removes its owner reference from the PVCs.",
			storedStorage:  "1Gi",
			desiredStorage: "1Gi",
			desiredOwners:  nil,
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
				statefulSetPVC("data-rfr-test-1", "1Gi", rfOwnerReference, otherOwner),
			},
			expOwners: map[string][]metav1.OwnerReference{
				"data-rfr-test-0": nil,
				"data-rfr-test-1": {otherOwner},
			},
		},
		{
			name:           "The PVCs keep the owner reference that the template references.",
			storedStorage:  "1Gi",
			desiredStorage: "1Gi",
			desiredOwners:  []metav1.OwnerReference{rfOwnerReference},
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
			},
			expOwners: map[string][]metav1.OwnerReference{
				"data-rfr-test-0": {rfOwnerReference},
			},
		},
		{
			name:            "A failed PVC update does not stop the StatefulSet update.",
			storedStorage:   "1Gi",
			desiredStorage:  "2Gi",
			desiredOwners:   []metav1.OwnerReference{rfOwnerReference},
			desiredReplicas: ptr.To(int32(3)),
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
			},
			errorOnUpdate:        errors.New("wanted error"),
			expStorage:           map[string]string{"data-rfr-test-0": "1Gi"},
			expStatefulSetUpdate: true,
		},
		{
			name:           "keepAfterDeletion and a resize in one reconcile change both, with the resourceVersion of the owner update.",
			storedStorage:  "1Gi",
			desiredStorage: "2Gi",
			desiredOwners:  nil,
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
			},
			expStorage: map[string]string{"data-rfr-test-0": "2Gi"},
			expOwners:  map[string][]metav1.OwnerReference{"data-rfr-test-0": nil},
		},
		{
			name:           "A conflict on the owner reference removal is retried on the current PVC.",
			storedStorage:  "1Gi",
			desiredStorage: "1Gi",
			desiredOwners:  nil,
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
			},
			conflictOnUpdate: true,
			expOwners:        map[string][]metav1.OwnerReference{"data-rfr-test-0": nil},
			expAnnotations:   map[string]map[string]string{"data-rfr-test-0": {"other-writer": "true"}},
		},
		{
			name:           "A failed PVC get for the owner reference removal returns an error.",
			storedStorage:  "1Gi",
			desiredStorage: "1Gi",
			desiredOwners:  nil,
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
			},
			errorOnGet: errors.New("wanted error"),
			expErr:     true,
		},
		{
			name:           "A rejected resize does not keep the owner reference that keepAfterDeletion removes.",
			storedStorage:  "1Gi",
			desiredStorage: "2Gi",
			desiredOwners:  nil,
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
			},
			rejectResize: true,
			expStorage:   map[string]string{"data-rfr-test-0": "1Gi"},
			expOwners:    map[string][]metav1.OwnerReference{"data-rfr-test-0": nil},
		},
		{
			name:           "A failed owner reference removal returns an error.",
			storedStorage:  "1Gi",
			desiredStorage: "1Gi",
			desiredOwners:  nil,
			pvcs: []*v1.PersistentVolumeClaim{
				statefulSetPVC("data-rfr-test-0", "1Gi", rfOwnerReference),
			},
			errorOnUpdate: errors.New("wanted error"),
			expErr:        true,
		},
		{
			name:           "A failed PVC list returns an error.",
			storedStorage:  "1Gi",
			desiredStorage: "2Gi",
			desiredOwners:  []metav1.OwnerReference{rfOwnerReference},
			errorOnList:    errors.New("wanted error"),
			expErr:         true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			stored := pvcStatefulSet(test.storedStorage, []metav1.OwnerReference{rfOwnerReference})
			stored.Annotations = test.storedAnnotations
			objects := []runtime.Object{stored}
			for _, pvc := range test.pvcs {
				objects = append(objects, pvc)
			}
			mcli := kubernetes.NewClientset(objects...)
			enforcePVCResourceVersion(mcli)
			if test.errorOnList != nil {
				mcli.PrependReactor("list", "persistentvolumeclaims", func(action kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, test.errorOnList
				})
			}
			if test.errorOnUpdate != nil {
				mcli.PrependReactor("update", "persistentvolumeclaims", func(action kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, test.errorOnUpdate
				})
			}

			if test.errorOnGet != nil {
				mcli.PrependReactor("get", "persistentvolumeclaims", func(action kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, test.errorOnGet
				})
			}
			if test.conflictOnUpdate {
				// Another controller writes the PVC between the get and the
				// first update of the operator.
				conflicted := false
				mcli.PrependReactor("update", "persistentvolumeclaims", func(action kubetesting.Action) (bool, runtime.Object, error) {
					if conflicted {
						return false, nil, nil
					}
					conflicted = true
					name := action.(kubetesting.UpdateAction).GetObject().(*v1.PersistentVolumeClaim).Name
					obj, err := mcli.Tracker().Get(pvcsGroup, action.GetNamespace(), name)
					if err != nil {
						return true, nil, err
					}
					other := obj.(*v1.PersistentVolumeClaim).DeepCopy()
					other.Annotations = map[string]string{"other-writer": "true"}
					other.ResourceVersion = nextResourceVersion(other.ResourceVersion)
					if err := mcli.Tracker().Update(pvcsGroup, other, action.GetNamespace()); err != nil {
						return true, nil, err
					}
					return true, nil, kubeerrors.NewConflict(pvcsGroup.GroupResource(), name, errors.New("the object has been modified"))
				})
			}
			if test.rejectResize {
				mcli.PrependReactor("update", "persistentvolumeclaims", func(action kubetesting.Action) (bool, runtime.Object, error) {
					pvc := action.(kubetesting.UpdateAction).GetObject().(*v1.PersistentVolumeClaim)
					if pvc.Spec.Resources.Requests.Storage().String() != "1Gi" {
						return true, nil, errors.New("the storage class does not allow expansion")
					}
					return false, nil, nil
				})
			}

			service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)
			desired := pvcStatefulSet(test.desiredStorage, test.desiredOwners)
			desired.Spec.Replicas = test.desiredReplicas
			err := service.CreateOrUpdateStatefulSet("testns", desired)

			if test.expErr {
				assert.Error(err)
				return
			}
			assert.NoError(err)
			for name, storage := range test.expStorage {
				assertPVCStorage(t, mcli, name, storage)
			}
			for name, owners := range test.expOwners {
				assert.ElementsMatch(owners, getPVC(t, mcli, name).OwnerReferences, "owner references of pvc %s", name)
			}
			for name, annotations := range test.expAnnotations {
				assert.Equal(annotations, getPVC(t, mcli, name).Annotations, "annotations of pvc %s", name)
			}
			statefulSetUpdated := false
			for _, action := range mcli.Actions() {
				if action.Matches("update", "statefulsets") {
					statefulSetUpdated = true
				}
			}
			assert.Equal(test.expStatefulSetUpdate, statefulSetUpdated, "StatefulSet update")
		})
	}
}

// realisticStatefulSet returns a StatefulSet shaped like what
// generateRedisStatefulSet actually builds: it explicitly sets
// UpdateStrategy and PodManagementPolicy (unlike the sentinel Deployment),
// but - like it - never sets Spec.RevisionHistoryLimit, PodSpec.
// RestartPolicy/SchedulerName, or container TerminationMessagePath/Policy,
// relying on the API server to default them.
func realisticStatefulSet(replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rfr-test",
			Namespace: "testns",
			Labels:    map[string]string{"app.kubernetes.io/name": "test", "app.kubernetes.io/component": "redis"},
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "rfr-test",
			Replicas:    ptr.To(replicas),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "test"}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "test"}},
				Spec: v1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
					Containers: []v1.Container{
						{Name: "redis", Image: "redis:7", ImagePullPolicy: v1.PullAlways},
					},
				},
			},
		},
	}
}

// serverDefaultedSTS is realisticDeployment's serverDefaulted counterpart for
// StatefulSet.
func serverDefaultedSTS(s *appsv1.StatefulSet) *appsv1.StatefulSet {
	s = s.DeepCopy()
	s.Spec.RevisionHistoryLimit = ptr.To(int32(10))
	s.Spec.Template.Spec.RestartPolicy = v1.RestartPolicyAlways
	s.Spec.Template.Spec.SchedulerName = "default-scheduler"
	s.Spec.Template.Spec.DeprecatedServiceAccount = s.Spec.Template.Spec.ServiceAccountName
	for i := range s.Spec.Template.Spec.Containers {
		s.Spec.Template.Spec.Containers[i].TerminationMessagePath = "/dev/termination-log"
		s.Spec.Template.Spec.Containers[i].TerminationMessagePolicy = v1.TerminationMessageReadFile
	}
	return s
}

func TestStatefulSetServiceObjectUpToDate(t *testing.T) {
	testns := "testns"

	tests := []struct {
		name          string
		stored        *appsv1.StatefulSet
		desired       *appsv1.StatefulSet
		expectUpdates int
	}{
		{
			name:          "identical desired is a no-op",
			stored:        realisticStatefulSet(3),
			desired:       realisticStatefulSet(3),
			expectUpdates: 0,
		},
		{
			name:          "server-defaulted fields the operator never sets do not trigger an update",
			stored:        serverDefaultedSTS(realisticStatefulSet(3)),
			desired:       realisticStatefulSet(3),
			expectUpdates: 0,
		},
		{
			name: "no custom RedisFailover annotations still converges to a no-op",
			// CreateOrUpdateStatefulSet re-merges stored's annotations into
			// desired on every reconcile; util.MergeAnnotations always
			// allocates a map even for two nils, while a real StatefulSet
			// with no annotations set comes back from the API server with a
			// nil map. Both stored and desired have nil ObjectMeta.
			// Annotations here, matching a RedisFailover with no
			// .metadata.annotations set.
			stored:        realisticStatefulSet(3),
			desired:       realisticStatefulSet(3),
			expectUpdates: 0,
		},
		{
			name:          "a real spec change still triggers an update",
			stored:        realisticStatefulSet(3),
			desired:       realisticStatefulSet(5),
			expectUpdates: 1,
		},
		{
			name:          "a label change still triggers an update",
			stored:        realisticStatefulSet(3),
			desired:       func() *appsv1.StatefulSet { s := realisticStatefulSet(3); s.Labels["extra"] = "value"; return s }(),
			expectUpdates: 1,
		},
		{
			name: "manual drift on the live object is detected and corrected, even though desired is unchanged",
			// stored's replica count was changed by hand since the
			// operator's last write; desired is exactly what the operator
			// always builds for this RedisFailover.
			stored:        realisticStatefulSet(9),
			desired:       realisticStatefulSet(3),
			expectUpdates: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			stored := test.stored.DeepCopy()
			stored.ResourceVersion = "1"
			mcli := kubernetes.NewClientset(stored)

			service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)
			assert.NoError(service.CreateOrUpdateStatefulSet(testns, test.desired.DeepCopy()))

			updates := 0
			for _, a := range mcli.Actions() {
				if a.GetVerb() == "update" && a.GetResource().Resource == "statefulsets" {
					updates++
				}
			}
			assert.Equal(test.expectUpdates, updates)
		})
	}
}

func TestStatefulSetServiceDeleteStatefulSet(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing StatefulSet", func(t *testing.T) {
		assertTest := assert.New(t)

		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "teststatefulset",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(sts)
		service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteStatefulSet(testns, "teststatefulset")
		assertTest.NoError(err)

		_, err = mcli.AppsV1().StatefulSets(testns).Get(context.TODO(), "teststatefulset", metav1.GetOptions{})
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})

	t.Run("returns an error when deleting a non-existent StatefulSet", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteStatefulSet(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestStatefulSetServiceListStatefulSets(t *testing.T) {
	assertTest := assert.New(t)
	testns := "testns"

	sts1 := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "sts1", Namespace: testns}}
	sts2 := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "sts2", Namespace: testns}}
	otherNsSts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "sts3", Namespace: "otherns"}}

	mcli := kubernetes.NewClientset(sts1, sts2, otherNsSts)
	service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)

	list, err := service.ListStatefulSets(testns)
	assertTest.NoError(err)
	assertTest.Len(list.Items, 2)
}
