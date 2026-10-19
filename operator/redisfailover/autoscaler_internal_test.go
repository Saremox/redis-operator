package redisfailover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
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
		memoryMax := resource.MustParse("2Gi")
		rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: mode, MemoryMax: &memoryMax}
	}
	require.NoError(t, rf.Validate())
	rf.Status.State = redisfailoverv1.HealthyState
	at := &autoscaleTest{
		rf:     rf,
		mk:     &mK8SService.Services{},
		mrfc:   noUnknownSentinelAddrs(&mRFService.RedisFailoverCheck{}),
		mrec:   &mMetrics.Recorder{},
		logger: &warnLogger{},
		now:    autoscaleT0,
	}
	// The tests of the steps assert the phase metric.
	at.mrec.On("SetAutoscalerPhase", "testns", "test", mock.Anything).Maybe()
	at.h = NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, at.mrfc, &mRFService.RedisFailoverHeal{}, at.mk, at.mrec, at.logger)
	at.h.now = func() time.Time { return at.now }
	// Each read of the state also reads the live limit. The tests of the
	// floor set their own StatefulSet with onStatefulSet.
	at.onStatefulSet().Maybe().Return(redisStatefulSet("1Gi"), nil)
	return at
}

// onStatefulSet replaces the answer of GetStatefulSet.
func (at *autoscaleTest) onStatefulSet() *mock.Call {
	at.mk.ExpectedCalls = slices.DeleteFunc(at.mk.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "GetStatefulSet" })
	return at.mk.On("GetStatefulSet", "testns", "rfr-test")
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
	// onWrite sees each state that a create or an update stores.
	onWrite func(autoscale.State)
	// err fails each create and update.
	err error
}

func (at *autoscaleTest) fakeStateStore(t *testing.T) *fakeStateStore {
	t.Helper()
	f := &fakeStateStore{}
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(func(string, string) (*corev1.ConfigMap, error) {
		if f.stored == nil {
			return nil, notFound()
		}
		return f.stored.DeepCopy(), nil
	})
	written := func() error {
		if f.onWrite != nil && f.err == nil {
			f.onWrite(decodeState(t, f.stored))
		}
		return f.err
	}
	at.mk.On("CreateConfigMap", "testns", mock.Anything).Return(func(_ string, cm *corev1.ConfigMap) error {
		if f.err != nil {
			return f.err
		}
		f.creates++
		f.stored = cm.DeepCopy()
		f.stored.ResourceVersion = "1"
		return written()
	})
	at.mk.On("UpdateConfigMap", "testns", mock.Anything).Return(func(_ string, cm *corev1.ConfigMap) error {
		if f.err != nil {
			return f.err
		}
		f.updates++
		assert.Equal(t, f.stored.ResourceVersion, cm.ResourceVersion)
		f.stored = cm.DeepCopy()
		f.stored.ResourceVersion += "1"
		return written()
	})
	return f
}

func TestAutoscaleWritesTheStateOnlyOnAChange(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	f := at.fakeStateStore(t)
	info := masterInfo(990)
	at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(func(*redisfailoverv1.RedisFailover, string) (*redis.AutoscalerInfo, error) {
		return info, nil
	})
	at.mrec.On("SetAutoscaler", "testns", "test", 1280*mib, 0.99, int32(0), 1024*mib).Times(4)

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
	at.mrec.On("SetAutoscaler", "testns", "test", 1280*mib, 0.99, int32(1), 1024*mib).Once()
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
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), mock.Anything, int32(0), 1024*mib)

	for i := range 15 {
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
	memoryMax := resource.MustParse("1Gi")
	at.rf.Spec.Redis.Autoscaling.MemoryMax = &memoryMax
	require.NoError(t, at.rf.Validate())
	at.fakeStateStore(t)
	at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(masterInfo(990), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.99, int32(0), 1024*mib).Once()

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
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.0, int32(0), 1024*mib).Once()
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
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.5, int32(0), 1024*mib).Once()
	at.now = autoscaleT0.Add(59 * time.Second)

	at.run(redisfailoverv1.AutoscalingStatus{})

	assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonLow}, at.rf.Status.Autoscaling)
	at.mk.AssertExpectations(t)
	at.mrec.AssertExpectations(t)
}

// After the clock went back, a sample time in the future must not stop the
// samples until the clock reaches that time.
func TestAutoscaleSamplesAfterTheClockWentBack(t *testing.T) {
	future := autoscaleT0.Add(time.Hour)
	tests := []struct {
		name      string
		memory    bool
		persisted bool
	}{
		{name: "sample time in memory", memory: true},
		{name: "persisted sample time", persisted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			f := at.fakeStateStore(t)
			if test.memory {
				at.h.autoscaleSamples.Store(failoverKey(at.rf), autoscaleSample{at: future})
			}
			if test.persisted {
				state, err := json.Marshal(autoscale.State{Version: 1, RunID: "run1", LastSample: future.Unix()})
				require.NoError(t, err)
				f.stored = &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf), ResourceVersion: "1"},
					Data:       map[string]string{"state": string(state)},
				}
			}
			at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(masterInfo(500), nil)
			at.mrec.On("SetAutoscaler", "testns", "test", int64(0), mock.Anything, int32(0), 1024*mib)

			at.run(redisfailoverv1.AutoscalingStatus{})

			at.mrfc.AssertNumberOfCalls(t, "GetAutoscalerInfo", 1)
			assert.Equal(t, autoscale.ReasonLow, at.rf.Status.Autoscaling.Reason)
			require.NotNil(t, f.stored)
			assert.Equal(t, autoscaleT0.Unix(), decodeState(t, f.stored).LastSample)
		})
	}
}

// While the state writes fail, each sample must continue from the last
// sample and not from the stored state. Else the status flips between the
// stored and the sampled state, and the windows never add up.
func TestAutoscaleContinuesWhileTheWritesFail(t *testing.T) {
	low := autoscale.State{
		Version: 1, Workload: "Store", RunID: "run1", Reason: autoscale.ReasonLow,
		Window:     autoscale.Window{Start: autoscaleT0.Unix(), PeakRatio: 0.5, PeakUsed: 500 * mib},
		LastSample: autoscaleT0.Unix(), Limit: 1024 * mib,
	}
	warmUp := autoscale.State{Version: 1, RunID: "run1", Reason: autoscale.ReasonWarmUp, LastSample: autoscaleT0.Unix(), Limit: 1024 * mib}
	tests := []struct {
		name    string
		stored  *autoscale.State
		used    int64
		changes int
		want    redisfailoverv1.AutoscalingStatus
	}{
		{name: "low use", stored: &low, used: 500, want: redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonLow}},
		{name: "stored warm up", stored: &warmUp, used: 500, want: redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonLow}},
		{name: "create fails", used: 500, want: redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonLow}},
		{
			name: "high use", stored: &low, used: 900,
			// The windows end at 5m, 10m and 15m.
			changes: 3,
			want: redisfailoverv1.AutoscalingStatus{
				Workload:          "Store",
				RecommendedMemory: "1280Mi",
				Reason:            autoscale.ReasonSustained,
				EvaluatedAt:       autoscaleT0.Add(15 * time.Minute).UTC().Format(time.RFC3339),
				HighWindows:       3,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			if test.stored != nil {
				state, err := json.Marshal(*test.stored)
				require.NoError(t, err)
				at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf), ResourceVersion: "1"},
					Data:       map[string]string{"state": string(state)},
				}, nil)
				at.mk.On("UpdateConfigMap", "testns", mock.Anything).Return(errors.New("forbidden"))
			} else {
				at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound())
				at.mk.On("CreateConfigMap", "testns", mock.Anything).Return(errors.New("exceeded quota"))
			}
			at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(masterInfo(test.used), nil)
			at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, 1024*mib)

			changes := 0
			var last redisfailoverv1.AutoscalingStatus
			for at.now = autoscaleT0.Add(time.Minute); at.now.Before(autoscaleT0.Add(31 * time.Minute)); at.now = at.now.Add(20 * time.Second) {
				at.run(last)
				if at.now.After(autoscaleT0.Add(time.Minute)) && at.rf.Status.Autoscaling != last {
					changes++
				}
				last = at.rf.Status.Autoscaling
			}
			assert.Equal(t, test.changes, changes)
			assert.Equal(t, test.want, last)
			at.mrfc.AssertNumberOfCalls(t, "GetAutoscalerInfo", 30)
			at.mk.AssertExpectations(t)
		})
	}
}

// Another operator, for example a later leader, can write the state. The
// state in memory then is out of date, also when its sample time is later.
func TestAutoscaleUsesAStateThatAnotherOperatorWrote(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    redisfailoverv1.AutoscalingStatus
	}{
		{name: "same resourceVersion", version: "1", want: redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonLow}},
		{name: "changed resourceVersion", version: "2", want: redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonHigh, HighWindows: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			sampled := autoscaleT0.Add(10 * time.Second)
			cached := autoscale.State{Version: 1, Workload: "Store", RunID: "run1", Reason: autoscale.ReasonLow, LastSample: sampled.Unix()}
			at.h.autoscaleSamples.Store(failoverKey(at.rf), autoscaleSample{at: sampled, ratio: 0.5, state: cached, version: "1", sampled: true})
			state, err := json.Marshal(autoscale.State{Version: 1, Workload: "Store", RunID: "run1", Reason: autoscale.ReasonHigh, HighWindows: 2, LastSample: autoscaleT0.Unix()})
			require.NoError(t, err)
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf), ResourceVersion: test.version},
				Data:       map[string]string{"state": string(state)},
			}, nil)
			at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.5, mock.Anything, 1024*mib).Once()
			at.now = autoscaleT0.Add(30 * time.Second)

			at.run(redisfailoverv1.AutoscalingStatus{})

			assert.Equal(t, test.want, at.rf.Status.Autoscaling)
			at.mrfc.AssertNotCalled(t, "GetAutoscalerInfo", mock.Anything, mock.Anything)
			at.mrec.AssertExpectations(t)
		})
	}
}

// The clock goes back behind the stored sample time while the writes fail.
// The samples must still continue from the state in memory, so that the high
// windows add up.
func TestAutoscaleContinuesAfterTheClockWentBackWhileTheWritesFail(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	state, err := json.Marshal(autoscale.State{
		Version: 1, Workload: "Store", RunID: "run1", Reason: autoscale.ReasonLow,
		Window:     autoscale.Window{Start: autoscaleT0.Unix(), PeakRatio: 0.5},
		LastSample: autoscaleT0.Unix(), Limit: 1024 * mib,
	})
	require.NoError(t, err)
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf), ResourceVersion: "1"},
		Data:       map[string]string{"state": string(state)},
	}, nil)
	at.mk.On("UpdateConfigMap", "testns", mock.Anything).Return(errors.New("forbidden"))
	at.mrfc.On("GetAutoscalerInfo", at.rf, autoscaleMaster).Return(masterInfo(900), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, 1024*mib)

	back := autoscaleT0.Add(-time.Hour)
	for at.now = back; at.now.Before(back.Add(16 * time.Minute)); at.now = at.now.Add(20 * time.Second) {
		at.run(at.rf.Status.Autoscaling)
	}

	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:          "Store",
		RecommendedMemory: "1280Mi",
		Reason:            autoscale.ReasonSustained,
		EvaluatedAt:       back.Add(15 * time.Minute).UTC().Format(time.RFC3339),
		HighWindows:       3,
	}, at.rf.Status.Autoscaling)
}

func TestAutoscaleSkipsTheSampleOfAnInstanceThatIsNotHealthy(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Status.State = redisfailoverv1.NotHealthyState
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound())
	at.mk.On("CreateConfigMap", "testns", mock.Anything).Return(nil)
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.0, int32(0), 1024*mib).Once()

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
			Data:       map[string]string{"state": `{"version":3}`},
		}, nil)
		at.mrec.On("DeleteAutoscaler", "testns", "test").Twice()

		at.run(observed)
		at.now = at.now.Add(time.Hour)
		at.run(observed)

		assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonNewerState}, at.rf.Status.Autoscaling)
		assert.Equal(t, []string{"Autoscaler: the state ConfigMap has version 3, this operator reads version 2"}, at.logger.warnings)
		assert.Len(t, at.logger.debugs, 1)
		// No write, no sample: each run reads the state and the live limit.
		at.mk.AssertExpectations(t)
		assert.Len(t, at.mk.Calls, 4)
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
		at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.0, int32(0), 1024*mib).Once()

		at.run(observed)

		// The invalid state freezes the limit also in mode Recommend.
		assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonFrozen}, at.rf.Status.Autoscaling)
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
	at.h.autoscaleReads.Store(key, autoscaleRead{})
	at.h.autoscaleMasters.Store(key, autoscaleMaster)
	at.h.autoscaleRollout.Store(key, autoscaleRollout{settled: true})
	at.h.autoscaleSamples.Store(key, autoscaleSample{at: autoscaleT0})
	at.h.autoscaleLogged.Store(key, "error")
	at.mrec.On("DeleteCluster", "testns", "test").Once()

	at.h.Forget(key)

	for _, m := range []interface{ Load(any) (any, bool) }{&at.h.autoscaleReads, &at.h.autoscaleMasters, &at.h.autoscaleRollout, &at.h.autoscaleSamples, &at.h.autoscaleLogged} {
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

		at.h.reportInvalid(context.Background(), at.rf, errors.New("invalid"))

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
		updateStatus(context.Background(), at.mk, at.rf, observedStatus)
		at.mk.AssertNotCalled(t, "UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything)

		at.mk.On("UpdateRedisFailoverStatus", mock.Anything, "testns", at.rf, mock.Anything).Once().Return()
		at.rf.Status.Autoscaling.HighWindows = 2
		updateStatus(context.Background(), at.mk, at.rf, observedStatus)
		at.mk.AssertExpectations(t)
	})
}

// An instance without autoscaling keeps no master of the reconcile in memory.
func TestCheckAndHealKeepsNoMasterWithoutAutoscaling(t *testing.T) {
	const sentinel = "10.0.0.9"
	for _, sentinelMode := range []bool{false, true} {
		for _, mode := range []string{"", redisfailoverv1.AutoscalingModeOff} {
			t.Run(fmt.Sprintf("sentinel %v, mode %q", sentinelMode, mode), func(t *testing.T) {
				at := newAutoscaleTest(t, mode)
				at.rf.Spec.Sentinel.Enabled = new(sentinelMode)
				at.h.mClient = metrics.Dummy
				noAppliedSecret(at.mk)
				at.mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Maybe().Return(&corev1.PodList{Items: make([]corev1.Pod, 5)}, nil)
				at.mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
				mrfh := &mRFService.RedisFailoverHeal{}
				mrfh.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
				mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
				mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
				mrfh.On("SetRedisCustomConfig", autoscaleMaster, at.rf).Return(nil)
				at.h.rfHealer = mrfh
				at.mrfc.On("IsRedisRunning", at.rf).Return(true)
				at.mrfc.On("GetNumberMasters", at.rf).Return(1, nil)
				at.mrfc.On("GetSentinelMasterIPs", at.rf).Maybe().Return(nil, nil)
				at.mrfc.On("CheckAllSlavesFromMaster", autoscaleMaster, at.rf).Return(nil)
				at.mrfc.On("GetRedisesIPs", at.rf).Return([]string{autoscaleMaster}, nil)
				at.mrfc.On("GetMasterIP", at.rf).Return(autoscaleMaster, nil)
				at.mrfc.On("GetStatefulSetUpdateRevision", at.rf).Return("1", nil)
				at.mrfc.On("GetRedisesSlavesPods", at.rf).Return([]string{}, nil)
				at.mrfc.On("GetRedisesMasterPod", at.rf).Return(autoscaleMaster, nil)
				at.mrfc.On("GetRedisRevisionHash", autoscaleMaster, at.rf).Return("1", nil)
				if sentinelMode {
					at.mrfc.On("IsSentinelRunningQuorum", at.rf).Return(true)
					at.mrfc.On("GetSentinelsIPs", at.rf).Return([]string{sentinel}, nil)
					at.mrfc.On("CheckSentinelMonitor", sentinel, autoscaleMaster, "6379").Return(nil)
					at.mrfc.On("CheckSentinelNumberInMemory", sentinel, at.rf).Return(nil)
					at.mrfc.On("CheckSentinelSlavesNumberInMemory", sentinel, at.rf).Return(nil)
					mrfh.On("SetSentinelCustomConfig", sentinel, at.rf).Return(nil)
				} else {
					at.mrfc.On("CheckMasterHealth", at.rf).Return(true, autoscaleMaster, nil)
				}

				require.NoError(t, at.h.CheckAndHeal(at.rf))

				assert.Equal(t, redisfailoverv1.HealthyState, at.rf.Status.State)
				_, kept := at.h.autoscaleMasters.Load(failoverKey(at.rf))
				assert.False(t, kept)
				at.mrfc.AssertExpectations(t)
			})
		}
	}
}

// stateConfigMap returns a state ConfigMap of the RedisFailover.
func (at *autoscaleTest) stateConfigMap(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM, OwnerReferences: at.h.createOwnerReferences(at.rf), ResourceVersion: "1"},
		Data:       map[string]string{"state": data},
	}
}

func encodeState(t *testing.T, s autoscale.State) string {
	t.Helper()
	data, err := json.Marshal(s)
	require.NoError(t, err)
	return string(data)
}

// Ensure writes the pods with the effective limit, so Handle reads the state
// before Ensure. Without autoscaling, Handle reads the ConfigMap one time
// after the operator start, for a leftover state.
func TestHandleReadsTheAutoscalerStateBeforeEnsure(t *testing.T) {
	stop := errors.New("stop")
	tests := []struct {
		name    string
		mode    string
		readErr error
		want    []string
		wantErr string
	}{
		{name: "unset", readErr: notFound(), want: []string{"GetConfigMap", "Ensure", "Ensure"}, wantErr: "stop"},
		{name: "Off", mode: redisfailoverv1.AutoscalingModeOff, readErr: notFound(), want: []string{"GetConfigMap", "Ensure", "Ensure"}, wantErr: "stop"},
		{name: "Recommend", mode: redisfailoverv1.AutoscalingModeRecommend, readErr: notFound(), want: []string{"GetConfigMap", "Ensure"}, wantErr: "stop"},
		{name: "read error", mode: redisfailoverv1.AutoscalingModeRecommend, readErr: errors.New("forbidden"), want: []string{"GetConfigMap"}, wantErr: "autoscaler state: forbidden"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			at := newAutoscaleTest(t, test.mode)
			at.rf.Finalizers = []string{redisFailoverFinalizer}
			var calls []string
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Run(func(mock.Arguments) { calls = append(calls, "GetConfigMap") }).Return(nil, test.readErr)
			mrfs := &mRFService.RedisFailoverClient{}
			mrfs.On("EnsureNotPresentRedisService", mock.Anything).Maybe().Run(func(mock.Arguments) { calls = append(calls, "Ensure") }).Return(stop)
			at.h.rfService = mrfs
			if at.rf.AutoscalingEnabled() {
				at.mrec.On("SetClusterError", "testns", "test").Once()
			} else {
				at.mrec.On("SetClusterError", "testns", "test").Times(2)
			}

			err := at.h.Handle(context.Background(), at.rf)
			require.EqualError(t, err, test.wantErr)
			if !at.rf.AutoscalingEnabled() {
				// One look for a leftover state after the operator start.
				require.EqualError(t, at.h.Handle(context.Background(), at.rf), test.wantErr)
			}

			assert.Equal(t, test.want, calls)
			at.mk.AssertExpectations(t)
			at.mrec.AssertExpectations(t)
			assert.Empty(t, at.mrfc.Calls, "no INFO")
		})
	}
}

// In mode Auto, the spec limit can shrink the pods. A state that gives no
// limit freezes it at the limit of the live StatefulSet, in [spec, memoryMax].
func TestReadAutoscaleFreezesTheLimitInAuto(t *testing.T) {
	newer := encodeState(t, autoscale.State{Version: autoscale.StateVersion + 1, Effective: 1536 * mib})
	states := []struct {
		name    string
		cm      func(at *autoscaleTest) (*corev1.ConfigMap, error)
		blocked string
	}{
		{name: "missing", cm: func(*autoscaleTest) (*corev1.ConfigMap, error) { return nil, notFound() }},
		{name: "corrupt", cm: func(at *autoscaleTest) (*corev1.ConfigMap, error) { return at.stateConfigMap("{"), nil }},
		{name: "foreign", cm: func(*autoscaleTest) (*corev1.ConfigMap, error) {
			return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM}}, nil
		}, blocked: autoscale.ReasonForeignState},
		{name: "newer", cm: func(at *autoscaleTest) (*corev1.ConfigMap, error) { return at.stateConfigMap(newer), nil }, blocked: autoscale.ReasonNewerState},
	}
	templates := []struct {
		name  string
		ss    *appsv1.StatefulSet
		err   error
		limit int64
	}{
		{name: "resized template", ss: redisStatefulSet("1280Mi"), limit: 1280 * mib},
		{name: "template above memoryMax", ss: redisStatefulSet("8Gi"), limit: 2048 * mib},
		{name: "template below the spec", ss: redisStatefulSet("512Mi"), limit: 1024 * mib},
		{name: "no StatefulSet", err: apierrors.NewNotFound(schema.GroupResource{Resource: "statefulsets"}, "rfr-test"), limit: 1024 * mib},
	}
	for _, state := range states {
		for _, template := range templates {
			t.Run(state.name+", "+template.name, func(t *testing.T) {
				at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
				at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
				cm, cmErr := state.cm(at)
				at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(cm, cmErr)
				at.onStatefulSet().Once().Return(template.ss, template.err)
				if template.err != nil {
					at.mk.On("ListPods", "testns").Once().Return(&corev1.PodList{}, nil)
				}

				read, err := at.h.readAutoscale(at.rf)

				require.NoError(t, err)
				assert.Equal(t, template.limit, read.effective)
				assert.Equal(t, 1024*mib, read.spec)
				assert.Equal(t, state.blocked, read.blocked)
				at.mk.AssertExpectations(t)
			})
		}
	}

	t.Run("StatefulSet read error", func(t *testing.T) {
		at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
		at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
		at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(nil, notFound())
		at.onStatefulSet().Return(nil, errors.New("forbidden"))

		_, err := at.h.readAutoscale(at.rf)

		assert.EqualError(t, err, "forbidden")
	})
}

func redisStatefulSet(limit string) *appsv1.StatefulSet {
	ss := &appsv1.StatefulSet{}
	ss.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:      "redis",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}},
	}}
	return ss
}

// Recommend changes no pod, so it reads no StatefulSet and uses the spec
// limit when the state gives no limit. A valid state gives its limit in both
// modes: Recommend keeps the limit of an earlier mode Auto.
func TestReadAutoscaleUsesTheStateLimit(t *testing.T) {
	step := autoscale.State{Version: autoscale.StateVersion, Effective: 1280 * mib, SpecLimit: 1024 * mib, Phase: autoscale.PhaseDone}
	tests := []struct {
		name  string
		mode  string
		data  string
		limit int64
	}{
		{name: "Recommend, corrupt", mode: redisfailoverv1.AutoscalingModeRecommend, data: "{", limit: 1024 * mib},
		{name: "Recommend, newer", mode: redisfailoverv1.AutoscalingModeRecommend, data: `{"version":3,"effective":2147483648}`, limit: 1024 * mib},
		{name: "Recommend, version 1", mode: redisfailoverv1.AutoscalingModeRecommend, data: `{"version":1,"limit":1073741824}`, limit: 1024 * mib},
		{name: "Recommend, step", mode: redisfailoverv1.AutoscalingModeRecommend, data: encodeState(t, step), limit: 1280 * mib},
		{name: "Auto, step", mode: redisfailoverv1.AutoscalingModeAuto, data: encodeState(t, step), limit: 1280 * mib},
		{name: "Auto, step above memoryMax", mode: redisfailoverv1.AutoscalingModeAuto, data: `{"version":2,"effective":4294967296}`, limit: 2048 * mib},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.rf.Spec.Redis.Autoscaling.Mode = test.mode
			at.mk.On("GetConfigMap", "testns", autoscaleCM).Once().Return(at.stateConfigMap(test.data), nil)

			read, err := at.h.readAutoscale(at.rf)

			require.NoError(t, err)
			assert.Equal(t, test.limit, read.effective)
			at.mk.AssertExpectations(t)
		})
	}
}

// handle runs the parts of Handle and CheckAndHeal for the autoscaler: the
// read before Ensure, and the autoscaler with the effective copy after the
// checks. Handle itself refuses mode Auto in Validate.
func (at *autoscaleTest) handle(t *testing.T, observed redisfailoverv1.AutoscalingStatus) *redisfailoverv1.RedisFailover {
	t.Helper()
	rf, err := at.h.withEffectiveLimit(at.rf)
	require.NoError(t, err)
	at.h.autoscaleMasters.Store(failoverKey(rf), autoscaleMaster)
	rf.Status.Autoscaling = redisfailoverv1.AutoscalingStatus{}
	at.h.autoscale(rf, observed)
	_, kept := at.h.autoscaleReads.Load(failoverKey(rf))
	assert.False(t, kept)
	return rf
}

// The state write keeps a frozen limit, so the next reconcile reads it from
// the state and not from the StatefulSet.
func TestAutoscaleWritesTheFrozenLimit(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
	f := at.fakeStateStore(t)
	at.onStatefulSet().Return(redisStatefulSet("1280Mi"), nil)
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", int64(0), 0.5, int32(0), 1280*mib).Twice()

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{})

	require.Equal(t, 1, f.creates)
	written := decodeState(t, f.stored)
	assert.Equal(t, 1280*mib, written.Effective)
	assert.Equal(t, 1024*mib, written.SpecLimit)
	assert.Equal(t, 1280*mib, written.Limit, "the sample uses the effective limit")
	assert.True(t, written.Frozen, "no step starts from a frozen limit")
	assert.Equal(t, redisfailoverv1.AutoscalingStatus{Workload: "Store", Reason: autoscale.ReasonFrozen, EffectiveMemory: "1280Mi", Phase: autoscale.PhaseIdle}, rf.Status.Autoscaling)

	at.now = at.now.Add(time.Minute)
	rf = at.handle(t, rf.Status.Autoscaling)
	assert.Equal(t, "1280Mi", rf.Status.Autoscaling.EffectiveMemory)
	at.mk.AssertNumberOfCalls(t, "GetStatefulSet", 2)
	at.mrec.AssertExpectations(t)
}

// A lost state also lost its budget and its back-off. A status shows the
// loss, so mode Auto freezes also the spec limit. A RedisFailover without an
// earlier status has no state to lose.
func TestAutoscaleFreezesALostStateAtTheSpecLimit(t *testing.T) {
	for name, observed := range map[string]redisfailoverv1.AutoscalingStatus{
		"lost":  {Workload: "Store", Reason: autoscale.ReasonLow, Phase: autoscale.PhaseIdle},
		"first": {},
		// The first write of the state failed, and the status has no phase.
		"failed first write": {Workload: "Store", Reason: autoscale.ReasonLow},
	} {
		t.Run(name, func(t *testing.T) {
			at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
			at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
			f := at.fakeStateStore(t)
			at.onStatefulSet().Return(redisStatefulSet("1Gi"), nil)
			at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)
			at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, 1024*mib).Maybe()

			rf := at.handle(t, observed)

			frozen := name == "lost"
			written := decodeState(t, f.stored)
			assert.Equal(t, frozen, written.Frozen)
			assert.Zero(t, written.Effective)
			assert.Equal(t, frozen, rf.Status.Autoscaling.Reason == autoscale.ReasonFrozen)
		})
	}
}

// A state of another owner or of a newer operator is not written, and the
// status shows the frozen limit.
func TestAutoscaleShowsTheFrozenLimitOfABlockedState(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Autoscaling.Mode = redisfailoverv1.AutoscalingModeAuto
	at.mk.On("GetConfigMap", "testns", autoscaleCM).Return(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: autoscaleCM}}, nil)
	at.onStatefulSet().Return(redisStatefulSet("1280Mi"), nil)
	at.mrec.On("DeleteAutoscaler", "testns", "test").Once()

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{})

	assert.Equal(t, redisfailoverv1.AutoscalingStatus{Reason: autoscale.ReasonForeignState, EffectiveMemory: "1280Mi"}, rf.Status.Autoscaling)
	assert.Equal(t, []string{"Autoscaler: the state ConfigMap is not controlled by the RedisFailover"}, at.logger.warnings)
	at.mk.AssertExpectations(t)
	at.mrec.AssertExpectations(t)
	at.mrfc.AssertNotCalled(t, "GetAutoscalerInfo", mock.Anything, mock.Anything)
}

// Recommend keeps the effective limit of an earlier mode Auto, and the
// recommendation is a step from that limit.
func TestAutoscaleRecommendKeepsTheEffectiveLimit(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	f := at.fakeStateStore(t)
	stored := autoscale.State{
		Version: autoscale.StateVersion, Workload: "Store", RunID: "run1", Limit: 1536 * mib, LastSample: autoscaleT0.Unix(),
		Effective: 1536 * mib, SpecLimit: 1280 * mib, Phase: autoscale.PhaseDone, StartedAt: autoscaleT0.Add(-time.Hour).Unix(),
	}
	f.stored = at.stateConfigMap(encodeState(t, stored))
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(990), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", 1920*mib, 0.99, int32(0), 1536*mib).Once()
	at.now = autoscaleT0.Add(time.Minute)

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{})

	assert.Equal(t, redisfailoverv1.AutoscalingStatus{
		Workload:          "Store",
		RecommendedMemory: "1920Mi",
		Reason:            autoscale.ReasonUrgent + "; " + autoscale.NoteSpecBelowEffective,
		EvaluatedAt:       at.now.UTC().Format(time.RFC3339),
		EffectiveMemory:   "1536Mi",
	}, rf.Status.Autoscaling, "Recommend shows no step of an earlier mode Auto")
	assert.Equal(t, "1536Mi", rf.Spec.Redis.Resources.Limits.Memory().String())
	assert.Equal(t, "1Gi", at.rf.Spec.Redis.Resources.Limits.Memory().String())
	written := decodeState(t, f.stored)
	assert.Equal(t, stored.Effective, written.Effective)
	assert.Equal(t, autoscale.PhaseDone, written.Phase)
	at.mrec.AssertExpectations(t)
}

// Handle gives Ensure and the checks the effective limit of the state, and
// the spec of the RedisFailover stays.
func TestHandleUsesTheEffectiveLimit(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Finalizers = []string{redisFailoverFinalizer}
	at.rf.Spec.Sentinel.Enabled = new(false)
	at.h.mClient = metrics.Dummy
	f := at.fakeStateStore(t)
	f.stored = at.stateConfigMap(encodeState(t, autoscale.State{
		Version: autoscale.StateVersion, Effective: 1280 * mib, SpecLimit: 1024 * mib, Phase: autoscale.PhaseDone,
	}))
	noAppliedSecret(at.mk)
	at.mk.On("GetStatefulSetPods", mock.Anything, mock.Anything).Maybe().Return(&corev1.PodList{}, nil)
	at.mk.On("UpdateRedisFailoverStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	limits := map[string]string{}
	capture := func(name string) func(mock.Arguments) {
		return func(args mock.Arguments) {
			limits[name] = args.Get(0).(*redisfailoverv1.RedisFailover).Spec.Redis.Resources.Limits.Memory().String()
		}
	}
	mrfs := &mRFService.RedisFailoverClient{}
	for _, ensure := range []string{"EnsureNotPresentRedisService", "EnsureNotPresentSentinelResources"} {
		mrfs.On(ensure, mock.Anything).Once().Run(capture(ensure)).Return(nil)
	}
	for _, ensure := range []string{"EnsureRedisMasterService", "EnsureRedisSlaveService", "EnsureRedisShutdownConfigMap", "EnsureRedisReadinessConfigMap", "EnsureRedisConfigMap", "EnsureRedisStatefulset"} {
		mrfs.On(ensure, mock.Anything, mock.Anything, mock.Anything).Once().Run(capture(ensure)).Return(nil)
	}
	at.h.rfService = mrfs
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("AbortOrphanedFailover", mock.Anything, mock.Anything).Maybe().Return(false, nil)
	mrfh.On("ApplyPassword", mock.Anything, mock.Anything, mock.Anything).Maybe().Return(true, false, nil)
	mrfh.On("ApplySentinelPassword", mock.Anything, mock.Anything).Maybe().Return(true, nil)
	at.h.rfHealer = mrfh
	at.mrfc.On("IsRedisRunning", mock.Anything).Once().Run(capture("IsRedisRunning")).Return(false)
	at.mrfc.On("IsRedisRunningQuorum", mock.Anything).Once().Run(capture("IsRedisRunningQuorum")).Return(false)

	require.NoError(t, at.h.Handle(context.Background(), at.rf))

	require.Contains(t, limits, "EnsureRedisStatefulset")
	require.Contains(t, limits, "IsRedisRunning")
	require.Contains(t, limits, "IsRedisRunningQuorum")
	for name, limit := range limits {
		assert.Equal(t, "1280Mi", limit, name)
	}
	assert.Equal(t, "1Gi", at.rf.Spec.Redis.Resources.Limits.Memory().String(), "the spec stays")
	mrfs.AssertExpectations(t)
}

// A spec limit above the effective limit is a user change. The state write
// then keeps no effective limit, so a later lower spec cannot bring it back.
func TestAutoscaleWritesTheBoundOfASpecChange(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	at.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1536Mi")
	f := at.fakeStateStore(t)
	f.stored = at.stateConfigMap(encodeState(t, autoscale.State{
		Version: autoscale.StateVersion, Workload: "Store", RunID: "run1", Limit: 1280 * mib, LastSample: autoscaleT0.Unix(),
		Effective: 1280 * mib, SpecLimit: 1024 * mib, Phase: autoscale.PhaseDone,
	}))
	at.mrfc.On("GetAutoscalerInfo", mock.Anything, autoscaleMaster).Return(masterInfo(500), nil)
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, 1536*mib).Once()
	at.now = autoscaleT0.Add(time.Minute)

	rf := at.handle(t, redisfailoverv1.AutoscalingStatus{})

	require.Equal(t, 1, f.updates)
	assert.Zero(t, decodeState(t, f.stored).Effective)
	assert.Empty(t, rf.Status.Autoscaling.EffectiveMemory)
	at.mrec.AssertExpectations(t)
}

// A spec change that ends the effective limit is written also when no sample
// is due. Otherwise a spec lowered again before the next sample gets the
// effective limit back. Reconciles without a change write nothing.
func TestAutoscaleWritesTheBoundWithoutASample(t *testing.T) {
	at := newAutoscaleTest(t, redisfailoverv1.AutoscalingModeRecommend)
	f := at.fakeStateStore(t)
	f.stored = at.stateConfigMap(encodeState(t, autoscale.State{
		Version: autoscale.StateVersion, Workload: "Store", RunID: "run1", Limit: 1280 * mib, LastSample: autoscaleT0.Unix(),
		Effective: 1280 * mib, SpecLimit: 1024 * mib, Phase: autoscale.PhaseDone,
	}))
	at.mrec.On("SetAutoscaler", "testns", "test", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	setSpec := func(limit string) {
		at.rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(limit)
	}

	setSpec("1536Mi")
	at.now = autoscaleT0.Add(20 * time.Second)
	at.handle(t, redisfailoverv1.AutoscalingStatus{})

	require.Equal(t, 1, f.updates)
	assert.Zero(t, decodeState(t, f.stored).Effective)
	at.mrfc.AssertNotCalled(t, "GetAutoscalerInfo", mock.Anything, mock.Anything)

	setSpec("1Gi")
	at.now = autoscaleT0.Add(40 * time.Second)
	read, err := at.h.readAutoscale(at.rf)
	require.NoError(t, err)
	assert.Equal(t, 1024*mib, read.effective)

	at.handle(t, redisfailoverv1.AutoscalingStatus{})
	at.now = autoscaleT0.Add(50 * time.Second)
	at.handle(t, redisfailoverv1.AutoscalingStatus{})
	assert.Equal(t, 1, f.updates, "no write without a change")
	assert.Zero(t, f.creates)
}
