package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubernetes "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

var (
	deploymentsGroup = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

func newDeploymentUpdateAction(ns string, deployment *appsv1.Deployment) kubetesting.UpdateActionImpl {
	return kubetesting.NewUpdateAction(deploymentsGroup, ns, deployment)
}

func newDeploymentGetAction(ns, name string) kubetesting.GetActionImpl {
	return kubetesting.NewGetAction(deploymentsGroup, ns, name)
}

func newDeploymentCreateAction(ns string, deployment *appsv1.Deployment) kubetesting.CreateActionImpl {
	return kubetesting.NewCreateAction(deploymentsGroup, ns, deployment)
}

func newDeploymentDeleteAction(ns, name string) kubetesting.DeleteActionImpl {
	propagation := metav1.DeletePropagationForeground
	return kubetesting.NewDeleteActionWithOptions(deploymentsGroup, ns, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
}

func newDeploymentListAction(ns string) kubetesting.ListActionImpl {
	return kubetesting.NewListAction(deploymentsGroup, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, ns, metav1.ListOptions{})
}

func TestDeploymentServiceGetCreateOrUpdate(t *testing.T) {
	testDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "testdeployment1",
			ResourceVersion: "10",
		},
	}

	testns := "testns"

	tests := []struct {
		name                string
		deployment          *appsv1.Deployment
		getDeploymentResult *appsv1.Deployment
		errorOnGet          error
		errorOnCreation     error
		expActions          []kubetesting.Action
		expErr              bool
	}{
		{
			name:                "A new deployment should create a new deployment.",
			deployment:          testDeployment,
			getDeploymentResult: nil,
			errorOnGet:          kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:     nil,
			expActions: []kubetesting.Action{
				newDeploymentGetAction(testns, testDeployment.Name),
				newDeploymentCreateAction(testns, testDeployment),
			},
			expErr: false,
		},
		{
			name:                "A new deployment should error when create a new deployment fails.",
			deployment:          testDeployment,
			getDeploymentResult: nil,
			errorOnGet:          kubeerrors.NewNotFound(schema.GroupResource{}, ""),
			errorOnCreation:     errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newDeploymentGetAction(testns, testDeployment.Name),
				newDeploymentCreateAction(testns, testDeployment),
			},
			expErr: true,
		},
		{
			// The stored and desired objects must actually differ here: an
			// identical desired object is now a no-op (see
			// TestDeploymentServiceObjectUpToDate) and would issue no Update
			// action, defeating the point of this test.
			name: "An existent deployment should update the deployment.",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "testdeployment1"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
			},
			getDeploymentResult: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "testdeployment1", ResourceVersion: "10"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
			},
			errorOnGet:      nil,
			errorOnCreation: nil,
			expActions: []kubetesting.Action{
				newDeploymentGetAction(testns, "testdeployment1"),
				newDeploymentUpdateAction(testns, &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "testdeployment1", ResourceVersion: "10"},
					Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
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
			mcli.AddReactor("get", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, test.getDeploymentResult, test.errorOnGet
			})
			mcli.AddReactor("create", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnCreation
			})

			service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)
			err := service.CreateOrUpdateDeployment(testns, test.deployment)

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

// realisticDeployment returns a Deployment shaped like what
// generateSentinelDeployment actually builds: it deliberately never sets
// Spec.Strategy, Spec.RevisionHistoryLimit, Spec.ProgressDeadlineSeconds,
// PodSpec.RestartPolicy/SchedulerName, or container
// TerminationMessagePath/Policy, relying on the API server to default them.
func realisticDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rfs-test",
			Namespace: "testns",
			Labels:    map[string]string{"app.kubernetes.io/name": "test", "app.kubernetes.io/component": "sentinel"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "test"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "sentinel", Image: "redis:7", ImagePullPolicy: corev1.PullAlways},
					},
				},
			},
		},
	}
}

// serverDefaulted returns a copy of d with the fields a real API server
// fills in on write that generateSentinelDeployment never sets, simulating
// what GET would return for an object this operator itself created and
// nothing else touched.
func serverDefaulted(d *appsv1.Deployment) *appsv1.Deployment {
	d = d.DeepCopy()
	d.Spec.Strategy = appsv1.DeploymentStrategy{
		Type:          appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{},
	}
	d.Spec.RevisionHistoryLimit = ptr.To(int32(10))
	d.Spec.ProgressDeadlineSeconds = ptr.To(int32(600))
	d.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	d.Spec.Template.Spec.SchedulerName = "default-scheduler"
	d.Spec.Template.Spec.DeprecatedServiceAccount = d.Spec.Template.Spec.ServiceAccountName
	for i := range d.Spec.Template.Spec.Containers {
		d.Spec.Template.Spec.Containers[i].TerminationMessagePath = "/dev/termination-log"
		d.Spec.Template.Spec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	return d
}

func withStrategy(d *appsv1.Deployment, s appsv1.DeploymentStrategy) *appsv1.Deployment {
	d.Spec.Strategy = s
	return d
}

func recreate() appsv1.DeploymentStrategy {
	return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
}

func rollingUpdate(maxSurge, maxUnavailable *intstr.IntOrString) appsv1.DeploymentStrategy {
	return appsv1.DeploymentStrategy{
		Type:          appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: maxSurge, MaxUnavailable: maxUnavailable},
	}
}

func TestDeploymentServiceObjectUpToDate(t *testing.T) {
	testns := "testns"

	tests := []struct {
		name          string
		stored        *appsv1.Deployment
		desired       *appsv1.Deployment
		expectUpdates int
	}{
		{
			name:          "identical desired is a no-op",
			stored:        realisticDeployment(3),
			desired:       realisticDeployment(3),
			expectUpdates: 0,
		},
		{
			name: "server-defaulted fields the operator never sets do not trigger an update",
			// stored is what a real API server would persist and return for
			// exactly this desired object - if the comparison isn't
			// normalized, this looks different on every single field the
			// server defaults, even though nothing meaningful changed.
			stored:        serverDefaulted(realisticDeployment(3)),
			desired:       realisticDeployment(3),
			expectUpdates: 0,
		},
		{
			name:          "a real spec change still triggers an update",
			stored:        realisticDeployment(3),
			desired:       realisticDeployment(5),
			expectUpdates: 1,
		},
		{
			name:          "a label change still triggers an update",
			stored:        realisticDeployment(3),
			desired:       func() *appsv1.Deployment { d := realisticDeployment(3); d.Labels["extra"] = "value"; return d }(),
			expectUpdates: 1,
		},
		{
			name: "manual drift on the live object is detected and corrected, even though desired is unchanged",
			// stored's replica count was changed by hand (kubectl scale,
			// or any actor other than this operator) since the operator's
			// last write; desired is exactly what the operator always
			// builds for this RedisFailover. This is the property the old
			// hash-annotation design (redis-operator PR #143) could not
			// provide, since it only ever compared desired against its own
			// previous value, never against what was actually live.
			stored:        realisticDeployment(9),
			desired:       realisticDeployment(3),
			expectUpdates: 1,
		},
		{
			name:          "a configured strategy the server stored as-is is a no-op",
			stored:        withStrategy(realisticDeployment(3), recreate()),
			desired:       withStrategy(realisticDeployment(3), recreate()),
			expectUpdates: 0,
		},
		{
			name:          "a partly configured strategy the server filled in is a no-op",
			stored:        withStrategy(realisticDeployment(3), rollingUpdate(ptr.To(intstr.FromInt32(1)), ptr.To(intstr.FromString("25%")))),
			desired:       withStrategy(realisticDeployment(3), rollingUpdate(ptr.To(intstr.FromInt32(1)), nil)),
			expectUpdates: 0,
		},
		{
			name:          "a changed strategy triggers an update",
			stored:        withStrategy(realisticDeployment(3), rollingUpdate(ptr.To(intstr.FromInt32(1)), ptr.To(intstr.FromString("25%")))),
			desired:       withStrategy(realisticDeployment(3), recreate()),
			expectUpdates: 1,
		},
		{
			name:          "removing a configured strategy triggers an update",
			stored:        withStrategy(realisticDeployment(3), recreate()),
			desired:       realisticDeployment(3),
			expectUpdates: 1,
		},
		{
			name: "a deployment-controller-owned annotation on stored does not trigger an update",
			// generateSentinelDeployment never sets Deployment-level
			// annotations, but the deployment controller stamps
			// deployment.kubernetes.io/revision on every rollout. Comparing
			// annotations as-is would treat this as a permanent difference.
			stored: func() *appsv1.Deployment {
				d := realisticDeployment(3)
				d.Annotations = map[string]string{"deployment.kubernetes.io/revision": "3"}
				return d
			}(),
			desired:       realisticDeployment(3),
			expectUpdates: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			stored := test.stored.DeepCopy()
			stored.ResourceVersion = "1"
			mcli := kubernetes.NewClientset(stored)

			service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)
			assert.NoError(service.CreateOrUpdateDeployment(testns, test.desired.DeepCopy()))

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

func TestDeploymentServiceUpdate(t *testing.T) {
	testDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testdeployment1",
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
			name:          "Updating an existent deployment should not error.",
			errorOnUpdate: nil,
			expActions: []kubetesting.Action{
				newDeploymentUpdateAction(testns, testDeployment),
			},
			expErr: false,
		},
		{
			name:          "Updating should error when the client fails.",
			errorOnUpdate: errors.New("wanted error"),
			expActions: []kubetesting.Action{
				newDeploymentUpdateAction(testns, testDeployment),
			},
			expErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertTest := assert.New(t)

			mcli := &kubernetes.Clientset{}
			mcli.AddReactor("update", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.errorOnUpdate
			})

			service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)
			err := service.UpdateDeployment(testns, testDeployment)

			if test.expErr {
				assertTest.Error(err)
			} else {
				assertTest.NoError(err)
			}
			assertTest.Equal(test.expActions, mcli.Actions())
		})
	}
}

func TestDeploymentServiceDelete(t *testing.T) {
	testns := "testns"

	t.Run("deletes an existing deployment", func(t *testing.T) {
		assertTest := assert.New(t)

		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testdeployment1",
				Namespace: testns,
			},
		}
		mcli := kubernetes.NewClientset(deployment)
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteDeployment(testns, "testdeployment1")
		assertTest.NoError(err)
		assertTest.Equal([]kubetesting.Action{newDeploymentDeleteAction(testns, "testdeployment1")}, mcli.Actions())

		_, getErr := mcli.AppsV1().Deployments(testns).Get(context.TODO(), "testdeployment1", metav1.GetOptions{})
		assertTest.Error(getErr)
		assertTest.True(kubeerrors.IsNotFound(getErr))
	})

	t.Run("returns a not found error when the deployment does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		err := service.DeleteDeployment(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
	})
}

func TestDeploymentServiceList(t *testing.T) {
	testns := "testns"

	t.Run("lists deployments in a namespace", func(t *testing.T) {
		assertTest := assert.New(t)

		d1 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d1", Namespace: testns}}
		d2 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d2", Namespace: testns}}
		d3 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d3", Namespace: "otherns"}}
		mcli := kubernetes.NewClientset(d1, d2, d3)
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListDeployments(testns)
		assertTest.NoError(err)
		assertTest.Len(list.Items, 2)
		names := []string{list.Items[0].Name, list.Items[1].Name}
		assertTest.ElementsMatch([]string{"d1", "d2"}, names)
	})

	t.Run("returns an error when the client fails", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := &kubernetes.Clientset{}
		wantErr := errors.New("wanted error")
		mcli.AddReactor("list", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
			return true, nil, wantErr
		})
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		list, err := service.ListDeployments(testns)
		assertTest.Error(err)
		assertTest.Empty(list.Items)
		assertTest.Equal([]kubetesting.Action{newDeploymentListAction(testns)}, mcli.Actions())
	})
}

func TestDeploymentServiceGetDeploymentPods(t *testing.T) {
	testns := "testns"

	t.Run("returns pods matching the deployment's selector", func(t *testing.T) {
		assertTest := assert.New(t)

		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "testdeployment1", Namespace: testns},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "redis"},
				},
			},
		}
		matchingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "matchingpod",
				Namespace: testns,
				Labels:    map[string]string{"app": "redis"},
			},
		}
		otherPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "otherpod",
				Namespace: testns,
				Labels:    map[string]string{"app": "other"},
			},
		}
		mcli := kubernetes.NewClientset(deployment, matchingPod, otherPod)
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		pods, err := service.GetDeploymentPods(testns, "testdeployment1")
		assertTest.NoError(err)
		assertTest.Len(pods.Items, 1)
		assertTest.Equal("matchingpod", pods.Items[0].Name)
	})

	t.Run("returns an error when the deployment does not exist", func(t *testing.T) {
		assertTest := assert.New(t)

		mcli := kubernetes.NewClientset()
		service := k8s.NewDeploymentService(mcli, log.Dummy, metrics.Dummy)

		pods, err := service.GetDeploymentPods(testns, "does-not-exist")
		assertTest.Error(err)
		assertTest.True(kubeerrors.IsNotFound(err))
		assertTest.Nil(pods)
	})
}
