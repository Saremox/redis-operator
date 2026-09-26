package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	"github.com/saremox/redis-operator/service/redis"
)

const mi = int64(1 << 20)

var (
	mmMaster  = "10.0.0.1"
	mmReplica = "10.0.0.2"
	mmRedises = []string{mmMaster, mmReplica}
	// 1Gi limit at 75% -> 768Mi
	mmTarget = 768 * mi
)

func rfWithMaxMemory(policy string, customConfig ...string) *redisfailoverv1.RedisFailover {
	return &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
				CustomConfig: customConfig,
				MaxMemory:    &redisfailoverv1.MaxMemorySettings{Percent: 75, Policy: policy},
			},
		},
	}
}

// redisMock accepts replicas being set to ignore maxmemory, which managed
// maxmemory does on every reconcile.
func redisMock() *mRedisService.Client {
	mr := &mRedisService.Client{}
	mr.On("SetCustomRedisConfig", mock.Anything, "0", []string{"replica-ignore-maxmemory yes"}, "").Maybe().Return(nil)
	return mr
}

func expectSet(mr *mRedisService.Client, ip, config string) {
	mr.On("SetCustomRedisConfig", ip, "0", []string{config}, "").Once().Return(nil)
}

func maxMemoryConfig(b int64) string { return fmt.Sprintf("maxmemory %d", b) }

const updateRevision = "new"

// redisPod returns a stale redis pod with the given memory limit in its spec
// and, when not empty, the limit the kubelet reports as applied.
func redisPod(ip, specLimit, appliedLimit string) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "rfr-" + ip, Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: "old"}},
		Status:     corev1.PodStatus{PodIP: ip},
	}
	container := corev1.Container{Name: redisContainerName}
	if specLimit != "" {
		container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(specLimit)}
	}
	pod.Spec.Containers = []corev1.Container{{Name: "exporter"}, container}
	if appliedLimit != "" {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:      redisContainerName,
			Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(appliedLimit)}},
		}}
	}
	return pod
}

func upToDate(pod corev1.Pod) corev1.Pod {
	pod.Labels = map[string]string{appsv1.ControllerRevisionHashLabelKey: updateRevision}
	return pod
}

func k8sWithPods(pods ...corev1.Pod) *mK8SService.Services {
	return k8sWithStatefulSet(&appsv1.StatefulSet{Status: appsv1.StatefulSetStatus{UpdateRevision: updateRevision}}, pods...)
}

func k8sWithStatefulSet(ss *appsv1.StatefulSet, pods ...corev1.Pod) *mK8SService.Services {
	if len(pods) == 0 {
		pods = []corev1.Pod{redisPod(mmMaster, "1Gi", ""), redisPod(mmReplica, "1Gi", "")}
	}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", "testns", mock.Anything).Return(&corev1.PodList{Items: pods}, nil)
	ms.On("GetStatefulSet", "testns", mock.Anything).Return(ss, nil)
	return ms
}

func runEnsureMaxMemory(t *testing.T, rf *redisfailoverv1.RedisFailover, master string, mr *mRedisService.Client, pods ...corev1.Pod) MaxMemoryResult {
	t.Helper()
	healer := NewRedisFailoverHealer(k8sWithPods(pods...), mr, log.Dummy)
	result, err := healer.EnsureRedisMaxMemory(rf, master, mmRedises)
	assert.NoError(t, err)
	mr.AssertExpectations(t)
	return result
}

// afterLowering is the master's memory info read back after lowering maxmemory.
func afterLowering(used int64) *redis.MemoryInfo {
	return &redis.MemoryInfo{UsedMemory: used, Role: "master"}
}

func TestEnsureRedisMaxMemoryUnmanagedDoesNothing(t *testing.T) {
	rf := rfWithMaxMemory("noeviction")
	rf.Spec.Redis.MaxMemory = nil
	assert.Empty(t, runEnsureMaxMemory(t, rf, mmMaster, &mRedisService.Client{}))
}

// A spec the API server accepts but that cannot be managed is reported, not applied.
func TestEnsureRedisMaxMemoryReportsUnmanageableSpec(t *testing.T) {
	rf := rfWithMaxMemory("noeviction")
	rf.Spec.Redis.Resources.Limits = nil
	result := runEnsureMaxMemory(t, rf, mmMaster, &mRedisService.Client{})
	assert.Equal(t, MaxMemoryResult{Message: "maxmemory not managed: redis.maxMemory requires redis.resources.limits.memory"}, result)
}

func expectPolicy(mr *mRedisService.Client, policy string) {
	expectSet(mr, mmMaster, "maxmemory-policy "+policy)
	expectSet(mr, mmReplica, "maxmemory-policy "+policy)
}

func expectMemoryInfo(mr *mRedisService.Client, info *redis.MemoryInfo) {
	mr.On("GetMemoryInfo", mmMaster, "0", "").Once().Return(info, nil)
}

func withShortEvictionWait(t *testing.T) {
	w, p := evictionWait, evictionPollInterval
	evictionWait, evictionPollInterval = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { evictionWait, evictionPollInterval = w, p })
}

func TestEnsureRedisMaxMemoryRaises(t *testing.T) {
	tests := map[string]*redis.MemoryInfo{
		"above the current value": {MaxMemory: 512 * mi, UsedMemory: 600 * mi, MaxMemoryPolicy: "noeviction"},
		"already at target":       {MaxMemory: mmTarget, UsedMemory: 800 * mi, MaxMemoryPolicy: "noeviction"},
	}
	for name, info := range tests {
		t.Run(name, func(t *testing.T) {
			mr := redisMock()
			expectPolicy(mr, "noeviction")
			expectMemoryInfo(mr, info)
			expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
			expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

			assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr))
		})
	}
}

// Lowering is verified on the master before the replicas follow.
func TestEnsureRedisMaxMemoryLowers(t *testing.T) {
	withShortEvictionWait(t)
	tests := map[string]struct {
		policy  string
		current *redis.MemoryInfo
		after   []int64
	}{
		"from unlimited": {"noeviction", &redis.MemoryInfo{UsedMemory: 100 * mi}, []int64{100 * mi}},
		"when it fits":   {"noeviction", &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 700 * mi}, []int64{700 * mi}},
		"allkeys evicts": {"allkeys-lru", &redis.MemoryInfo{UsedMemory: 800 * mi}, []int64{790 * mi, 700 * mi}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			test.current.MaxMemoryPolicy = test.policy
			mr := redisMock()
			expectPolicy(mr, test.policy)
			expectMemoryInfo(mr, test.current)
			expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
			for _, used := range test.after {
				expectMemoryInfo(mr, afterLowering(used))
			}
			expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

			assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory(test.policy), mmMaster, mr))
		})
	}
}

// Without an eviction policy that can free memory, nothing is lowered and the
// replicas follow the master's current value.
func TestEnsureRedisMaxMemoryKeepsValueThatDoesNotFit(t *testing.T) {
	tests := map[string]string{
		"noeviction": "noeviction",
		// Evicting every key with a TTL might still not fit.
		"volatile-lru": "volatile-lru",
	}
	for name, policy := range tests {
		t.Run(name, func(t *testing.T) {
			mr := redisMock()
			expectPolicy(mr, policy)
			expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 800 * mi, MaxMemoryPolicy: policy})
			expectSet(mr, mmReplica, maxMemoryConfig(900*mi))

			result := runEnsureMaxMemory(t, rfWithMaxMemory(policy), mmMaster, mr)
			assert.Equal(t, "maxmemory kept at 900.0Mi: lowering it to 768.0Mi would not fit the memory in use under policy "+policy, result.Message)
		})
	}
}

// A lowered value that turns out not to fit is restored on the master.
func TestEnsureRedisMaxMemoryRestoresValueThatDoesNotFit(t *testing.T) {
	mr := redisMock()
	expectPolicy(mr, "noeviction")
	expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 700 * mi, MaxMemoryPolicy: "noeviction"})
	expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
	expectMemoryInfo(mr, afterLowering(790*mi))
	expectSet(mr, mmMaster, maxMemoryConfig(900*mi))
	expectSet(mr, mmReplica, maxMemoryConfig(900*mi))

	result := runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr)
	assert.Equal(t, "maxmemory kept at 900.0Mi: lowering it to 768.0Mi would not fit the memory in use under policy noeviction", result.Message)
}

// Evicting a large dataset can take longer than the wait; the lowered value
// stays and the replicas follow it.
func TestEnsureRedisMaxMemoryKeepsEvictingDownToTheTarget(t *testing.T) {
	withShortEvictionWait(t)
	const evicting = "maxmemory lowered to 768.0Mi, evicting down to it"
	t.Run("while lowering", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "allkeys-lru")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 800 * mi, MaxMemoryPolicy: "allkeys-lru"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		mr.On("GetMemoryInfo", mmMaster, "0", "").Return(afterLowering(790*mi), nil)
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Equal(t, MaxMemoryResult{Message: evicting}, runEnsureMaxMemory(t, rfWithMaxMemory("allkeys-lru"), mmMaster, mr))
	})

	t.Run("at the target", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 790 * mi, MaxMemoryPolicy: "allkeys-lru"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))
		expectPolicy(mr, "allkeys-lru")

		assert.Equal(t, MaxMemoryResult{Message: evicting}, runEnsureMaxMemory(t, rfWithMaxMemory("allkeys-lru"), mmMaster, mr))
	})
}

func TestEnsureRedisMaxMemoryCustomConfigTakesPrecedence(t *testing.T) {
	t.Run("maxmemory", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction", "MaxMemory 100mb"), mmMaster, mr))
	})

	t.Run("maxmemory-policy", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi, MaxMemoryPolicy: "allkeys-lru"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction", "maxmemory-policy allkeys-lru", "maxmemory-samples 10"), mmMaster, mr))
	})
}

func TestEnsureRedisMaxMemoryWithoutMaster(t *testing.T) {
	unreachable := errors.New("dial tcp 10.0.0.2:6379: connect: connection refused")

	t.Run("replicas never evict", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "allkeys-lru")
		expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		mr.On("GetMemoryInfo", mmReplica, "0", "").Once().Return(&redis.MemoryInfo{UsedMemory: 800 * mi}, nil)

		result := runEnsureMaxMemory(t, rfWithMaxMemory("allkeys-lru"), "", mr)
		assert.Equal(t, "maxmemory kept at unlimited: lowering it to 768.0Mi would not fit the memory in use on 10.0.0.2", result.Message)
	})

	t.Run("unreachable redises are skipped", func(t *testing.T) {
		mr := redisMock()
		expectSet(mr, mmMaster, "maxmemory-policy noeviction")
		mr.On("SetCustomRedisConfig", mmReplica, "0", []string{"maxmemory-policy noeviction"}, "").Once().Return(unreachable)
		expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 100 * mi})
		mr.On("GetMemoryInfo", mmReplica, "0", "").Once().Return(nil, unreachable)
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), "", mr))
	})

	t.Run("pods without a known limit are skipped", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))

		pending := redisPod("", "1Gi", "")
		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), "", mr, redisPod(mmMaster, "1Gi", ""), redisPod(mmReplica, "32Mi", ""), pending))
	})

	t.Run("errors", func(t *testing.T) {
		fail := errors.New("ERR boom")
		tests := map[string]func(mr *mRedisService.Client){
			"setting the policy": func(mr *mRedisService.Client) {
				mr.On("SetCustomRedisConfig", mmMaster, "0", []string{"maxmemory-policy noeviction"}, "").Once().Return(fail)
			},
			"reading the memory": func(mr *mRedisService.Client) {
				expectPolicy(mr, "noeviction")
				mr.On("GetMemoryInfo", mmMaster, "0", "").Once().Return(nil, fail)
			},
			"setting maxmemory": func(mr *mRedisService.Client) {
				expectPolicy(mr, "noeviction")
				expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 100 * mi})
				mr.On("SetCustomRedisConfig", mmMaster, "0", []string{maxMemoryConfig(mmTarget)}, "").Once().Return(fail)
			},
		}
		for name, setup := range tests {
			t.Run(name, func(t *testing.T) {
				mr := redisMock()
				setup(mr)
				healer := NewRedisFailoverHealer(k8sWithPods(), mr, log.Dummy)
				_, err := healer.EnsureRedisMaxMemory(rfWithMaxMemory("noeviction"), "", mmRedises)
				assert.ErrorIs(t, err, fail)
				mr.AssertExpectations(t)
			})
		}
	})
}

func TestEnsureRedisMaxMemoryErrors(t *testing.T) {
	withShortEvictionWait(t)
	fail := errors.New("ERR boom")
	evicting := &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 800 * mi, MaxMemoryPolicy: "allkeys-lru"}
	tests := map[string]func(mr *mRedisService.Client){
		"setting the policy": func(mr *mRedisService.Client) {
			expectMemoryInfo(mr, evicting)
			mr.On("SetCustomRedisConfig", mmMaster, "0", []string{"maxmemory-policy allkeys-lru"}, "").Once().Return(fail)
		},
		"reading the master's memory": func(mr *mRedisService.Client) {
			mr.On("GetMemoryInfo", mmMaster, "0", "").Once().Return(nil, fail)
		},
		"raising": func(mr *mRedisService.Client) {
			expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi})
			mr.On("SetCustomRedisConfig", mmMaster, "0", []string{maxMemoryConfig(mmTarget)}, "").Once().Return(fail)
		},
		"setting the policy after raising": func(mr *mRedisService.Client) {
			expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi})
			expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
			expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))
			mr.On("SetCustomRedisConfig", mmMaster, "0", []string{"maxmemory-policy allkeys-lru"}, "").Once().Return(fail)
		},
		// The previous value is restored whenever the attempt fails.
		"lowering the master": func(mr *mRedisService.Client) {
			expectPolicy(mr, "allkeys-lru")
			expectMemoryInfo(mr, evicting)
			mr.On("SetCustomRedisConfig", mmMaster, "0", []string{maxMemoryConfig(mmTarget)}, "").Once().Return(fail)
			expectSet(mr, mmMaster, maxMemoryConfig(900*mi))
		},
		"verifying the lowered value": func(mr *mRedisService.Client) {
			expectPolicy(mr, "allkeys-lru")
			expectMemoryInfo(mr, evicting)
			expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
			mr.On("GetMemoryInfo", mmMaster, "0", "").Once().Return(nil, fail)
			expectSet(mr, mmMaster, maxMemoryConfig(900*mi))
		},
		"lowering the replicas": func(mr *mRedisService.Client) {
			expectPolicy(mr, "allkeys-lru")
			expectMemoryInfo(mr, evicting)
			expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
			expectMemoryInfo(mr, afterLowering(700*mi))
			mr.On("SetCustomRedisConfig", mmReplica, "0", []string{maxMemoryConfig(mmTarget)}, "").Once().Return(fail)
		},
		"restoring the master": func(mr *mRedisService.Client) {
			expectPolicy(mr, "allkeys-lru")
			expectMemoryInfo(mr, evicting)
			expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
			expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 700 * mi, Role: "slave"})
			mr.On("SetCustomRedisConfig", mmMaster, "0", []string{maxMemoryConfig(900 * mi)}, "").Once().Return(fail)
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			mr := redisMock()
			setup(mr)
			healer := NewRedisFailoverHealer(k8sWithPods(), mr, log.Dummy)
			_, err := healer.EnsureRedisMaxMemory(rfWithMaxMemory("allkeys-lru"), mmMaster, mmRedises)
			assert.ErrorIs(t, err, fail)
			mr.AssertExpectations(t)
		})
	}

	t.Run("listing the pods", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetStatefulSetPods", "testns", mock.Anything).Once().Return(nil, fail)
		healer := NewRedisFailoverHealer(ms, redisMock(), log.Dummy)
		_, err := healer.EnsureRedisMaxMemory(rfWithMaxMemory("noeviction"), mmMaster, mmRedises)
		assert.ErrorIs(t, err, fail)
	})

	t.Run("reading the statefulset", func(t *testing.T) {
		ms := &mK8SService.Services{}
		ms.On("GetStatefulSetPods", "testns", mock.Anything).Once().Return(&corev1.PodList{}, nil)
		ms.On("GetStatefulSet", "testns", mock.Anything).Once().Return(nil, fail)
		healer := NewRedisFailoverHealer(ms, redisMock(), log.Dummy)
		_, err := healer.EnsureRedisMaxMemory(rfWithMaxMemory("noeviction"), mmMaster, mmRedises)
		assert.ErrorIs(t, err, fail)
	})

	t.Run("reading the password", func(t *testing.T) {
		rf := rfWithMaxMemory("noeviction")
		rf.Spec.Auth.SecretPath = "redis-auth"
		ms := &mK8SService.Services{}
		ms.On("GetSecret", "testns", "redis-auth").Once().Return(nil, fail)
		healer := NewRedisFailoverHealer(ms, redisMock(), log.Dummy)
		_, err := healer.EnsureRedisMaxMemory(rf, mmMaster, mmRedises)
		assert.ErrorIs(t, err, fail)
	})
}

// maxmemory follows the limit of the running master, not the spec: with
// OnDelete the master keeps its old container until it is replaced.
func TestEnsureRedisMaxMemoryFollowsTheRunningMasterLimit(t *testing.T) {
	tests := map[string]corev1.Pod{
		"old limit in the pod spec":             redisPod(mmMaster, "512Mi", ""),
		"applied limit preferred over the spec": redisPod(mmMaster, "1Gi", "512Mi"),
	}
	for name, master := range tests {
		t.Run(name, func(t *testing.T) {
			mr := redisMock()
			expectPolicy(mr, "noeviction")
			expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 256 * mi, UsedMemory: 100 * mi})
			// 512Mi at 75%, not the 768Mi the 1Gi spec would give.
			expectSet(mr, mmMaster, maxMemoryConfig(384*mi))
			expectSet(mr, mmReplica, maxMemoryConfig(384*mi))

			assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr, master, redisPod(mmReplica, "1Gi", "")))
		})
	}

	// A lowered limit applies before the smaller replicas are rolled out.
	t.Run("lowered spec limit", func(t *testing.T) {
		withShortEvictionWait(t)
		rf := rfWithMaxMemory("noeviction")
		rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(384*mi))
		expectMemoryInfo(mr, afterLowering(100*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(384*mi))

		assert.Empty(t, runEnsureMaxMemory(t, rf, mmMaster, mr))
	})

	t.Run("master still below the managed minimum", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr, redisPod(mmMaster, "32Mi", ""), redisPod(mmReplica, "1Gi", "")))
	})

	// Without a limit the pod is about to get the configured one.
	t.Run("master without a limit", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi, UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))
		expectPolicy(mr, "noeviction")

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr, redisPod(mmMaster, "", ""), redisPod(mmReplica, "1Gi", "")))
	})
}

// When raising, maxmemory goes up before an evicting policy is enabled.
func TestEnsureRedisMaxMemoryRaisesBeforeChangingThePolicy(t *testing.T) {
	mr := redisMock()
	expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 512 * mi, UsedMemory: 500 * mi, MaxMemoryPolicy: "noeviction"})
	expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
	expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))
	expectPolicy(mr, "allkeys-lru")

	assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("allkeys-lru"), mmMaster, mr))

	var order []string
	for _, call := range mr.Calls {
		if call.Method == "SetCustomRedisConfig" && call.Arguments.Get(2).([]string)[0] != "replica-ignore-maxmemory yes" {
			order = append(order, call.Arguments.Get(2).([]string)[0])
		}
	}
	assert.Equal(t, []string{
		maxMemoryConfig(mmTarget), maxMemoryConfig(mmTarget),
		"maxmemory-policy allkeys-lru", "maxmemory-policy allkeys-lru",
	}, order)
}

// When lowering, the new policy is in place before it is relied on to evict.
func TestEnsureRedisMaxMemoryChangesThePolicyBeforeLowering(t *testing.T) {
	withShortEvictionWait(t)
	mr := redisMock()
	expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 800 * mi, MaxMemoryPolicy: "noeviction"})
	expectPolicy(mr, "allkeys-lru")
	expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
	expectMemoryInfo(mr, afterLowering(700*mi))
	expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

	assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("allkeys-lru"), mmMaster, mr))
	var sets []string
	for _, call := range mr.Calls {
		if call.Method == "SetCustomRedisConfig" {
			sets = append(sets, call.Arguments.Get(2).([]string)[0])
		}
	}
	assert.Equal(t, []string{
		"replica-ignore-maxmemory yes", "replica-ignore-maxmemory yes",
		"maxmemory-policy allkeys-lru", "maxmemory-policy allkeys-lru",
		maxMemoryConfig(mmTarget), maxMemoryConfig(mmTarget),
	}, sets)
}

// While pods are about to be replaced with a smaller limit, a master that
// cannot be brought down to fit it holds the rollout.
func TestEnsureRedisMaxMemoryHoldsTheRolloutOfALoweredLimit(t *testing.T) {
	withShortEvictionWait(t)
	lowered := func() *redisfailoverv1.RedisFailover {
		rf := rfWithMaxMemory("noeviction")
		rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
		return rf
	}

	t.Run("dataset does not fit", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 500 * mi})
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		result := runEnsureMaxMemory(t, lowered(), mmMaster, mr)
		assert.True(t, result.HoldRollout)
		assert.Contains(t, result.Message, "would not fit")
	})

	t.Run("master changed while lowering", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(384*mi))
		expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 100 * mi, Role: "slave"})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))

		result := runEnsureMaxMemory(t, lowered(), mmMaster, mr)
		assert.True(t, result.HoldRollout)
		assert.Equal(t, "maxmemory kept at 768.0Mi: 10.0.0.1 stopped being the master while lowering it", result.Message)
	})

	t.Run("master below the managed minimum", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")

		// Holding would keep the master from ever being replaced.
		result := runEnsureMaxMemory(t, lowered(), mmMaster, mr, redisPod(mmMaster, "32Mi", ""), redisPod(mmReplica, "1Gi", ""))
		assert.False(t, result.HoldRollout)
	})

	t.Run("still evicting at the target", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 384 * mi, UsedMemory: 400 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(384*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(384*mi))
		expectPolicy(mr, "allkeys-lru")

		rf := lowered()
		rf.Spec.Redis.MaxMemory.Policy = "allkeys-lru"
		assert.True(t, runEnsureMaxMemory(t, rf, mmMaster, mr).HoldRollout)
	})

	t.Run("without a master", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 500 * mi})
		mr.On("GetMemoryInfo", mmReplica, "0", "").Once().Return(&redis.MemoryInfo{MaxMemory: 384 * mi, UsedMemory: 100 * mi}, nil)
		expectSet(mr, mmReplica, maxMemoryConfig(384*mi))

		assert.True(t, runEnsureMaxMemory(t, lowered(), "", mr).HoldRollout)
	})

	t.Run("not while the limit stays", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 900 * mi, UsedMemory: 800 * mi})
		expectSet(mr, mmReplica, maxMemoryConfig(900*mi))

		result := runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr)
		assert.False(t, result.HoldRollout)
		assert.NotEmpty(t, result.Message)
	})

	// Under noeviction, a full master overshoots maxmemory by the last write.
	t.Run("not for a full master at the target", func(t *testing.T) {
		mr := redisMock()
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 384 * mi, UsedMemory: 385 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(384*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(384*mi))
		expectPolicy(mr, "noeviction")

		assert.Empty(t, runEnsureMaxMemory(t, lowered(), mmMaster, mr))
	})
}

// Replicas ignore maxmemory and hold the master's whole dataset, and any of
// them can be promoted, so the smallest container sets maxmemory.
func TestEnsureRedisMaxMemoryFollowsTheSmallestPod(t *testing.T) {
	t.Run("replica still on the old limit", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: 384 * mi, UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(384*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(384*mi))

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr, upToDate(redisPod(mmMaster, "1Gi", "")), redisPod(mmReplica, "1Gi", "512Mi")))
	})

	t.Run("replica below the managed minimum", func(t *testing.T) {
		mr := redisMock()
		expectPolicy(mr, "noeviction")

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr, upToDate(redisPod(mmMaster, "1Gi", "")), redisPod(mmReplica, "32Mi", "")))
	})
}

// A restarted redis runs without maxmemory; one that does not fit the target
// is capped instead of being left unlimited.
func TestEnsureRedisMaxMemoryCapsAnUnlimitedMaster(t *testing.T) {
	tests := map[string]struct{ used, capped int64 }{
		// 1Gi less the 32Mi reserve.
		"at the reserve":       {800 * mi, 992 * mi},
		"at the memory in use": {1000 * mi, 1000 * mi},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			mr := redisMock()
			expectPolicy(mr, "noeviction")
			expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: test.used})
			expectSet(mr, mmMaster, maxMemoryConfig(test.capped))
			expectSet(mr, mmReplica, maxMemoryConfig(test.capped))

			result := runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr)
			assert.Contains(t, result.Message, "maxmemory kept at "+formatBytes(test.capped))
		})
	}

	t.Run("by the running container during a downsize", func(t *testing.T) {
		rf := rfWithMaxMemory("noeviction")
		rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 500 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(992*mi))
		expectSet(mr, mmReplica, maxMemoryConfig(992*mi))

		assert.True(t, runEnsureMaxMemory(t, rf, mmMaster, mr).HoldRollout)
	})

	t.Run("error", func(t *testing.T) {
		fail := errors.New("ERR boom")
		mr := redisMock()
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{UsedMemory: 800 * mi})
		mr.On("SetCustomRedisConfig", mmMaster, "0", []string{maxMemoryConfig(992 * mi)}, "").Once().Return(fail)
		_, err := NewRedisFailoverHealer(k8sWithPods(), mr, log.Dummy).EnsureRedisMaxMemory(rfWithMaxMemory("noeviction"), mmMaster, mmRedises)
		assert.ErrorIs(t, err, fail)
	})
}

// Only stale pods are replaced: an up-to-date larger container, e.g. resized
// by a VPA, does not hold the rollout.
func TestEnsureRedisMaxMemoryDoesNotHoldForUpToDatePods(t *testing.T) {
	rf := rfWithMaxMemory("noeviction")
	rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
	mr := redisMock()
	expectPolicy(mr, "noeviction")
	expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 500 * mi})
	expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

	result := runEnsureMaxMemory(t, rf, mmMaster, mr, upToDate(redisPod(mmMaster, "1Gi", "")), upToDate(redisPod(mmReplica, "", "")))
	assert.False(t, result.HoldRollout)
	assert.Contains(t, result.Message, "would not fit")
}

// Until the controller observes a lowered limit, its update revision is the
// previous one, which the pods may still be on.
func TestEnsureRedisMaxMemoryHoldsBeforeTheRevisionIsUpdated(t *testing.T) {
	rf := rfWithMaxMemory("noeviction")
	rf.Spec.Redis.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
	mr := redisMock()
	expectPolicy(mr, "noeviction")
	expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 500 * mi})
	expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

	ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, UpdateRevision: updateRevision}}
	healer := NewRedisFailoverHealer(k8sWithStatefulSet(ss, upToDate(redisPod(mmMaster, "1Gi", "")), upToDate(redisPod(mmReplica, "1Gi", ""))), mr, log.Dummy)
	result, err := healer.EnsureRedisMaxMemory(rf, mmMaster, mmRedises)
	assert.NoError(t, err)
	assert.True(t, result.HoldRollout)
	mr.AssertExpectations(t)
}

// Removing "replica-ignore-maxmemory no" from customConfig does not reset it
// on running replicas, so managed maxmemory resets it on every reconcile.
func TestEnsureRedisMaxMemoryMakesReplicasIgnoreMaxMemory(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		mr := &mRedisService.Client{}
		expectSet(mr, mmMaster, "replica-ignore-maxmemory yes")
		expectSet(mr, mmReplica, "replica-ignore-maxmemory yes")
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction"), mmMaster, mr))
	})

	t.Run("with maxmemory in customConfig", func(t *testing.T) {
		mr := &mRedisService.Client{}
		expectSet(mr, mmMaster, "replica-ignore-maxmemory yes")
		expectSet(mr, mmReplica, "replica-ignore-maxmemory yes")
		expectPolicy(mr, "noeviction")

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction", "maxmemory 100mb"), mmMaster, mr))
	})

	t.Run("also when customConfig sets it", func(t *testing.T) {
		mr := &mRedisService.Client{}
		expectSet(mr, mmMaster, "replica-ignore-maxmemory yes")
		expectSet(mr, mmReplica, "replica-ignore-maxmemory yes")
		expectPolicy(mr, "noeviction")
		expectMemoryInfo(mr, &redis.MemoryInfo{MaxMemory: mmTarget, UsedMemory: 100 * mi})
		expectSet(mr, mmMaster, maxMemoryConfig(mmTarget))
		expectSet(mr, mmReplica, maxMemoryConfig(mmTarget))

		assert.Empty(t, runEnsureMaxMemory(t, rfWithMaxMemory("noeviction", "slave-ignore-maxmemory yes"), mmMaster, mr))
	})

	t.Run("error", func(t *testing.T) {
		fail := errors.New("ERR boom")
		mr := &mRedisService.Client{}
		mr.On("SetCustomRedisConfig", mmMaster, "0", []string{"replica-ignore-maxmemory yes"}, "").Once().Return(fail)
		_, err := NewRedisFailoverHealer(k8sWithPods(), mr, log.Dummy).EnsureRedisMaxMemory(rfWithMaxMemory("noeviction"), mmMaster, mmRedises)
		assert.ErrorIs(t, err, fail)
	})
}
