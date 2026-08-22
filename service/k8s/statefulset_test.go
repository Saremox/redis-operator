package k8s_test

import (
	"errors"
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

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

var (
	statefulSetsGroup = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
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

// TestStatefulSetServiceGetStatefulSetPods exercises the real selector-building
// logic in GetStatefulSetPods: it must list only the pods that belong to the
// named StatefulSet, scoped by both the StatefulSet's own MatchLabels selector
// and namespace. This is the exact property a recent fix relies on to prevent
// replicas from attaching to the wrong RedisFailover's master after pod-IP
// reuse across namespaces, so it uses a real fake clientset (not hand-rolled
// reactors) to get genuine label-selector and namespace filtering semantics.
func TestStatefulSetServiceGetStatefulSetPods(t *testing.T) {
	testns := "testns"
	otherns := "otherns"
	stsName := "teststatefulset"

	testStatefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stsName,
			Namespace: testns,
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":       "redis",
					"component": "master",
				},
			},
		},
	}

	matchingPod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matching-pod-1",
			Namespace: testns,
			Labels: map[string]string{
				"app":       "redis",
				"component": "master",
			},
		},
	}
	matchingPod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "matching-pod-2",
			Namespace: testns,
			Labels: map[string]string{
				"app":       "redis",
				"component": "master",
				"extra":     "label-should-not-matter",
			},
		},
	}
	// Same namespace, but does not carry all of the StatefulSet's selector labels.
	nonMatchingLabelsPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "non-matching-labels-pod",
			Namespace: testns,
			Labels: map[string]string{
				"app":       "redis",
				"component": "slave",
			},
		},
	}
	// Different namespace, but carries the exact same labels as the matching pods.
	// This proves the selector is correctly namespace-scoped: same labels alone
	// must not be enough to match across namespaces (the #698-class fix property).
	crossNamespacePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-namespace-pod",
			Namespace: otherns,
			Labels: map[string]string{
				"app":       "redis",
				"component": "master",
			},
		},
	}

	mcli := kubernetes.NewClientset(
		testStatefulSet,
		matchingPod1,
		matchingPod2,
		nonMatchingLabelsPod,
		crossNamespacePod,
	)

	service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)

	t.Run("returns exactly the pods matching selector and namespace", func(t *testing.T) {
		assertTest := assert.New(t)

		podList, err := service.GetStatefulSetPods(testns, stsName)
		assertTest.NoError(err)
		assertTest.NotNil(podList)

		gotNames := map[string]bool{}
		for _, p := range podList.Items {
			gotNames[p.Name] = true
		}

		assertTest.Len(podList.Items, 2)
		assertTest.True(gotNames["matching-pod-1"])
		assertTest.True(gotNames["matching-pod-2"])
		assertTest.False(gotNames["non-matching-labels-pod"], "pod with a different label value must be excluded")
		assertTest.False(gotNames["cross-namespace-pod"], "pod in a different namespace with identical labels must be excluded")
	})

	t.Run("propagates the error when the StatefulSet itself does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		podList, err := service.GetStatefulSetPods(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.Nil(podList)
	})

	t.Run("an empty MatchLabels selector matches every pod in the namespace", func(t *testing.T) {
		// This documents the actual behavior of GetStatefulSetPods when
		// Spec.Selector.MatchLabels is empty: the joined selector string is
		// empty, which k8s treats as "select everything" (no restriction),
		// so every pod in the namespace is returned -- not zero pods.
		assertTest := assert.New(t)

		emptySelectorSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "empty-selector-sts",
				Namespace: testns,
			},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{},
				},
			},
		}

		mcli2 := kubernetes.NewClientset(
			emptySelectorSts,
			matchingPod1,
			matchingPod2,
			nonMatchingLabelsPod,
		)
		service2 := k8s.NewStatefulSetService(mcli2, log.Dummy, metrics.Dummy)

		podList, err := service2.GetStatefulSetPods(testns, "empty-selector-sts")
		assertTest.NoError(err)
		assertTest.NotNil(podList)
		assertTest.Len(podList.Items, 3, "an empty selector matches every pod in the namespace")
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
			name:                 "An existent statefulSet should update the statefulSet.",
			statefulSet:          testStatefulSet,
			getStatefulSetResult: testStatefulSet,
			errorOnGet:           nil,
			errorOnCreation:      nil,
			expActions: []kubetesting.Action{
				newStatefulSetGetAction(testns, testStatefulSet.Name),
				newStatefulSetUpdateAction(testns, testStatefulSet),
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
			beforeSts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "teststatefulSet1",
					ResourceVersion: "10",
				},
				Spec: appsv1.StatefulSetSpec{
					VolumeClaimTemplates: []v1.PersistentVolumeClaim{
						{
							Spec: v1.PersistentVolumeClaimSpec{
								Resources: v1.VolumeResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceStorage: resource.MustParse("0.5Gi"),
									},
								},
							},
						},
					},
				},
			}
			afterSts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "teststatefulSet1",
					ResourceVersion: "10",
				},
				Spec: appsv1.StatefulSetSpec{
					VolumeClaimTemplates: []v1.PersistentVolumeClaim{
						{
							Spec: v1.PersistentVolumeClaimSpec{
								Resources: v1.VolumeResourceRequirements{
									Requests: v1.ResourceList{
										v1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							},
						},
					},
				},
			}
			pvcList := &v1.PersistentVolumeClaimList{
				Items: []v1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app.kubernetes.io/component": "redis",
								"app.kubernetes.io/name":      "teststatefulSet1",
								"app.kubernetes.io/part-of":   "redis-failover",
							},
						},
						Spec: v1.PersistentVolumeClaimSpec{
							VolumeName: "vol-1",
							Resources: v1.VolumeResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("0.5Gi"),
								},
							},
						},
					},
					// resized already
					{
						Spec: v1.PersistentVolumeClaimSpec{
							VolumeName: "vol-2",
							Resources: v1.VolumeResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			}
			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "statefulsets", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, beforeSts, nil
			})
			mcli.AddReactor("list", "persistentvolumeclaims", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
				return true, pvcList, nil
			})
			mcli.AddReactor("update", "persistentvolumeclaims", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
				// update pvc[0]
				pvcList.Items[0] = *action.(kubetesting.UpdateActionImpl).Object.(*v1.PersistentVolumeClaim)
				return true, action.(kubetesting.UpdateActionImpl).Object, nil
			})
			service := k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateStatefulSet(testns, afterSts)
			assertTest.NoError(err)
			assertTest.Equal(pvcList.Items[0].Spec.Resources, pvcList.Items[1].Spec.Resources)
			// should not call update
			mcli.AddReactor("update", "persistentvolumeclaims", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
				panic("shouldn't call update")
			})
			service = k8s.NewStatefulSetService(mcli, log.Dummy, metrics.Dummy)
			err = service.CreateOrUpdateStatefulSet(testns, afterSts)
			assertTest.NoError(err)
		})
	}
}
