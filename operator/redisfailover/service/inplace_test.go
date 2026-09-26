package service

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/service/k8s"
)

const resizePod = "rfr-test-0"

func resources(cpu, memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
	}}
}

func podTemplate(redis corev1.ResourceRequirements) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: redisContainerName, Image: "redis:7", Resources: redis},
		{Name: "exporter", Image: "exporter"},
	}}}
}

func revision(name string, t corev1.PodTemplateSpec) *appsv1.ControllerRevision {
	raw, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"template": t}})
	return &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: runtime.RawExtension{Raw: raw}}
}

// stalePod is a pod created from the "old" revision, with spec resources
// requested and status resources applied as given.
func stalePod(requested, applied corev1.ResourceRequirements, conditions ...corev1.PodCondition) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: resizePod, Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "old"}}}
	requested, applied = withDefaultRequests(requested), withDefaultRequests(applied)
	pod.Spec.Containers = []corev1.Container{{Name: redisContainerName, Resources: requested}, {Name: "exporter"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: redisContainerName, Resources: &applied},
		{Name: "exporter", Resources: &corev1.ResourceRequirements{}},
	}
	pod.Status.Conditions = conditions
	return pod
}

func condition(t corev1.PodConditionType, reason string, age time.Duration) corev1.PodCondition {
	return corev1.PodCondition{Type: t, Status: corev1.ConditionTrue, Reason: reason, Message: "msg", LastTransitionTime: metav1.NewTime(time.Now().Add(-age))}
}

// podFrom is a pod created from the "old" revision template, with its
// resources applied.
func podFrom(t corev1.PodTemplateSpec) *corev1.Pod {
	pod := stalePod(corev1.ResourceRequirements{}, corev1.ResourceRequirements{})
	pod.Spec.Containers, pod.Status.ContainerStatuses = nil, nil
	for _, c := range t.Spec.Containers {
		r := withDefaultRequests(c.Resources)
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: c.Name, Resources: r})
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: c.Name, Resources: &r})
	}
	return pod
}

type resizeCase struct {
	support  k8s.PodResizeSupport
	old, new corev1.PodTemplateSpec
	pod      *corev1.Pod
}

func runResize(t *testing.T, c resizeCase, setup func(ms *mK8SService.Services)) (ResizeResult, *mK8SService.Services, error) {
	t.Helper()
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	ms := &mK8SService.Services{}
	ms.On("PodResizeSupport").Return(c.support, nil)
	ms.On("GetPod", "testns", resizePod).Return(c.pod, nil)
	ms.On("GetControllerRevision", "testns", "old").Return(revision("old", c.old), nil)
	ms.On("GetControllerRevision", "testns", "new").Return(revision("new", c.new), nil)
	ms.On("UpdatePodAnnotations", "testns", resizePod, mock.Anything).Maybe().Return(nil)
	if setup != nil {
		setup(ms)
	}
	result, err := NewRedisFailoverHealer(ms, nil, log.Dummy).ResizePodInPlace(rf, resizePod, "new")
	return result, ms, err
}

var fullSupport = k8s.PodResizeSupport{Supported: true, MemoryLimitDecrease: true}

func TestResizePodInPlaceRequestsTheResize(t *testing.T) {
	old, new := resources("500m", "1Gi"), resources("1", "2Gi")
	var got map[string]corev1.ResourceRequirements
	result, ms, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), stalePod(old, old)}, func(ms *mK8SService.Services) {
		ms.On("ResizePod", "testns", resizePod, mock.Anything).Once().Run(func(args mock.Arguments) {
			got = args.Get(2).(map[string]corev1.ResourceRequirements)
		}).Return(nil)
	})
	assert.NoError(t, err)
	assert.Equal(t, ResizeWaiting, result.Action)
	ms.AssertExpectations(t)
	// Requests are defaulted to the limits, as the API server does for pods.
	assert.Equal(t, withDefaultRequests(new), got[redisContainerName])
	assert.Contains(t, got, "exporter")
}

func TestResizePodInPlaceFollowsTheKubelet(t *testing.T) {
	old, new := resources("500m", "1Gi"), resources("1", "2Gi")
	tests := map[string]struct {
		pod    *corev1.Pod
		action ResizeAction
	}{
		"not applied yet":        {stalePod(new, old), ResizeWaiting},
		"infeasible":             {stalePod(new, old, condition(corev1.PodResizePending, corev1.PodReasonInfeasible, 0)), ResizeRecreate},
		"deferred":               {stalePod(new, old, condition(corev1.PodResizePending, corev1.PodReasonDeferred, time.Minute)), ResizeWaiting},
		"deferred for too long":  {stalePod(new, old, condition(corev1.PodResizePending, corev1.PodReasonDeferred, time.Hour)), ResizeRecreate},
		"in progress":            {stalePod(new, old, condition(corev1.PodResizeInProgress, "", time.Hour)), ResizeWaiting},
		"failing":                {stalePod(new, old, condition(corev1.PodResizeInProgress, corev1.PodReasonError, time.Minute)), ResizeWaiting},
		"failing for too long":   {stalePod(new, old, condition(corev1.PodResizeInProgress, corev1.PodReasonError, time.Hour)), ResizeRecreate},
		"applied":                {stalePod(new, new), ResizeDone},
		"pending after applying": {stalePod(new, new, condition(corev1.PodResizePending, corev1.PodReasonDeferred, 0)), ResizeWaiting},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result, ms, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), test.pod}, func(ms *mK8SService.Services) {
				if test.action == ResizeDone {
					ms.On("UpdatePodLabels", "testns", resizePod, map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}).Once().Return(nil)
				}
			})
			assert.NoError(t, err)
			assert.Equal(t, test.action, result.Action)
			ms.AssertExpectations(t)
		})
	}
}

// The kubelet's memory usage includes the page cache, so it can refuse a
// memory decrease that the recreated pod fits.
func TestResizePodInPlaceRecreatesForARefusedMemoryDecrease(t *testing.T) {
	old, new := resources("1", "2Gi"), resources("1", "1Gi")
	failing := func(age time.Duration) *corev1.Pod {
		c := condition(corev1.PodResizeInProgress, corev1.PodReasonError, age)
		c.Message = "cannot decrease memory limits: attempting to set container 'redis' memory limit (1073741824) below current usage (1181116006)"
		return stalePod(new, old, c)
	}
	result, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), failing(time.Minute)}, nil)
	assert.NoError(t, err)
	assert.Equal(t, ResizeWaiting, result.Action)

	result, _, err = runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), failing(time.Hour)}, nil)
	assert.NoError(t, err)
	assert.Equal(t, ResizeRecreate, result.Action)
}

// A resize that the kubelet neither applies nor reports on in time falls back
// to recreating the pod.
func TestResizePodInPlaceTimesOutWithoutCondition(t *testing.T) {
	old, new := resources("500m", "1Gi"), resources("1", "2Gi")
	requested := func(age time.Duration) *corev1.Pod {
		pod := stalePod(new, old)
		pod.Annotations = map[string]string{resizeRequestedAnnotation: time.Now().Add(-age).UTC().Format(time.RFC3339)}
		return pod
	}
	noAnnotation := func(ms *mK8SService.Services) {
		ms.ExpectedCalls = ms.ExpectedCalls[:len(ms.ExpectedCalls)-1]
	}

	result, ms, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), requested(time.Minute)}, noAnnotation)
	assert.NoError(t, err)
	assert.Equal(t, ResizeWaiting, result.Action)
	ms.AssertExpectations(t)

	result, _, err = runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), requested(time.Hour)}, noAnnotation)
	assert.NoError(t, err)
	assert.Equal(t, ResizeRecreate, result.Action)
	assert.Equal(t, "resize not applied by the kubelet", result.Message)

	// Without a recorded request, the timeout starts now.
	result, ms, err = runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), stalePod(new, old)}, nil)
	assert.NoError(t, err)
	assert.Equal(t, ResizeWaiting, result.Action)
	ms.AssertCalled(t, "UpdatePodAnnotations", "testns", resizePod, mock.Anything)
}

// A condition from an earlier resize request is not acted on.
func TestResizePodInPlaceIgnoresStaleConditions(t *testing.T) {
	old, new := resources("500m", "1Gi"), resources("1", "2Gi")
	stale := condition(corev1.PodResizePending, corev1.PodReasonInfeasible, time.Hour)
	stale.ObservedGeneration = 1
	pod := stalePod(new, old, stale)
	pod.Generation = 2
	result, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), pod}, nil)
	assert.NoError(t, err)
	assert.Equal(t, ResizeWaiting, result.Action)
}

// Values added by admission, e.g. a LimitRange default, are kept.
func TestResizePodInPlaceKeepsAdmittedResources(t *testing.T) {
	memory := func(size string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(size)}}
	}
	admitted := func(r corev1.ResourceRequirements) corev1.ResourceRequirements {
		r = withDefaultRequests(r)
		r.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
		return r
	}
	var got map[string]corev1.ResourceRequirements
	_, ms, err := runResize(t, resizeCase{fullSupport, podTemplate(memory("1Gi")), podTemplate(memory("2Gi")), stalePod(admitted(memory("1Gi")), admitted(memory("1Gi")))}, func(ms *mK8SService.Services) {
		ms.On("ResizePod", "testns", resizePod, mock.Anything).Once().Run(func(args mock.Arguments) {
			got = args.Get(2).(map[string]corev1.ResourceRequirements)
		}).Return(nil)
	})
	assert.NoError(t, err)
	ms.AssertExpectations(t)
	assert.Equal(t, admitted(memory("2Gi")), got[redisContainerName])

	// Once applied, the pod matches and moves to the update revision.
	result, _, err := runResize(t, resizeCase{fullSupport, podTemplate(memory("1Gi")), podTemplate(memory("2Gi")), stalePod(admitted(memory("2Gi")), admitted(memory("2Gi")))}, func(ms *mK8SService.Services) {
		ms.On("UpdatePodLabels", "testns", resizePod, mock.Anything).Once().Return(nil)
	})
	assert.NoError(t, err)
	assert.Equal(t, ResizeDone, result.Action)
}

func TestResizePodInPlaceRecreates(t *testing.T) {
	cpu := func(r corev1.ResourceRequirements) corev1.ResourceRequirements {
		r.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
		delete(r.Limits, corev1.ResourceCPU)
		return r
	}
	withStorage := func(r corev1.ResourceRequirements, size string) corev1.ResourceRequirements {
		r.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse(size)
		return r
	}
	other := podTemplate(resources("1", "2Gi"))
	other.Spec.Containers[0].Image = "redis:8"
	claimed := podTemplate(resources("1", "1Gi"))
	claimed.Spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "gpu"}}
	tests := map[string]struct {
		support  k8s.PodResizeSupport
		old, new corev1.PodTemplateSpec
		reason   string
	}{
		"unsupported cluster":         {k8s.PodResizeSupport{}, podTemplate(resources("1", "1Gi")), podTemplate(resources("2", "1Gi")), ""},
		"more than resources change":  {fullSupport, podTemplate(resources("1", "1Gi")), other, "the update changes more than container resources"},
		"resource claims change":      {fullSupport, podTemplate(resources("1", "1Gi")), claimed, "the update changes more than container resources"},
		"limit replaced":              {fullSupport, podTemplate(resources("1", "1Gi")), podTemplate(corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}}), "requests or limits are added or removed"},
		"limit removed":               {fullSupport, podTemplate(resources("1", "1Gi")), podTemplate(cpu(resources("1", "1Gi"))), "requests or limits are added or removed"},
		"other resources change":      {fullSupport, podTemplate(withStorage(resources("1", "1Gi"), "1Gi")), podTemplate(withStorage(resources("1", "1Gi"), "2Gi")), "only cpu and memory can be resized in place"},
		"memory decrease before 1.35": {k8s.PodResizeSupport{Supported: true}, podTemplate(resources("1", "2Gi")), podTemplate(resources("1", "1Gi")), "lowering a memory limit in place needs Kubernetes 1.35"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result, _, err := runResize(t, resizeCase{test.support, test.old, test.new, podFrom(test.old)}, nil)
			assert.NoError(t, err)
			assert.Equal(t, ResizeRecreate, result.Action)
			assert.Equal(t, test.reason, result.Message)
		})
	}
}

// A kubelet without in-place resize support reports no container resources.
func TestResizePodInPlaceRecreatesOnAKubeletWithoutSupport(t *testing.T) {
	old := podTemplate(resources("1", "1Gi"))
	pod := podFrom(old)
	pod.Status.ContainerStatuses = nil
	result, _, err := runResize(t, resizeCase{fullSupport, old, podTemplate(resources("1", "2Gi")), pod}, nil)
	assert.NoError(t, err)
	assert.Equal(t, ResizeRecreate, result.Action)
	assert.Equal(t, "the kubelet does not report the container's resources", result.Message)
}

func TestResizePodInPlaceRecreatesWithoutRevisionsOrSupport(t *testing.T) {
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	old := resources("1", "1Gi")
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "controllerrevisions"}, "old")

	t.Run("disabled", func(t *testing.T) {
		rf := rf.DeepCopy()
		rf.Spec.Redis.InPlaceResize = "Disabled"
		result, err := NewRedisFailoverHealer(&mK8SService.Services{}, nil, log.Dummy).ResizePodInPlace(rf, resizePod, "new")
		assert.NoError(t, err)
		assert.Equal(t, ResizeRecreate, result.Action)
	})

	t.Run("revisions forbidden", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("PodResizeSupport").Return(fullSupport, nil)
		ms.On("GetPod", "testns", resizePod).Return(stalePod(old, old), nil)
		ms.On("GetControllerRevision", "testns", "old").Return(nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "controllerrevisions"}, "old", errors.New("rbac")))
		result, err := NewRedisFailoverHealer(ms, nil, log.Dummy).ResizePodInPlace(rf, resizePod, "new")
		assert.NoError(t, err)
		assert.Equal(t, ResizeRecreate, result.Action)
	})

	for _, missing := range []string{"old", "new"} {
		t.Run(missing+" revision gone", func(t *testing.T) {
			ms := &mK8SService.Services{}
			ms.On("PodResizeSupport").Return(fullSupport, nil)
			ms.On("GetPod", "testns", resizePod).Return(stalePod(old, old), nil)
			for _, rev := range []string{"old", "new"} {
				if rev == missing {
					ms.On("GetControllerRevision", "testns", rev).Return(nil, notFound)
				} else {
					ms.On("GetControllerRevision", "testns", rev).Return(revision(rev, podTemplate(old)), nil)
				}
			}
			result, err := NewRedisFailoverHealer(ms, nil, log.Dummy).ResizePodInPlace(rf, resizePod, "new")
			assert.NoError(t, err)
			assert.Equal(t, ResizeRecreate, result.Action)
		})
	}
}

func TestResizePodInPlaceErrors(t *testing.T) {
	old, new := resources("500m", "1Gi"), resources("1", "2Gi")
	fail := errors.New("boom")

	t.Run("resize rejected", func(t *testing.T) {
		forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/resize"}, resizePod, fail)
		result, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), stalePod(old, old)}, func(ms *mK8SService.Services) {
			ms.On("ResizePod", "testns", resizePod, mock.Anything).Return(forbidden)
		})
		assert.NoError(t, err)
		assert.Equal(t, ResizeRecreate, result.Action)
	})

	tests := map[string]func(ms *mK8SService.Services){
		// Retried, as recreating the master would fail over.
		"detecting support": func(ms *mK8SService.Services) {
			ms.ExpectedCalls = nil
			ms.On("PodResizeSupport").Return(k8s.PodResizeSupport{}, fail)
		},
		"resizing": func(ms *mK8SService.Services) {
			ms.On("ResizePod", "testns", resizePod, mock.Anything).Return(fail)
		},
		"reading the pod": func(ms *mK8SService.Services) {
			ms.ExpectedCalls = nil
			ms.On("PodResizeSupport").Return(fullSupport, nil)
			ms.On("GetPod", "testns", resizePod).Return(nil, fail)
		},
		"reading a revision": func(ms *mK8SService.Services) {
			ms.ExpectedCalls = nil
			ms.On("PodResizeSupport").Return(fullSupport, nil)
			ms.On("GetPod", "testns", resizePod).Return(stalePod(old, old), nil)
			ms.On("GetControllerRevision", "testns", "old").Return(nil, fail)
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), stalePod(old, old)}, setup)
			assert.ErrorIs(t, err, fail)
		})
	}

	t.Run("corrupt revision", func(t *testing.T) {
		_, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), stalePod(old, old)}, func(ms *mK8SService.Services) {
			ms.ExpectedCalls = nil
			ms.On("PodResizeSupport").Return(fullSupport, nil)
			ms.On("GetPod", "testns", resizePod).Return(stalePod(old, old), nil)
			ms.On("GetControllerRevision", "testns", "old").Return(&appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: []byte("{")}}, nil)
		})
		assert.Error(t, err)
	})

	for name, pod := range map[string]*corev1.Pod{"requesting the resize": stalePod(old, old), "recording the request": stalePod(new, old)} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), pod}, func(ms *mK8SService.Services) {
				ms.ExpectedCalls = ms.ExpectedCalls[:len(ms.ExpectedCalls)-1]
				ms.On("UpdatePodAnnotations", "testns", resizePod, mock.Anything).Return(fail)
			})
			assert.ErrorIs(t, err, fail)
		})
	}

	t.Run("relabelling", func(t *testing.T) {
		_, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), stalePod(new, new)}, func(ms *mK8SService.Services) {
			ms.On("UpdatePodLabels", "testns", resizePod, mock.Anything).Return(fail)
		})
		assert.ErrorIs(t, err, fail)
	})
}

func TestOverlay(t *testing.T) {
	list := func(kv ...string) corev1.ResourceList {
		l := corev1.ResourceList{}
		for i := 0; i < len(kv); i += 2 {
			l[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
		}
		return l
	}
	assert.Equal(t, list("memory", "2Gi", "cpu", "1"), overlay(list("memory", "1Gi", "cpu", "1"), list("memory", "2Gi")))
	assert.Equal(t, list("memory", "2Gi", "ephemeral-storage", "1Gi"), overlay(list("ephemeral-storage", "1Gi"), list("memory", "2Gi", "ephemeral-storage", "2Gi")))
	assert.Equal(t, list("cpu", "1"), overlay(nil, list("cpu", "1")))
	assert.Nil(t, overlay(nil, nil))
}

// A pod still running an earlier resize that a newer revision superseded is
// resized to the newer revision's values, not only the ones it changes.
func TestResizePodInPlaceSupersededResize(t *testing.T) {
	a, b, c := resources("1", "1Gi"), resources("2", "1Gi"), resources("1", "2Gi")
	var got map[string]corev1.ResourceRequirements
	_, ms, err := runResize(t, resizeCase{fullSupport, podTemplate(a), podTemplate(c), stalePod(b, b)}, func(ms *mK8SService.Services) {
		ms.On("ResizePod", "testns", resizePod, mock.Anything).Once().Run(func(args mock.Arguments) {
			got = args.Get(2).(map[string]corev1.ResourceRequirements)
		}).Return(nil)
	})
	assert.NoError(t, err)
	ms.AssertExpectations(t)
	assert.Equal(t, withDefaultRequests(c), got[redisContainerName])

	// Reverting a memory increase lowers the pod's limit, although the revisions have the same.
	result, _, err := runResize(t, resizeCase{k8s.PodResizeSupport{Supported: true}, podTemplate(a), podTemplate(a), stalePod(c, c)}, nil)
	assert.NoError(t, err)
	assert.Equal(t, ResizeRecreate, result.Action)
	assert.Equal(t, "lowering a memory limit in place needs Kubernetes 1.35", result.Message)
}

// A condition keeps its transition time when a newer resize supersedes the
// one it reported on, so the timeout also counts from the latest request.
func TestResizePodInPlaceTimesOutFromTheLatestRequest(t *testing.T) {
	old, new := resources("500m", "1Gi"), resources("1", "2Gi")
	for name, c := range map[string]corev1.PodCondition{
		"deferred": condition(corev1.PodResizePending, corev1.PodReasonDeferred, time.Hour),
		"failing":  condition(corev1.PodResizeInProgress, corev1.PodReasonError, time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			pod := stalePod(new, old, c)
			pod.Annotations = map[string]string{resizeRequestedAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
			result, _, err := runResize(t, resizeCase{fullSupport, podTemplate(old), podTemplate(new), pod}, nil)
			assert.NoError(t, err)
			assert.Equal(t, ResizeWaiting, result.Action)
		})
	}
}
