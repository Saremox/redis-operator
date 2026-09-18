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
	configMapsGroup = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
)

func newConfigMapUpdateAction(ns string, configMap *corev1.ConfigMap) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(configMapsGroup, ns, configMap)
}

func newConfigMapGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(configMapsGroup, ns, name)
}

func newConfigMapCreateAction(ns string, configMap *corev1.ConfigMap) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(configMapsGroup, ns, configMap)
}

func newConfigMapDeleteAction(ns, name string) kubetesting.DeleteActionImpl {
	return kubetesting.NewDeleteAction(configMapsGroup, ns, name)
}

func newConfigMapListAction(ns string) kubetesting.ListActionImpl {
	return kubetesting.NewListAction(configMapsGroup, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}, ns, metav1.ListOptions{})
}

func TestConfigMapServiceGetCreateOrUpdate(t *testing.T) {
	testConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testconfigmap1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name               string
		configMap          *corev1.ConfigMap
		getConfigMapResult *corev1.ConfigMap
		errorOnGet         error
		errorOnCreation    error
		expActions         []kubetesting.Action
		expErr             bool
	}{
		{
			name:               "A new configmap should create a new configmap.",
			configMap:          testConfigMap,
			getConfigMapResult: nil,
			errorOnGet:         kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:    nil,
			expActions: []kubetesting.Action{
				newConfigMapGetAction(testns, testConfigMap.Name),
				newConfigMapCreateAction(testns, testConfigMap),
			},
			expErr: false,
		},
		{
			name:               "A new configmap should error when create a new configmap fails.",
			configMap:          testConfigMap,
			getConfigMapResult: nil,
			errorOnGet:         kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:    errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newConfigMapGetAction(testns, testConfigMap.Name),
				newConfigMapCreateAction(testns, testConfigMap),
			},
			expErr: true,
		},
		{
			// The stored and desired objects must actually differ here: an
			// identical desired object is now a no-op (see
			// TestConfigMapServiceObjectUpToDate) and would issue no Update
			// action, defeating the point of this test.
			name: "An existent configmap should update the configmap.",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "testconfigmap1"},
				Data:       map[string]string{"key": "new-value"},
			},
			getConfigMapResult: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "testconfigmap1", ResourceVersion: "10"},
				Data:       map[string]string{"key": "old-value"},
			},
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newConfigMapGetAction(testns, "testconfigmap1"),
				newConfigMapUpdateAction(testns, &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "testconfigmap1", ResourceVersion: "10"},
					Data:       map[string]string{"key": "new-value"},
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
			mcli.AddReactor("get", "configmaps", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getConfigMapResult, test.errorOnGet
			})
			mcli.AddReactor("create", "configmaps", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewConfigMapService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateConfigMap(testns, test.configMap)

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

func TestConfigMapServiceObjectUpToDate(t *testing.T) {
	testns := "testns"

	realistic := func(value string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rfr-test",
				Namespace: "testns",
				Labels:    map[string]string{"app.kubernetes.io/name": "test"},
			},
			Data: map[string]string{"redis.conf": value},
		}
	}

	tests := []struct {
		name          string
		stored        *corev1.ConfigMap
		desired       *corev1.ConfigMap
		expectUpdates int
	}{
		{
			name:          "identical desired is a no-op",
			stored:        realistic("port 6379"),
			desired:       realistic("port 6379"),
			expectUpdates: 0,
		},
		{
			name:          "a real data change still triggers an update",
			stored:        realistic("port 6379"),
			desired:       realistic("port 6380"),
			expectUpdates: 1,
		},
		{
			name:          "manual drift on the live object is detected and corrected, even though desired is unchanged",
			stored:        realistic("hand-edited"),
			desired:       realistic("port 6379"),
			expectUpdates: 1,
		},
		{
			name: "a label change still triggers an update",
			stored: func() *corev1.ConfigMap {
				cm := realistic("port 6379")
				cm.Labels["extra"] = "value"
				return cm
			}(),
			desired:       realistic("port 6379"),
			expectUpdates: 1,
		},
		{
			name: "an annotation change still triggers an update",
			stored: func() *corev1.ConfigMap {
				cm := realistic("port 6379")
				cm.Annotations = map[string]string{"checksum": "abc"}
				return cm
			}(),
			desired:       realistic("port 6379"),
			expectUpdates: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			stored := test.stored.DeepCopy()
			stored.ResourceVersion = "1"
			mcli := kubernetes.NewClientset(stored)

			service := k8s.NewConfigMapService(mcli, log.Dummy, metrics.Dummy)
			assert.NoError(service.CreateOrUpdateConfigMap(testns, test.desired.DeepCopy()))

			updates := 0
			for _, a := range mcli.Actions() {
				if a.GetVerb() == "update" {
					updates++
				}
			}
			assert.Equal(test.expectUpdates, updates)
		})
	}
}

func TestConfigMapServiceUpdate(t *testing.T) {
	testConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testconfigmap1",
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
			name:          "Updating an existent configmap should not error.",
			errorOnUpdate: nil,
			expActions: []kubetesting.Action{
				newConfigMapUpdateAction(testns, testConfigMap),
			},
			expErr: false,
		},
		{
			name:          "Updating should error when the client fails.",
			errorOnUpdate: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newConfigMapUpdateAction(testns, testConfigMap),
			},
			expErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("update", "configmaps", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnUpdate
			})

			service := k8s.NewConfigMapService(mcli, log.Dummy, metrics.Dummy)
			err := service.UpdateConfigMap(testns, testConfigMap)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			assertTest.Equal(test.expActions, mcli.Actions())
		})
	}
}

func TestConfigMapServiceDelete(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing configmap", func(t *testing.T) {
		assertTest := assert.New(t)

		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testconfigmap1",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(configMap)
		service := k8s.NewConfigMapService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteConfigMap(testns, "testconfigmap1")
		assertTest.NoError(err)
		assertTest.Equal([]kubetesting.Action{newConfigMapDeleteAction(testns, "testconfigmap1")}, mcli.Actions())

		_, getErr := mcli.CoreV1().ConfigMaps(testns).Get(context.TODO(), "testconfigmap1", metav1.GetOptions{})
		assertTest.Error(getErr)
		assertTest.True(kubeerrors.IsNotFound(getErr))
	})

	t.Run("returns a not found error when the configmap does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewConfigMapService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteConfigMap(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestConfigMapServiceList(t *testing.T) {
	testns := "testns"

	t.Run("lists configmaps in a namespace", func(t *testing.T) {
		assertTest := assert.New(t)

		cm1 := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: testns}}
		cm2 := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm2", Namespace: testns}}
		cm3 := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm3", Namespace: "otherns"}}
		mcli := kubernetes.NewClientset(cm1, cm2, cm3)
		service := k8s.NewConfigMapService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListConfigMaps(testns)
		assertTest.NoError(err)
		assertTest.Len(list.Items, 2)
		names := []string{list.Items[0].Name, list.Items[1].Name}
		assertTest.ElementsMatch([]string{"cm1", "cm2"}, names)
	})

	t.Run("returns an error when the client fails", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := &kubernetes.Clientset{}
		wantErr := errors.New("wanted error")
		mcli.AddReactor("list", "configmaps", func(action kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, wantErr
		})
		service := k8s.NewConfigMapService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListConfigMaps(testns)
		assertTest.Error(err)
		assertTest.Empty(list.Items)
		assertTest.Equal([]kubetesting.Action{newConfigMapListAction(testns)}, mcli.Actions())
	})
}
