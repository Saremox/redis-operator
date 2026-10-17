package redisfailover

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/k8s"
)

func memoryLimit(q string) corev1.ResourceRequirements {
	l := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}
	return corev1.ResourceRequirements{Limits: l, Requests: l}
}

func controllerRevision(t *testing.T, name, limit string) *appsv1.ControllerRevision {
	t.Helper()
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "redis", Image: "redis:7", Resources: memoryLimit(limit)}}}}
	raw, err := json.Marshal(map[string]any{"spec": map[string]any{"template": template}})
	require.NoError(t, err)
	return &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: runtime.RawExtension{Raw: raw}}
}

// inPlaceTest is a rolloutTest in mode Auto with the healer of the operator.
// The pods run on the revision "old" with 1Gi, and the update revision "new"
// of the step has 2Gi. The k8s mock keeps the pod changes.
type inPlaceTest struct {
	*rolloutTest
	healer  *rfservice.RedisFailoverHealer
	updated string
	// resizeErr is the answer of the API server to a resize.
	resizeErr error
	// lagging makes the StatefulSet status show an earlier generation.
	lagging bool
}

func newInPlaceTest(t *testing.T) *inPlaceTest {
	t.Helper()
	rt := newRolloutTest(t)
	rt.synced = true
	memoryMax := resource.MustParse("4Gi")
	rt.rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeAuto, MemoryMax: &memoryMax}
	rt.rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	rt.rf.Spec.Redis.Resources = memoryLimit("2Gi")
	for i := range rt.pods {
		pod := &rt.pods[i]
		pod.Labels[appsv1.ControllerRevisionHashLabelKey] = "old"
		pod.Status.Conditions = podReady
		pod.Spec.Containers = []corev1.Container{{Name: "redis", Resources: memoryLimit("1Gi")}}
		applied := memoryLimit("1Gi")
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "redis", Resources: &applied, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	}
	it := &inPlaceTest{rolloutTest: rt, updated: "new"}
	it.setUpdateRevision("new")
	pod := func(name string) *corev1.Pod {
		i := slices.IndexFunc(rt.pods, func(p corev1.Pod) bool { return p.Name == name })
		require.GreaterOrEqual(t, i, 0, name)
		return &rt.pods[i]
	}
	mk := rt.k8s
	mk.On("PodResizeSupport").Return(k8s.PodResizeSupport{Supported: true}, nil)
	mk.On("GetPod", "testns", mock.Anything).Return(func(_, name string) (*corev1.Pod, error) { return pod(name).DeepCopy(), nil })
	mk.On("GetControllerRevision", "testns", "old").Return(controllerRevision(t, "old", "1Gi"), nil)
	mk.On("GetControllerRevision", "testns", "new").Return(controllerRevision(t, "new", "2Gi"), nil)
	mk.On("ResizePod", "testns", mock.Anything, mock.Anything).Return(func(_, name string, desired map[string]corev1.ResourceRequirements) error {
		if it.resizeErr != nil {
			return it.resizeErr
		}
		pod(name).Spec.Containers[0].Resources = desired["redis"]
		return nil
	})
	mk.On("UpdatePodAnnotations", "testns", mock.Anything, mock.Anything).Return(func(_, name string, annotations map[string]string) error {
		p := pod(name)
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		maps.Copy(p.Annotations, annotations)
		return nil
	})
	mk.On("UpdatePodLabels", "testns", mock.Anything, mock.Anything).Return(func(_, name string, labels map[string]string) error {
		maps.Copy(pod(name).Labels, labels)
		return nil
	})
	mk.On("GetStatefulSet", "testns", "rfr-test").Return(func(string, string) (*appsv1.StatefulSet, error) {
		ss := &appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: it.updated}}
		if it.lagging {
			ss.Generation, ss.Status.ObservedGeneration = 2, 1
		}
		return ss, nil
	})
	it.healer = rfservice.NewRedisFailoverHealer(mk, nil, log.Dummy)
	return it
}

// setUpdateRevision makes the StatefulSet report rev as the update revision.
func (it *inPlaceTest) setUpdateRevision(rev string) {
	it.updated, it.lagging = rev, false
	it.check.ExpectedCalls = slices.DeleteFunc(it.check.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "GetStatefulSetUpdateRevision" })
	it.check.On("GetStatefulSetUpdateRevision", it.rf).Return(rev, nil)
}

// setLagging makes the StatefulSet status show the generation before the
// template change, with the update revision before the change.
func (it *inPlaceTest) setLagging() {
	it.lagging = true
	it.check.ExpectedCalls = slices.DeleteFunc(it.check.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "GetStatefulSetUpdateRevision" })
	it.check.On("GetStatefulSetUpdateRevision", it.rf).Return("", rfservice.ErrUpdateRevisionNotObserved)
}

// handler is the handler of an operator process that starts with the state
// of a step from 1Gi to 2Gi.
func (it *inPlaceTest) handler(state autoscale.State) *RedisFailoverHandler {
	h := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, it.check, it.healer, it.k8s, metrics.Dummy, log.Dummy)
	h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: state, spec: 1 << 30, effective: autoscaleEffective})
	return h
}

func stepTo2Gi() autoscale.State {
	return autoscale.State{
		Version: autoscale.StateVersion, Phase: autoscale.PhaseApplying, From: 1 << 30, To: autoscaleEffective, Effective: autoscaleEffective,
		SpecLimit: 1 << 30, StartedAt: time.Now().Unix(), LastSample: time.Now().Unix(),
	}
}

// An operator restart during a step continues it. The pod spec holds the
// request, so the restarted process sends no second resize.
func TestUpdateRedisesPodsResizesOnceAcrossARestart(t *testing.T) {
	it := newInPlaceTest(t)

	require.NoError(t, it.handler(stepTo2Gi()).UpdateRedisesPods(it.rf))
	it.k8s.AssertNumberOfCalls(t, "ResizePod", 1)
	require.True(t, rfservice.ResizeRequested(&it.pods[1]))

	restarted := it.handler(stepTo2Gi())
	require.NoError(t, restarted.UpdateRedisesPods(it.rf))
	it.k8s.AssertNumberOfCalls(t, "ResizePod", 1)

	// The kubelet applies the resize: the pod moves to the update revision.
	applied := memoryLimit("2Gi")
	it.pods[1].Status.ContainerStatuses[0].Resources = &applied
	require.NoError(t, restarted.UpdateRedisesPods(it.rf))
	assert.Equal(t, "new", it.pods[1].Labels[appsv1.ControllerRevisionHashLabelKey])
	assert.False(t, rfservice.ResizeRequested(&it.pods[1]))

	// The master is last, and also resized in place.
	require.NoError(t, restarted.UpdateRedisesPods(it.rf))
	it.k8s.AssertNumberOfCalls(t, "ResizePod", 2)
	it.k8s.AssertCalled(t, "ResizePod", "testns", "rfr-test-0", mock.Anything)
	it.k8s.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
}

// A revert after a refusal leaves the pod that holds the resize request on
// the reverted update revision. The rollout neither deletes it nor resizes it
// back.
func TestAutoscaleRevertKeepsAPatchedPod(t *testing.T) {
	it := newInPlaceTest(t)
	h := it.handler(stepTo2Gi())
	require.NoError(t, h.UpdateRedisesPods(it.rf))
	require.Equal(t, "2Gi", it.pods[1].Spec.Containers[0].Resources.Limits.Memory().String())

	it.pods[1].Status.Conditions = append(it.pods[1].Status.Conditions, corev1.PodCondition{
		Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: corev1.PodReasonInfeasible, Message: "Node didn't have enough capacity",
	})
	h.autoscaleRollout.Delete(failoverKey(it.rf))
	require.NoError(t, h.UpdateRedisesPods(it.rf))
	v, _ := h.autoscaleRollout.Load(failoverKey(it.rf))
	rollout := v.(autoscaleRollout)
	require.NotNil(t, rollout.refusal)
	assert.False(t, rollout.anyApplied)
	assert.Equal(t, []string{"rfr-test-1"}, rollout.requested)

	// The autoscaler reverts the step.
	stored := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rfa-test", ResourceVersion: "1"}}
	var written autoscale.State
	it.k8s.On("UpdateConfigMap", "testns", mock.Anything).Once().Run(func(args mock.Arguments) {
		require.NoError(t, json.Unmarshal([]byte(args.Get(1).(*corev1.ConfigMap).Data["state"]), &written))
	}).Return(nil)
	h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: stepTo2Gi(), cm: stored, spec: 1 << 30, effective: autoscaleEffective})
	it.rf.Status.State = redisfailoverv1.HealthyState
	h.autoscale(it.rf, redisfailoverv1.AutoscalingStatus{})
	require.Equal(t, autoscale.PhaseReverted, written.Phase)
	assert.Zero(t, written.Effective)
	assert.False(t, rfservice.ResizeRequested(&it.pods[1]), "the revert clears the request")

	// The next reconcile has the spec limit, and the update revision is the
	// revision of the pods again.
	it.rf.Spec.Redis.Resources = memoryLimit("1Gi")
	it.setUpdateRevision("old")
	h.autoscaleReads.Store(failoverKey(it.rf), autoscaleRead{state: written, spec: 1 << 30, effective: 1 << 30})
	require.NoError(t, h.UpdateRedisesPods(it.rf))
	require.NoError(t, h.UpdateRedisesPods(it.rf))

	it.k8s.AssertNumberOfCalls(t, "ResizePod", 1)
	it.k8s.AssertNotCalled(t, "DeletePod", mock.Anything, mock.Anything)
	it.k8s.AssertNotCalled(t, "DeletePodWithUID", mock.Anything, mock.Anything, mock.Anything)
	assert.Equal(t, "2Gi", it.pods[1].Spec.Containers[0].Resources.Limits.Memory().String(), "a pod larger than the template is safe")
}
