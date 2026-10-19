package redisfailover_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfOperator "github.com/saremox/redis-operator/operator/redisfailover"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

// TestCheckAndHealAutoscaling runs a healthy reconcile in both modes. Without
// autoscaling, the reconcile must make no autoscaler call: the strict mocks
// panic on a call without an expectation. With Recommend, the autoscaler
// samples the master that the checks found. With Auto and a recommendation,
// a settled instance starts a step in both modes.
func TestCheckAndHealAutoscaling(t *testing.T) {
	const (
		master   = "10.0.0.1"
		sentinel = "1.1.1.1"
	)
	healthy := map[string]func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover){
		"sentinel": func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
			mrfc.On("IsRedisRunning", rf).Return(true)
			mrfc.On("IsSentinelRunningQuorum", rf).Return(true)
			mrfc.On("GetNumberMasters", rf).Return(1, nil)
			mrfc.On("GetMasterIP", rf).Return(master, nil)
			mrfc.On("CheckAllSlavesFromMaster", master, rf).Return(nil)
			mrfc.On("GetRedisesIPs", rf).Return([]string{master}, nil)
			mrfh.On("SetRedisCustomConfig", master, rf).Return(nil)
			mrfc.On("GetStatefulSetUpdateRevision", rf).Return("1", nil)
			mrfc.On("GetRedisesSlavesPods", rf).Return([]string{}, nil)
			mrfc.On("GetRedisesMasterPod", rf).Return(master, nil)
			mrfc.On("GetRedisRevisionHash", master, rf).Return("1", nil)
			mrfc.On("CheckSentinelMonitor", sentinel, master, "0").Return(nil)
			mrfc.On("GetSentinelsIPs", rf).Return([]string{sentinel}, nil)
			mrfc.On("CheckSentinelNumberInMemory", sentinel, rf).Return(nil)
			mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, rf).Return(nil)
			mrfh.On("SetSentinelCustomConfig", sentinel, rf).Return(nil)
		},
		"operator-managed": func(mrfc *mRFService.RedisFailoverCheck, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover) {
			rf.Spec.Sentinel.Enabled = new(false)
			mrfc.On("IsRedisRunning", rf).Return(true)
			mrfc.On("GetNumberMasters", rf).Return(1, nil)
			mrfc.On("CheckMasterHealth", rf).Return(true, master, nil)
			mrfc.On("CheckAllSlavesFromMaster", master, rf).Return(nil)
			mrfc.On("GetRedisesIPs", rf).Return([]string{master}, nil)
			mrfh.On("SetRedisCustomConfig", master, rf).Return(nil)
			mrfc.On("GetMasterIP", rf).Return(master, nil)
			mrfc.On("GetStatefulSetUpdateRevision", rf).Return("1", nil)
			mrfc.On("GetRedisesSlavesPods", rf).Return([]string{}, nil)
			mrfc.On("GetRedisesMasterPod", rf).Return(master, nil)
			mrfc.On("GetRedisRevisionHash", master, rf).Return("1", nil)
		},
	}

	for mode, setup := range healthy {
		for _, autoscaling := range []string{"unset", v1.AutoscalingModeOff, v1.AutoscalingModeRecommend, v1.AutoscalingModeAuto} {
			t.Run(mode+", "+autoscaling, func(t *testing.T) {
				rf := generateRF(false, false)
				rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
				if autoscaling != "unset" {
					memoryMax := resource.MustParse("2Gi")
					rf.Spec.Redis.Autoscaling = &v1.AutoscalingSettings{Mode: autoscaling, MemoryMax: &memoryMax, Workload: v1.AutoscalingWorkloadAuto}
				}
				mk := settledK8sServices()
				var patched v1.RedisFailoverStatus
				mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
					patched = args.Get(2).(*v1.RedisFailover).Status
				}).Return()
				mrfc := noUnknownSentinelAddrs(&mRFService.RedisFailoverCheck{})
				mrfc.On("GetSentinelMasterIPs", mock.Anything).Maybe().Return(nil, nil)
				mrfh := &mRFService.RedisFailoverHeal{}
				mrfh.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
				mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
				mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
				setup(mrfc, mrfh, rf)
				var step map[string]any
				if autoscaling == v1.AutoscalingModeAuto {
					autoscaleStep(t, mk, mrfh, rf, master, &step)
				}
				if autoscaling == v1.AutoscalingModeRecommend {
					// The read of the live limit: no StatefulSet and no pods.
					mk.On("GetStatefulSet", rf.Namespace, "rfr-"+rf.Name).Once().Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "statefulsets"}, "rfr-"+rf.Name))
					mk.On("ListPods", rf.Namespace).Once().Return(&corev1.PodList{}, nil)
					mk.On("GetConfigMap", rf.Namespace, "rfa-"+rf.Name).Once().Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "rfa-"+rf.Name))
					mk.On("CreateConfigMap", rf.Namespace, mock.Anything).Once().Return(nil)
					mrfc.On("GetAutoscalerInfo", rf, master).Once().Return(&redis.AutoscalerInfo{
						UsedMemory: 100 << 20, MaxMemory: 768 << 20, UptimeSeconds: 3600, RunID: "run1", Role: "master",
					}, nil)
				}
				handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)

				require.NoError(t, handler.CheckAndHeal(rf))

				assert.Equal(t, v1.HealthyState, patched.State)
				switch autoscaling {
				case v1.AutoscalingModeAuto:
					assert.Equal(t, "Applying", patched.Autoscaling.Phase)
					assert.Equal(t, "Applying", step["phase"])
					assert.InDelta(t, float64(1280<<20), step["effective"], 0)
				case v1.AutoscalingModeRecommend:
					assert.Equal(t, v1.AutoscalingStatus{Workload: v1.AutoscalingWorkloadStore, Reason: "memory use is below the threshold"}, patched.Autoscaling)
				default:
					assert.Zero(t, patched.Autoscaling)
					mk.AssertNotCalled(t, "GetConfigMap", mock.Anything, mock.Anything)
				}
				mk.AssertExpectations(t)
				mrfc.AssertExpectations(t)
			})
		}
	}
}

// TestHandleAutoscalingAPICalls runs the first full healthy reconcile after an
// operator start. Recommend reads the state before Ensure and writes it. For
// the missing state, it reads the StatefulSet one time. Without autoscaling,
// the reconcile looks for a leftover state, and makes no INFO call. Validate
// accepts mode Auto, which also reads the StatefulSet for the rollout result.
func TestHandleAutoscalingAPICalls(t *testing.T) {
	const master = "10.0.0.1"
	for _, autoscaling := range []string{"unset", v1.AutoscalingModeOff, v1.AutoscalingModeRecommend, v1.AutoscalingModeAuto} {
		t.Run(autoscaling, func(t *testing.T) {
			rf := generateRF(false, false)
			rf.Finalizers = []string{"redisfailovers.databases.spotahome.com/finalizer"}
			rf.Spec.Sentinel.Enabled = new(false)
			rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
			if autoscaling != "unset" {
				memoryMax := resource.MustParse("2Gi")
				rf.Spec.Redis.Autoscaling = &v1.AutoscalingSettings{Mode: autoscaling, MemoryMax: &memoryMax}
			}
			if rf.AutoscalingEnabled() {
				// effectiveRF adds the annotation of the spec limit, so the
				// mocks match the copy for Ensure and the checks.
				rf.Annotations = map[string]string{rfservice.SpecMemoryLimitAnnotation: "1Gi"}
			}
			var calls []string
			record := func(name string) func(mock.Arguments) {
				return func(mock.Arguments) { calls = append(calls, name) }
			}
			mk := settledK8sServices()
			mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
			mrfs := &mRFService.RedisFailoverClient{}
			mrfs.On("EnsureNotPresentRedisService", rf).Once().Run(record("Ensure")).Return(nil)
			mrfs.On("EnsureNotPresentSentinelResources", rf).Once().Return(nil)
			for _, ensure := range []string{"EnsureRedisMasterService", "EnsureRedisSlaveService", "EnsureRedisShutdownConfigMap", "EnsureRedisReadinessConfigMap", "EnsureRedisConfigMap", "EnsureRedisStatefulset"} {
				mrfs.On(ensure, rf, mock.Anything, mock.Anything).Once().Return(nil)
			}
			mrfc := noUnknownSentinelAddrs(&mRFService.RedisFailoverCheck{})
			mrfc.On("GetSentinelMasterIPs", mock.Anything).Maybe().Return(nil, nil)
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
			mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
			mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
			mrfc.On("IsRedisRunning", rf).Return(true)
			mrfc.On("GetNumberMasters", rf).Return(1, nil)
			mrfc.On("CheckMasterHealth", rf).Return(true, master, nil)
			mrfc.On("CheckAllSlavesFromMaster", master, rf).Return(nil)
			mrfc.On("GetRedisesIPs", rf).Return([]string{master}, nil)
			mrfh.On("SetRedisCustomConfig", master, rf).Return(nil)
			mrfc.On("GetMasterIP", rf).Return(master, nil)
			mrfc.On("GetStatefulSetUpdateRevision", rf).Return("1", nil)
			mrfc.On("GetRedisesSlavesPods", rf).Return([]string{}, nil)
			mrfc.On("GetRedisesMasterPod", rf).Return(master, nil)
			mrfc.On("GetRedisRevisionHash", master, rf).Return("1", nil)
			mk.On("GetConfigMap", rf.Namespace, "rfa-"+rf.Name).Once().Run(record("GetConfigMap")).Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "rfa-"+rf.Name))
			want := map[string][]string{
				"unset":                     {"GetConfigMap", "Ensure"},
				v1.AutoscalingModeOff:       {"GetConfigMap", "Ensure"},
				v1.AutoscalingModeRecommend: {"GetConfigMap", "GetStatefulSet", "ListPods", "Ensure", "CreateConfigMap"},
				v1.AutoscalingModeAuto:      {"GetConfigMap", "GetStatefulSet", "Ensure", "GetStatefulSet", "CreateConfigMap"},
			}[autoscaling]
			if rf.AutoscalingEnabled() {
				mk.On("CreateConfigMap", rf.Namespace, mock.Anything).Once().Run(record("CreateConfigMap")).Return(nil)
				mrfc.On("GetAutoscalerInfo", rf, master).Once().Return(&redis.AutoscalerInfo{
					UsedMemory: 100 << 20, MaxMemory: 768 << 20, UptimeSeconds: 3600, RunID: "run1", Role: "master",
				}, nil)
			}
			if autoscaling == v1.AutoscalingModeRecommend {
				// Each reconcile reads the live limit. Without the StatefulSet,
				// the pods give it.
				mk.On("GetStatefulSet", rf.Namespace, "rfr-"+rf.Name).Once().Run(record("GetStatefulSet")).Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "statefulsets"}, "rfr-"+rf.Name))
				mk.On("ListPods", rf.Namespace).Once().Run(record("ListPods")).Return(&corev1.PodList{}, nil)
			}
			if autoscaling == v1.AutoscalingModeAuto {
				rf.Spec.Redis.MaxMemory = &v1.MaxMemorySettings{}
				mrfh.On("EnsureRedisMaxMemory", rf, master, []string{master}).Return(rfservice.MaxMemoryResult{}, nil)
				mk.On("GetStatefulSet", rf.Namespace, "rfr-"+rf.Name).Twice().Run(record("GetStatefulSet")).Return(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: "1"}}, nil)
			}
			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)

			require.NoError(t, handler.Handle(context.Background(), rf))

			assert.Equal(t, want, calls)
			if !rf.AutoscalingEnabled() {
				mk.AssertNotCalled(t, "GetStatefulSet", mock.Anything, mock.Anything)
			}
			mk.AssertExpectations(t)
			mrfs.AssertExpectations(t)
			mrfc.AssertExpectations(t)
		})
	}
}

// autoscaleStep gives a CheckAndHeal of mode Auto a state with a sustained
// recommendation of 1280Mi. Three pods are ready on the update revision "1",
// and the checks before a step pass. The written state goes to step.
func autoscaleStep(t *testing.T, mk *mK8SService.Services, mrfh *mRFService.RedisFailoverHeal, rf *v1.RedisFailover, master string, step *map[string]any) {
	t.Helper()
	rf.Spec.Redis.MaxMemory = &v1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	mrfh.On("EnsureRedisMaxMemory", rf, mock.Anything, []string{master}).Return(rfservice.MaxMemoryResult{}, nil)
	state, err := json.Marshal(map[string]any{
		"version": 2, "workload": "Store", "runID": "run1", "limit": 1 << 30, "lastSample": time.Now().Unix(),
		"reason": "memory use is high in 3 consecutive windows", "recommended": 1280 << 20, "highWindows": 3,
	})
	require.NoError(t, err)
	owner := metav1.NewControllerRef(rf, v1.VersionKind(v1.RFKind))
	mk.On("GetConfigMap", rf.Namespace, "rfa-"+rf.Name).Once().Return(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "rfa-" + rf.Name, ResourceVersion: "7", OwnerReferences: []metav1.OwnerReference{*owner}},
		Data:       map[string]string{"state": string(state)},
	}, nil)
	pods := make([]corev1.Pod, rf.Spec.Redis.Replicas)
	for i := range pods {
		limit := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		pods[i] = corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "1"}},
			Status: corev1.PodStatus{
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "redis", Resources: &limit}},
			},
		}
	}
	mk.ExpectedCalls = slices.DeleteFunc(mk.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "GetStatefulSetPods" })
	mk.On("GetStatefulSetPods", rf.Namespace, "rfr-"+rf.Name).Return(&corev1.PodList{Items: pods}, nil)
	mk.On("GetStatefulSet", rf.Namespace, "rfr-"+rf.Name).Return(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdateRevision: "1"},
	}, nil)
	mk.On("PodResizeSupport").Once().Return(k8s.PodResizeSupport{Supported: true}, nil)
	mk.On("GetControllerRevision", rf.Namespace, "1").Once().Return(&appsv1.ControllerRevision{}, nil)
	mk.On("UpdateConfigMap", rf.Namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		cm := args.Get(1).(*corev1.ConfigMap)
		assert.Equal(t, "7", cm.ResourceVersion)
		require.NoError(t, json.Unmarshal([]byte(cm.Data["state"]), step))
	}).Return(nil)
}
