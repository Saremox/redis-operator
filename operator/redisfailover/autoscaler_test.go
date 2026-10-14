package redisfailover_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	v1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	rfOperator "github.com/saremox/redis-operator/operator/redisfailover"
	"github.com/saremox/redis-operator/service/redis"
)

// TestCheckAndHealAutoscaling runs a healthy reconcile in both modes. Without
// autoscaling, the reconcile must make no autoscaler call: the strict mocks
// panic on a call without an expectation. With Recommend, the autoscaler
// samples the master that the checks found.
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
			rf.Spec.Sentinel.Enabled = ptr.To(false)
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
		for _, autoscaling := range []string{"unset", v1.AutoscalingModeOff, v1.AutoscalingModeRecommend} {
			t.Run(mode+", "+autoscaling, func(t *testing.T) {
				rf := generateRF(false, false)
				rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
				if autoscaling != "unset" {
					max := resource.MustParse("2Gi")
					rf.Spec.Redis.Autoscaling = &v1.AutoscalingSettings{Mode: autoscaling, MemoryMax: &max, Workload: v1.AutoscalingWorkloadAuto}
				}
				mk := settledK8sServices()
				var patched v1.RedisFailoverStatus
				mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
					patched = args.Get(2).(*v1.RedisFailover).Status
				}).Return()
				mrfc := &mRFService.RedisFailoverCheck{}
				mrfh := &mRFService.RedisFailoverHeal{}
				mrfh.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
				mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
				mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
				setup(mrfc, mrfh, rf)
				if autoscaling == v1.AutoscalingModeRecommend {
					mk.On("GetConfigMap", rf.Namespace, "rfa-"+rf.Name).Once().Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "rfa-"+rf.Name))
					mk.On("CreateConfigMap", rf.Namespace, mock.Anything).Once().Return(nil)
					mrfc.On("GetAutoscalerInfo", rf, master).Once().Return(&redis.AutoscalerInfo{
						UsedMemory: 100 << 20, MaxMemory: 768 << 20, UptimeSeconds: 3600, RunID: "run1", Role: "master",
					}, nil)
				}
				handler := rfOperator.NewRedisFailoverHandler(generateConfig(), &mRFService.RedisFailoverClient{}, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)

				assert.NoError(t, handler.CheckAndHeal(rf))

				assert.Equal(t, v1.HealthyState, patched.State)
				if autoscaling == v1.AutoscalingModeRecommend {
					assert.Equal(t, v1.AutoscalingStatus{Workload: v1.AutoscalingWorkloadStore, Reason: "memory use is below the threshold"}, patched.Autoscaling)
				} else {
					assert.Zero(t, patched.Autoscaling)
					mk.AssertNotCalled(t, "GetConfigMap", mock.Anything, mock.Anything)
				}
				mk.AssertExpectations(t)
				mrfc.AssertExpectations(t)
			})
		}
	}
}

// TestHandleAutoscalingAPICalls runs a full healthy reconcile. Recommend makes
// the API calls of the autoscaler without the seam: one state read, before
// Ensure, and the state write. It reads no StatefulSet.
func TestHandleAutoscalingAPICalls(t *testing.T) {
	const master = "10.0.0.1"
	for _, autoscaling := range []string{"unset", v1.AutoscalingModeOff, v1.AutoscalingModeRecommend} {
		t.Run(autoscaling, func(t *testing.T) {
			rf := generateRF(false, false)
			rf.Finalizers = []string{"redisfailovers.databases.spotahome.com/finalizer"}
			rf.Spec.Sentinel.Enabled = ptr.To(false)
			rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
			if autoscaling != "unset" {
				max := resource.MustParse("2Gi")
				rf.Spec.Redis.Autoscaling = &v1.AutoscalingSettings{Mode: autoscaling, MemoryMax: &max}
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
			mrfc := &mRFService.RedisFailoverCheck{}
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
			want := []string{"Ensure"}
			if autoscaling == v1.AutoscalingModeRecommend {
				want = []string{"GetConfigMap", "Ensure", "CreateConfigMap"}
				mk.On("GetConfigMap", rf.Namespace, "rfa-"+rf.Name).Once().Run(record("GetConfigMap")).Return(nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "rfa-"+rf.Name))
				mk.On("CreateConfigMap", rf.Namespace, mock.Anything).Once().Run(record("CreateConfigMap")).Return(nil)
				mrfc.On("GetAutoscalerInfo", rf, master).Once().Return(&redis.AutoscalerInfo{
					UsedMemory: 100 << 20, MaxMemory: 768 << 20, UptimeSeconds: 3600, RunID: "run1", Role: "master",
				}, nil)
			}
			handler := rfOperator.NewRedisFailoverHandler(generateConfig(), mrfs, mrfc, mrfh, mk, metrics.Dummy, log.Dummy)

			assert.NoError(t, handler.Handle(context.Background(), rf))

			assert.Equal(t, want, calls)
			mk.AssertNotCalled(t, "GetStatefulSet", mock.Anything, mock.Anything)
			mk.AssertExpectations(t)
			mrfs.AssertExpectations(t)
			mrfc.AssertExpectations(t)
		})
	}
}
