package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
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
	podsGroup = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
)

func newPodUpdateAction(ns string, pod *corev1.Pod) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(podsGroup, ns, pod)
}

func newPodGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(podsGroup, ns, name)
}

func newPodCreateAction(ns string, pod *corev1.Pod) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(podsGroup, ns, pod)
}

func newPodDeleteAction(ns, name string) kubetesting.DeleteActionImpl {
	return kubetesting.NewDeleteAction(podsGroup, ns, name)
}

func newPodListAction(ns string) kubetesting.ListActionImpl {
	return kubetesting.NewListAction(podsGroup, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, ns, metav1.ListOptions{})
}

// TestPodServiceUpdatePodLabels exercises UpdatePodLabels, the mechanism
// behind master/slave role-label updates during failover. It builds a JSON
// Patch with Op "replace", which per RFC 6902 requires the target path to
// already exist -- these tests prove that behavior against a real fake
// clientset rather than assuming it.
func TestPodServiceUpdatePodLabels(t *testing.T) {
	testns := "testns"

	t.Run("updates an existing label to a new value", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: testns,
				Labels: map[string]string{
					"role": "slave",
				},
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "testpod", map[string]string{"role": "master"})
		assertTest.NoError(err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		assertTest.NoError(err)
		assertTest.Equal("master", got.Labels["role"])
	})

	t.Run("updates multiple existing labels at once", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: testns,
				Labels: map[string]string{
					"role":  "slave",
					"ready": "false",
				},
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "testpod", map[string]string{
			"role":  "master",
			"ready": "true",
		})
		assertTest.NoError(err)

		got, err := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		assertTest.NoError(err)
		assertTest.Equal("master", got.Labels["role"])
		assertTest.Equal("true", got.Labels["ready"])
	})

	t.Run("returns an error when the pod does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "does-not-exist", map[string]string{"role": "master"})
		assertTest.Error(err)
	})

	t.Run("documents actual replace semantics: a label key absent from the pod but with an existing labels map is upserted, not rejected", func(t *testing.T) {
		// NOTE: strict RFC 6902 says "replace" must fail when the target path
		// does not already exist. In practice, the JSON Patch library used
		// here (gopkg.in/evanphx/json-patch.v4, via client-go's fake and real
		// Patch codepaths) does NOT enforce that for object/map members: its
		// "replace" implementation only errors when some *ancestor* path
		// segment is entirely missing (see the next sub-test, where the pod
		// has no labels map at all). When the labels map already exists --
		// true for every pod in this codebase, since a default role label is
		// baked into the pod template at creation -- "replace" against a
		// label key that isn't present yet silently succeeds and adds it,
		// behaving like an upsert rather than a strict replace. This is a
		// real deviation from RFC 6902 in the dependency, not a bug in
		// UpdatePodLabels itself -- documented here rather than fixed.
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: testns,
				Labels: map[string]string{
					"role": "slave",
				},
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "testpod", map[string]string{"new-label-key": "value"})
		assertTest.NoError(err)

		got, getErr := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod", metav1.GetOptions{})
		assertTest.NoError(getErr)
		assertTest.Equal("slave", got.Labels["role"], "pre-existing labels are left untouched")
		assertTest.Equal("value", got.Labels["new-label-key"], "the new key is upserted rather than rejected")
	})

	t.Run("replace fails when the pod has no labels map at all", func(t *testing.T) {
		// This is the actual failure mode of "replace": it errors only when
		// an ancestor container of the target path (here, /metadata/labels
		// itself) does not exist on the object, not merely when the leaf key
		// is missing. Every pod in this codebase gets a default role label at
		// creation, so its labels map always exists in practice -- this case
		// documents what would happen if that ever weren't true.
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nolabelspod",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.UpdatePodLabels(testns, "nolabelspod", map[string]string{"role": "master"})
		assertTest.Error(err, "replace against a path whose parent container is entirely absent must fail")
	})
}

func TestPodServiceGetCreateOrUpdate(t *testing.T) {
	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testpod1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name            string
		pod             *corev1.Pod
		getPodResult    *corev1.Pod
		errorOnGet      error
		errorOnCreation error
		expActions      []kubetesting.Action
		expErr          bool
	}{
		{
			name:            "A new pod should create a new pod.",
			pod:             testPod,
			getPodResult:    nil,
			errorOnGet:      kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newPodGetAction(testns, testPod.Name),
				newPodCreateAction(testns, testPod),
			},
			expErr: false,
		},
		{
			name:            "A new pod should error when create a new pod fails.",
			pod:             testPod,
			getPodResult:    nil,
			errorOnGet:      kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newPodGetAction(testns, testPod.Name),
				newPodCreateAction(testns, testPod),
			},
			expErr: true,
		},
		{
			name:            "An existent pod should update the pod.",
			pod:             testPod,
			getPodResult:    testPod,
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newPodGetAction(testns, testPod.Name),
				newPodUpdateAction(testns, testPod),
			},
			expErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			// Mock.
			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("get", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getPodResult, test.errorOnGet
			})
			mcli.AddReactor("create", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdatePod(testns, test.pod)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
				// Check calls to kubernetes.
				assertTest.Equal(test.expActions, mcli.Actions())
			}
		})
	}
}

func TestPodServiceUpdate(t *testing.T) {
	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testpod1",
		},
	}

	testns := "testns"

	tests := []struct {
		name          string
		errorOnUpdate error
		expActions    []kubetesting.Action
		expErr        bool
	}{
		{
			name:          "Updating an existent pod should not error.",
			errorOnUpdate: nil,
			expActions: []kubetesting.Action{
				newPodUpdateAction(testns, testPod),
			},
			expErr: false,
		},
		{
			name:          "Updating should error when the client fails.",
			errorOnUpdate: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newPodUpdateAction(testns, testPod),
			},
			expErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("update", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnUpdate
			})

			service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)
			err := service.UpdatePod(testns, testPod)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			assertTest.Equal(test.expActions, mcli.Actions())
		})
	}
}

func TestPodServiceDelete(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing pod", func(t *testing.T) {
		assertTest := assert.New(t)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod1",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(pod)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeletePod(testns, "testpod1")
		assertTest.NoError(err)
		assertTest.Equal([]kubetesting.Action{newPodDeleteAction(testns, "testpod1")}, mcli.Actions())

		_, getErr := mcli.CoreV1().Pods(testns).Get(context.TODO(), "testpod1", metav1.GetOptions{})
		assertTest.Error(getErr)
		assertTest.True(kubeerrors.IsNotFound(getErr))
	})

	t.Run("returns a not found error when the pod does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeletePod(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestPodServiceList(t *testing.T) {
	testns := "testns"

	t.Run("lists pods in a namespace", func(t *testing.T) {
		assertTest := assert.New(t)

		p1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: testns}}
		p2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: testns}}
		p3 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "otherns"}}
		mcli := kubernetes.NewClientset(p1, p2, p3)
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListPods(testns)
		assertTest.NoError(err)
		assertTest.Len(list.Items, 2)
		names := []string{list.Items[0].Name, list.Items[1].Name}
		assertTest.ElementsMatch([]string{"p1", "p2"}, names)
	})

	t.Run("returns an error when the client fails", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := &kubernetes.Clientset{}
		wantErr := errors.New("wanted error")
		mcli.AddReactor("list", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, wantErr
		})
		service := k8s.NewPodService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListPods(testns)
		assertTest.Error(err)
		assertTest.Empty(list.Items)
		assertTest.Equal([]kubetesting.Action{newPodListAction(testns)}, mcli.Actions())
	})
}
