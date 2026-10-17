package redisfailover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	"github.com/saremox/redis-operator/operator/redisfailover/autoscale"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

// withAutoscaling sets the mode after Validate, because Validate refuses
// mode Auto.
func withAutoscaling(rf *redisfailoverv1.RedisFailover, mode string) *redisfailoverv1.RedisFailover {
	memoryMax := resource.MustParse("64Gi")
	rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: mode, MemoryMax: &memoryMax, Workload: redisfailoverv1.AutoscalingWorkloadAuto}
	return rf
}

func exampleFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../example/redisfailover/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	return files
}

// An effective limit equal to the spec limit must give the golden files, or
// an upgrade of the operator restarts the pods of each RedisFailover. With
// autoscaling, the redis StatefulSet only gets the annotation of the spec
// limit, which changes no pod.
func TestEffectiveRFKeepsTheGoldenFiles(t *testing.T) {
	for _, f := range exampleFiles(t) {
		example := strings.TrimSuffix(filepath.Base(f), ".yaml")
		for _, mode := range []string{redisfailoverv1.AutoscalingModeOff, redisfailoverv1.AutoscalingModeRecommend, redisfailoverv1.AutoscalingModeAuto} {
			t.Run(example+"/"+mode, func(t *testing.T) {
				rf := withAutoscaling(exampleRedisFailover(t, f), mode)
				spec := rf.Spec.Redis.Resources.Limits.Memory().Value()
				stored := autoscale.State{Version: autoscale.StateVersion, Effective: spec, SpecLimit: spec}
				effective := autoscale.EffectiveLimit(stored, spec, rf.Spec.Redis.Autoscaling.MemoryMax.Value())

				got := effectiveRF(rf, effective)
				if mode == redisfailoverv1.AutoscalingModeOff {
					require.Same(t, rf, got)
				}

				objects := ensuredObjects(t, got)
				dir := filepath.Join(goldenDir, example)
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Len(t, objects, len(entries))
				statefulSet := "StatefulSet-" + rfservice.GetRedisName(rf) + ".json"
				for _, e := range entries {
					want, err := os.ReadFile(filepath.Join(dir, e.Name()))
					require.NoError(t, err)
					if e.Name() != statefulSet || mode == redisfailoverv1.AutoscalingModeOff {
						assert.Equal(t, string(want), string(objects[e.Name()]), e.Name())
						continue
					}
					wantSS, gotSS := decodeStatefulSet(t, want), decodeStatefulSet(t, objects[e.Name()])
					assert.Equal(t, rf.Spec.Redis.Resources.Limits.Memory().String(), gotSS.Annotations[rfservice.SpecMemoryLimitAnnotation])
					delete(gotSS.Annotations, rfservice.SpecMemoryLimitAnnotation)
					if len(gotSS.Annotations) == 0 {
						gotSS.Annotations = wantSS.Annotations
					}
					assert.Equal(t, wantSS, gotSS)
				}
			})
		}
	}
}

// An effective limit above the spec limit changes only the resources of the
// redis container. Each other change would restart the pods instead of a
// resize in place.
func TestEffectiveRFChangesOnlyTheRedisResources(t *testing.T) {
	for _, f := range exampleFiles(t) {
		example := strings.TrimSuffix(filepath.Base(f), ".yaml")
		t.Run(example, func(t *testing.T) {
			rf := withAutoscaling(exampleRedisFailover(t, f), redisfailoverv1.AutoscalingModeRecommend)
			resources := &rf.Spec.Redis.Resources
			if _, ok := resources.Limits[corev1.ResourceMemory]; !ok {
				if resources.Limits == nil {
					resources.Limits = corev1.ResourceList{}
				}
				resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
			}
			spec := *resources.Limits.Memory()
			request, hasRequest := resources.Requests[corev1.ResourceMemory]
			effective := spec.Value() + 256*mib

			want := ensuredObjects(t, effectiveRF(rf, spec.Value()))
			got := ensuredObjects(t, effectiveRF(rf, effective))

			assert.Equal(t, spec, rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory], "the spec stays")
			require.Len(t, got, len(want))
			statefulSet := "StatefulSet-" + rfservice.GetRedisName(rf) + ".json"
			for name := range want {
				require.Contains(t, got, name)
				if name != statefulSet {
					assert.Equal(t, string(want[name]), string(got[name]), name)
					continue
				}
				wantSS, gotSS := decodeStatefulSet(t, want[name]), decodeStatefulSet(t, got[name])
				wantRes, gotRes := redisResources(t, wantSS), redisResources(t, gotSS)
				assert.Equal(t, effective, gotRes.Limits.Memory().Value())
				switch {
				case !hasRequest:
					assert.NotContains(t, gotRes.Requests, corev1.ResourceMemory)
				case request.Cmp(spec) == 0:
					assert.Equal(t, effective, gotRes.Requests.Memory().Value())
				default:
					assert.True(t, request.Equal(gotRes.Requests[corev1.ResourceMemory]), "a request below the limit stays")
				}
				assert.True(t, wantRes.Limits.Cpu().Equal(*gotRes.Limits.Cpu()))
				assert.True(t, wantRes.Requests.Cpu().Equal(*gotRes.Requests.Cpu()))
				*wantRes, *gotRes = corev1.ResourceRequirements{}, corev1.ResourceRequirements{}
				assert.Equal(t, string(canonicalJSON(t, wantSS)), string(canonicalJSON(t, gotSS)), "only the redis resources change")
			}
		})
	}
}

func decodeStatefulSet(t *testing.T, data []byte) *appsv1.StatefulSet {
	t.Helper()
	var ss appsv1.StatefulSet
	require.NoError(t, json.Unmarshal(data, &ss))
	return &ss
}

func redisResources(t *testing.T, ss *appsv1.StatefulSet) *corev1.ResourceRequirements {
	t.Helper()
	for i, c := range ss.Spec.Template.Spec.Containers {
		if c.Name == "redis" {
			return &ss.Spec.Template.Spec.Containers[i].Resources
		}
	}
	require.Fail(t, "no redis container")
	return nil
}

func TestEffectiveRF(t *testing.T) {
	newRF := func(mode string, request string) *redisfailoverv1.RedisFailover {
		rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
		rf.Spec.Redis.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		}
		if request != "" {
			rf.Spec.Redis.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(request)
		}
		if mode != "" {
			rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: mode}
		}
		return rf
	}

	for _, mode := range []string{"", redisfailoverv1.AutoscalingModeOff} {
		rf := newRF(mode, "1Gi")
		assert.Same(t, rf, effectiveRF(rf, 2*1024*mib), "mode %q", mode)
	}
	// The spec limit changes only the annotations, and not rf.
	rf := newRF(redisfailoverv1.AutoscalingModeRecommend, "1Gi")
	got := effectiveRF(rf, 1024*mib)
	assert.Equal(t, rf.Spec, got.Spec)
	assert.Equal(t, map[string]string{rfservice.SpecMemoryLimitAnnotation: "1Gi"}, got.Annotations)
	assert.Empty(t, rf.Annotations)

	tests := []struct {
		name, request, want string
	}{
		{name: "request equal to the limit", request: "1Gi", want: "1280Mi"},
		{name: "request below the limit", request: "512Mi", want: "512Mi"},
		{name: "no request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := newRF(redisfailoverv1.AutoscalingModeAuto, test.request)
			spec := rf.DeepCopy()

			got := effectiveRF(rf, 1280*mib)

			assert.Equal(t, spec, rf, "the spec stays")
			assert.Equal(t, "1280Mi", got.Spec.Redis.Resources.Limits.Memory().String())
			memory, ok := got.Spec.Redis.Resources.Requests[corev1.ResourceMemory]
			if test.want == "" {
				assert.False(t, ok)
			} else {
				assert.Equal(t, test.want, memory.String())
			}
			assert.Equal(t, "1", got.Spec.Redis.Resources.Limits.Cpu().String())
			assert.Equal(t, "100m", got.Spec.Redis.Resources.Requests.Cpu().String())
		})
	}
}

// The maxmemory readers must see the effective limit through the copy:
// maxmemory follows the smaller of the effective limit and the smallest pod.
func TestEffectiveRFReaders(t *testing.T) {
	const master, replica = "10.0.0.1", "10.0.0.2"
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: "noeviction"}
	rf.Spec.Redis.Autoscaling = &redisfailoverv1.AutoscalingSettings{Mode: redisfailoverv1.AutoscalingModeRecommend}
	pod := func(ip, limit string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "rfr-" + ip, Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "new"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "redis",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}},
			}}},
			Status: corev1.PodStatus{PodIP: ip},
		}
	}
	tests := []struct {
		name    string
		rf      *redisfailoverv1.RedisFailover
		replica string
		limit   int64
	}{
		{name: "spec", rf: rf, replica: "1280Mi", limit: 1024 * mib},
		{name: "effective", rf: effectiveRF(rf, 1280*mib), replica: "1280Mi", limit: 1280 * mib},
		{name: "effective, a replica not resized", rf: effectiveRF(rf, 1280*mib), replica: "1Gi", limit: 1024 * mib},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mk := &mK8SService.Services{}
			mk.On("GetStatefulSetPods", "testns", "rfr-test").Return(&corev1.PodList{Items: []corev1.Pod{pod(master, "1280Mi"), pod(replica, test.replica)}}, nil)
			mk.On("GetStatefulSet", "testns", "rfr-test").Return(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: "new"}}, nil)
			mr := &mRedisService.Client{}
			mr.On("GetMemoryInfo", master, "0", "").Return(&redis.MemoryInfo{MaxMemory: 512 * mib, MaxMemoryPolicy: "noeviction", Role: "master"}, nil)
			want := test.rf.MaxMemoryFor(test.limit)
			for _, ip := range []string{master, replica} {
				mr.On("SetCustomRedisConfig", ip, "0", []string{"replica-ignore-maxmemory yes"}, "").Return(nil)
				mr.On("SetCustomRedisConfig", ip, "0", []string{"maxmemory " + strconv.FormatInt(want, 10)}, "").Once().Return(nil)
				mr.On("SetCustomRedisConfig", ip, "0", []string{"maxmemory-policy noeviction"}, "").Return(nil)
			}

			_, err := rfservice.NewRedisFailoverHealer(mk, mr, log.Dummy).EnsureRedisMaxMemory(test.rf, master, []string{master, replica})

			require.NoError(t, err)
			mr.AssertExpectations(t)
		})
	}

	small := rf.DeepCopy()
	small.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("32Mi")
	require.Error(t, small.ManagedMaxMemoryError())
	assert.NoError(t, effectiveRF(small, 128*mib).ManagedMaxMemoryError())
}
