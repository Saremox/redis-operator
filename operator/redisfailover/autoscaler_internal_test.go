package redisfailover

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	mMetrics "github.com/saremox/redis-operator/mocks/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	"github.com/saremox/redis-operator/service/redis"
)

const (
	autoscaleMaster = "10.0.0.1"
	autoscaleCM     = "rfa-test"
	mib             = int64(1 << 20)
)

var autoscaleT0 = time.Unix(1_800_000_000, 0)

type autoscaleTest struct {
	h      *RedisFailoverHandler
	rf     *redisfailoverv1.RedisFailover
	mk     *mK8SService.Services
	mrfc   *mRFService.RedisFailoverCheck
	mrec   *mMetrics.Recorder
	logger *warnLogger
	now    time.Time
}

func newAutoscaleTest(t *testing.T, mode string) *autoscaleTest {
	t.Helper()
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns", UID: "rf-uid"}}
	rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	if mode != "" {
		max := resource.MustParse("2Gi")
		rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: mode, MemoryMax: &max}
	}
	require.NoError(t, rf.Validate())
	rf.Status.State = redisfailoverv1.HealthyState
	at := &autoscaleTest{
		rf:     rf,
		mk:     &mK8SService.Services{},
		mrfc:   &mRFService.RedisFailoverCheck{},
		mrec:   &mMetrics.Recorder{},
		logger: &warnLogger{},
		now:    autoscaleT0,
	}
	at.h = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, at.mrfc, &mRFService.RedisFailoverHeal{}, at.mk, at.mrec, at.logger)
	at.h.now = func() time.Time { return at.now }
	return at
}

// run runs the autoscaler as the end of CheckAndHeal does.
func (at *autoscaleTest) run(observed redisfailoverv1.AutoscalingStatus) {
	at.h.autoscaleMasters.Store(failoverKey(at.rf), autoscaleMaster)
	at.rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{}
	at.h.autoscale(at.rf, observed)
}

func (at *autoscaleTest) assertNoCalls(t *testing.T) {
	t.Helper()
	at.mk.AssertExpectations(t)
	at.mrfc.AssertExpectations(t)
	at.mrec.AssertExpectations(t)
	assert.Empty(t, at.mk.Calls)
	assert.Empty(t, at.mrfc.Calls)
	assert.Empty(t, at.mrec.Calls)
}

func masterInfo(usedMiB int64) *redis.AutoscalerInfo {
	return &redis.AutoscalerInfo{UsedMemory: usedMiB * mib, MaxMemory: 1000 * mib, UptimeSeconds: 3600, RunID: "run1", Role: "master"}
}

func notFound() error {
	return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, autoscaleCM)
}

func decodeState(t *testing.T, cm *corev1.ConfigMap) autoscale.State {
	t.Helper()
	var s autoscale.State
	require.NoError(t, json.Unmarshal([]byte(cm.Data["state"]), &s))
	return s
}

// fakeStateStore keeps the state ConfigMap of an autoscaleTest.
type fakeStateStore struct {
	stored           *corev1.ConfigMap
	creates, updates int
}

func (at *autoscaleTest) fakeStateStore(t *testing.T) *fakeStateStore {
	f := &fakeStateStore{}
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(func(string, string) (*corev1.ConfigMap, error) {
		if f.stored == nil {
			return nil, notFound()
		}
		return f.stored.DeepCopy(), nil
	})
	at.mk.On("CreateConfigMap", "testns", mock.Anything).Run(func(args mock.Arguments) {
		f.creates++
		f.stored = args.Get(1).(*corev1.ConfigMap).DeepCopy()
		f.stored.ResourceVersion = "1"
	}).Return(nil)
	at.mk.On("UpdateConfigMap", "testns", mock.Anything).Run(func(args mock.Arguments) {
		f.updates++
		cm := args.Get(1).(*corev1.ConfigMap)
		assert.Equal(t, f.stored.ResourceVersion, cm.ResourceVersion)
		f.stored = cm.DeepCopy()
		f.stored.ResourceVersion += "1"
	}).Return(nil)
	return f
}

func TestAutoscaleWritesTheStateOnlyOnAChange(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	f := at.fakeStateStore(t)
	info := masterInfo(990)
	at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(func(*redisfailoverv1.RedisFailover, string) (*redis.AutoscalerInfo, error) {
		return info, nil
	})
	at.mrec.On("SetAutoscaler", "testns", "test", 1280*mib, 0.99, int32(0)).Times(4)

	at.run(redisfailoverv1.AutoscalingStatus{})
	require.Equal(t, 1, f.creates)
	assert.Equal(t, autoscaleCM, f.stored.Name)
	assert.Equal(t, "autoscaler", f.stored.Labels["app.kubernetes.io/component"])
	assert.True(t, metav1.IsControlledBy(f.stored, at.rf))
	assert.Equal(t, autoscaleT0.Unix(), decodeState(t, f.stored).LastSample)
	want := redisfailoverv1.AutoscalingStatus{
		Workload:          redisfailoverv1.AutoscalingWorkloadStore,
		RecommendedMemory: "1280Mi",
		Reason:            autoscale.ReasonUrgent,
		EvaluatedAt:       autoscaleT0.UTC().Format(time.RFC3339),
	}
	assert.Equal(t, want, at.rf.Status.Autoscaling)

	// Within the sample interval: no INFO and no write.
	at.now = autoscaleT0.Add(30 * time.Second)
	at.run(want)
	at.mrfc.AssertNumberOfCalls(t, "GetAutoscalerInfo", 1)
	assert.Equal(t, want, at.rf.Status.Autoscaling)

	// A sample that changes only the sample time writes nothing.
	at.now = autoscaleT0.Add(time.Minute)
	at.run(want)
	at.mrfc.AssertNumberOfCalls(t, "GetAutoscalerInfo", 2)
	assert.Equal(t, 0, f.updates)
	assert.Equal(t, want, at.rf.Status.Autoscaling)

	// The persisted sample time is older than a minute, but the time in
	// memory is not: no INFO.
	at.now = autoscaleT0.Add(90 * time.Second)
	at.run(want)
	at.mrfc.AssertNumberOfCalls(t, "GetAutoscalerInfo", 2)

	// The end of a window is a change.
	at.mrec.On("SetAutoscaler", "testns", "test", 1280*mib, 0.99, int32(1)).Once()
	at.now = autoscaleT0.Add(5 * time.Minute)
	at.run(want)
	assert.Equal(t, 1, f.updates)
	assert.Equal(t, int32(1), decodeState(t, f.stored).HighWindows)
	assert.Equal(t, 1, f.creates)
	at.mk.AssertExpectations(t)
	at.mrec.AssertExpectations(t)
}

// A store that grows below the threshold writes the state at most one time
// in each window.
func TestAutoscaleWritesAGrowingStoreOncePerWindow(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	f := at.fakeStateStore(t)
	used := int64(400)
	at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(func(*redisfailoverv1.RedisFailover, string) (*redis.AutoscalerInfo, error) {
		used += 7
		return masterInfo(used), nil
	})
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), mock.Anything, int32(0))

	for i := 0; i < 15; i++ {
		at.now = autoscaleT0.Add(time.Duration(i) * time.Minute)
		at.run(at.rf.Status.Autoscaling)
	}

	at.mrfc.AssertNumberOfCalls(t, "GetAutoscalerInfo", 15)
	assert.Equal(t, 1, f.creates)
	// The windows end at 5m and 10m.
	assert.Equal(t, 2, f.updates)
}

// memoryMax equal to the limit passes the validation, and is the ceiling: the
// status gives the reason and no value that the validation would reject.
func TestAutoscaleAtMemoryMax(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	max := resource.MustParse("1Gi")
	at.rf.Spec.Redis.Autoscaling.MemoryMax = &max
	require.NoError(t, at.rf.Validate())
	at.fakeStateStore(t)
	at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(masterInfo(990), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.99, int32(0)).Once()

	at.run(redisfailoverv1.AutoscalingStatus{})

	assert.Equal(t, redisfailoverv1.AutoscalingStatus{Workload: redisfailoverv1.AutoscalingWorkloadStore, Reason: autoscale.ReasonMaximum}, at.rf.Status.Autoscaling)
	at.mrec.AssertExpectations(t)
}

// A state ConfigMap that stayed after a mode change to Off, for example after
// a lost status patch, must not show its recommendation again.
func TestAutoscaleDiscardsAnOldState(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Status.State = redisfailoverv1.NotHealthyState
	f := at.fakeStateStore(t)
	state, err := json.Marshal(autoscale.State{
		Version: 1, Workload: "Store", RunID: "run1", OOMErrors: 3, HighWindows: 3,
		Recommended: 1280 * mib, Reason: autoscale.ReasonSustained,
		EvaluatedAt: autoscaleT0.Unix(), LastSample: autoscaleT0.Unix(),
	})
	require.NoError(t, err)
	f.stored = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf), ResourceVersion: "1"},
		Data:       map[string]string{"state": string(state)},
	}
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.0, int32(0)).Once()
	at.now = autoscaleT0.Add(time.Hour)

	at.run(redisfailoverv1.AutoscalingStatus{})

	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:    "Store",
		Reason:      autoscale.ReasonNotHealthy,
		EvaluatedAt: at.now.UTC().Format(time.RFC3339),
	}, at.rf.Status.Autoscaling)
	written := decodeState(t, f.stored)
	assert.Zero(t, written.Recommended)
	assert.Empty(t, written.RunID)
	at.mrec.AssertExpectations(t)
}

// The persisted sample time also limits the INFO calls after an operator
// restart, which loses the in-memory sample time.
func TestAutoscaleUsesThePersistedSampleTime(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	state, err := json.Marshal(autoscale.State{Version: 1, RunID: "run1", Reason: autoscale.ReasonLow, LastSample: autoscaleT0.Unix(), Window: autoscale.Window{PeakRatio: 0.5}})
	require.NoError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf)},
		Data:       map[string]string{"state": string(state)},
	}
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(cm, nil)
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.5, int32(0)).Once()
	at.now = autoscaleT0.Add(59 * time.Second)

	at.run(redisfailoverv1.AutoscalingStatus{})

	assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonLow}, at.rf.Status.Autoscaling)
	at.mk.AssertExpectations(t)
	at.mrec.AssertExpectations(t)
}

func TestAutoscaleSkipsTheSampleOfAnInstanceThatIsNotHealthy(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Status.State = redisfailoverv1.NotHealthyState
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound())
	at.mk.On("CreateConfigMap", "testns", mock.Anything).Return(nil)
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.0, int32(0)).Once()

	at.run(redisfailoverv1.AutoscalingStatus{})

	assert.Equal(t, autoscale.ReasonNotHealthy, at.rf.Status.Autoscaling.Reason)
	at.mrfc.AssertNotCalled(t, "GetAutoscalerInfo", mock.Anything, mock.Anything)
	at.mk.AssertExpectations(t)
}

func TestAutoscaleErrorsOnlyLog(t *testing.T) {
	observed := redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonLow}

	t.Run("state read", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, errors.New("forbidden"))

		at.run(observed)
		at.run(observed)

		assert.Equal(t, observed, at.rf.Status.Autoscaling)
		assert.Equal(t, redisfailoverv1.HealthyState, at.rf.Status.State)
		assert.Equal(t, []string{"Autoscaler: forbidden"}, at.logger.warnings)
		assert.Equal(t, []string{"Autoscaler: forbidden"}, at.logger.debugs)
	})

	t.Run("configmap of another owner", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM}}, nil)
		at.mrec.On("DeleteAutoscaler", "testns", "test").Twice()

		at.run(observed)
		at.run(observed)

		assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonForeignState}, at.rf.Status.Autoscaling)
		assert.Equal(t, []string{"Autoscaler: the state ConfigMap is not controlled by the RedisFailover"}, at.logger.warnings)
		at.mrfc.AssertNotCalled(t, "GetAutoscalerInfo", mock.Anything, mock.Anything)
		at.mrec.AssertExpectations(t)
	})

	t.Run("state of a newer operator", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf)},
			Data:       map[string]string{"state": `{"version":2}`},
		}, nil)
		at.mrec.On("DeleteAutoscaler", "testns", "test").Twice()

		at.run(observed)
		at.now = at.now.Add(time.Hour)
		at.run(observed)

		assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonNewerState}, at.rf.Status.Autoscaling)
		assert.Equal(t, []string{"Autoscaler: the state ConfigMap has version 2, this operator reads version 1"}, at.logger.warnings)
		assert.Len(t, at.logger.debugs, 1)
		// No write, no sample.
		at.mk.AssertExpectations(t)
		assert.Len(t, at.mk.Calls, 2)
		at.mrfc.AssertNotCalled(t, "GetAutoscalerInfo", mock.Anything, mock.Anything)
	})

	t.Run("INFO and write", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf)},
			Data:       map[string]string{"state": "{"},
		}, nil)
		at.mk.On("UpdateConfigMap", "testns", mock.Anything).Return(errors.New("conflict"))
		at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(nil, errors.New("timeout"))
		at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.0, int32(0)).Once()

		at.run(observed)

		assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonNoInfo}, at.rf.Status.Autoscaling)
		require.Len(t, at.logger.warnings, 2)
		assert.Contains(t, at.logger.warnings[0], "is not valid, the autoscaler starts again")
		assert.Equal(t, "Autoscaler: no autoscaler sample of 10.0.0.1: timeout\nconflict", at.logger.warnings[1])

		// A reconcile without an error forgets the logged error.
		at.h.logAutoscale(at.rf, nil)
		_, logged := at.h.autoscaleLogged.Load(failoverKey(at.rf))
		assert.False(t, logged)
	})
}

// An instance without autoscaling must make no API call, no Redis call and
// no metric call for the autoscaler.
func TestAutoscaleOffMakesNoCall(t *testing.T) {
	for _, mode := range []string{"", redisfailoverv1.AutoscalingModeOff} {
		t.Run("mode "+mode, func(t *testing.T) {
			at := newAutoscaleTest(t, mode)
			at.run(redisfailoverv1.AutoscalingStatus{})
			assert.Zero(t, at.rf.Status.Autoscaling)
			at.assertNoCalls(t)
			_, ok := at.h.autoscaleMasters.Load(failoverKey(at.rf))
			assert.False(t, ok, "the master of the reconcile is not kept")
		})
	}
}

func TestAutoscaleOffRemovesTheState(t *testing.T) {
	observed := redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonLow}

	t.Run("delete", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeOff)
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf)}}, nil).Once()
		at.mk.On("DeleteConfigMap", "testns", autoscaleCM).Return(nil).Once()
		at.mrec.On("DeleteAutoscaler", "testns", "test").Once()
		at.h.autoscaleSamples.Store(failoverKey(at.rf), autoscaleSample{at: autoscaleT0})

		at.run(observed)

		assert.Zero(t, at.rf.Status.Autoscaling)
		_, ok := at.h.autoscaleSamples.Load(failoverKey(at.rf))
		assert.False(t, ok)
		at.mk.AssertExpectations(t)
		at.mrec.AssertExpectations(t)
	})

	t.Run("already deleted", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound()).Once()
		at.mrec.On("DeleteAutoscaler", "testns", "test").Once()

		at.run(observed)

		assert.Zero(t, at.rf.Status.Autoscaling)
		at.mk.AssertExpectations(t)
	})

	t.Run("delete gone meanwhile", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf)}}, nil).Once()
		at.mk.On("DeleteConfigMap", "testns", autoscaleCM).Return(notFound()).Once()
		at.mrec.On("DeleteAutoscaler", "testns", "test").Once()

		at.run(observed)

		assert.Zero(t, at.rf.Status.Autoscaling)
	})

	t.Run("configmap of another owner is not deleted", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM}}, nil).Once()
		at.mrec.On("DeleteAutoscaler", "testns", "test").Once()

		at.run(redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonForeignState})

		assert.Zero(t, at.rf.Status.Autoscaling)
		at.mk.AssertNotCalled(t, "DeleteConfigMap", mock.Anything, mock.Anything)
		at.mrec.AssertExpectations(t)
	})

	t.Run("delete error keeps the status for a retry", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf)}}, nil).Once()
		at.mk.On("DeleteConfigMap", "testns", autoscaleCM).Return(errors.New("forbidden")).Once()

		at.run(observed)

		assert.Equal(t, observed, at.rf.Status.Autoscaling)
		assert.Equal(t, []string{"Autoscaler: forbidden"}, at.logger.warnings)
		at.mrec.AssertNotCalled(t, "DeleteAutoscaler", mock.Anything, mock.Anything)
	})
}

func TestForgetDropsTheAutoscalerState(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	key := failoverKey(at.rf)
	at.h.autoscaleMasters.Store(key, autoscaleMaster)
	at.h.autoscaleSamples.Store(key, autoscaleSample{at: autoscaleT0})
	at.h.autoscaleLogged.Store(key, "error")
	at.mrec.On("DeleteCluster", "testns", "test").Once()

	at.h.Forget(key)

	for _, m := range []interface{ Load(any) (any, bool) }{&at.h.autoscaleMasters, &at.h.autoscaleSamples, &at.h.autoscaleLogged} {
		_, ok := m.Load(key)
		assert.False(t, ok)
	}
	at.mrec.AssertExpectations(t)
}

// The checks replace rf.Status as a whole. The autoscaler status must survive
// a validation error and a panic, and an unchanged status writes nothing.
func TestAutoscalingStatusCarryOver(t *testing.T) {
	observed := redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonLow, HighWindows: 1}

	t.Run("validation error", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.rf.Status.Autoscaling = observed
		var patched redisfailoverv1.RedisFailoverStatus
		at.mk.On("UpdateRedisFailoverStatus", mock.Anything, "testns", at.rf, mock.Anything).Run(func(args mock.Arguments) {
			patched = args.Get(2).(*redisfailoverv1.RedisFailover).Status
		}).Once().Return()

		at.h.reportInvalid(at.rf, errors.New("invalid"))

		assert.Equal(t, observed, patched.Autoscaling)
		assert.Equal(t, "invalid", patched.Message)
	})

	t.Run("panic", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.rf.Status.Autoscaling = observed
		at.rf.Spec.Auth.SecretPath = "secret"
		at.mk.On("GetSecret", "testns", "secret").Run(func(mock.Arguments) { panic("boom") }).Return(nil, nil)
		at.mk.On("UpdateRedisFailoverStatus", mock.Anything, "testns", at.rf, mock.Anything).Once().Return()
		at.mrec.On("SetClusterError", "testns", "test").Once()

		assert.Panics(t, func() { _ = at.h.CheckAndHeal(at.rf) })

		assert.Equal(t, observed, at.rf.Status.Autoscaling)
		assert.Equal(t, redisfailoverv1.NotHealthyState, at.rf.Status.State)
	})

	t.Run("unchanged status", func(t *testing.T) {
		at := newAutoscaleTest(t, "")
		at.rf.Status.Autoscaling = observed
		observedStatus := at.rf.Status
		updateStatus(at.mk, at.rf, observedStatus)
		at.mk.AssertNotCalled(t, "UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything)

		at.mk.On("UpdateRedisFailoverStatus", mock.Anything, "testns", at.rf, mock.Anything).Once().Return()
		at.rf.Status.Autoscaling.HighWindows = 2
		updateStatus(at.mk, at.rf, observedStatus)
		at.mk.AssertExpectations(t)
	})
}
