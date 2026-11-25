package service_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// errorOf runs a checker call and keeps only its error.
type errorOf func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error

func masterIPError(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
	_, err := c.GetMasterIP(rf)
	return err
}

// A checker call returns the error of the pod list. It does not answer from
// an empty list, because that would hide an API outage as "no master".
func TestCheckerReturnsThePodListError(t *testing.T) {
	calls := map[string]errorOf{
		"GetMasterIP": masterIPError,
		"GetNumberMasters": func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetNumberMasters(rf)
			return err
		},
		"GetMaxRedisPodTime": func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetMaxRedisPodTime(rf)
			return err
		},
		"GetRedisesMasterPod": func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetRedisesMasterPod(rf)
			return err
		},
		"GetRedisesSlavesPods": func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetRedisesSlavesPods(rf)
			return err
		},
		"GetReplicaReplicationOffsets": func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetReplicaReplicationOffsets(rf)
			return err
		},
	}
	listErr := errors.New("list err")
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			rf := generateRF()
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(nil, listErr)
			checker := rfservice.NewRedisFailoverChecker(ms, &mRedisService.Client{}, log.DummyLogger{}, metrics.Dummy)

			require.ErrorIs(t, call(checker, rf), listErr)
		})
	}
}

// A checker call returns the error of the password Secret, and sends no
// command to Redis without the password.
func TestCheckerReturnsThePasswordError(t *testing.T) {
	onePod := &corev1.PodList{Items: []corev1.Pod{{Status: corev1.PodStatus{PodIP: "1.1.1.1", Phase: corev1.PodRunning}}}}
	tests := []struct {
		name string
		// pods is the answer of the pod list, or nil when the call reads no pods.
		pods *corev1.PodList
		call errorOf
	}{
		{"GetRedisesMasterPod", onePod, func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetRedisesMasterPod(rf)
			return err
		}},
		{"GetRedisesSlavesPods", onePod, func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetRedisesSlavesPods(rf)
			return err
		}},
		{"CheckIfMasterLocalhost", onePod, func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.CheckIfMasterLocalhost(rf)
			return err
		}},
		{"GetReplicaReplicationOffsets", &corev1.PodList{}, func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.GetReplicaReplicationOffsets(rf)
			return err
		}},
		{"CheckRedisSlavesReady", nil, func(c *rfservice.RedisFailoverChecker, rf *redisfailoverv1.RedisFailover) error {
			_, err := c.CheckRedisSlavesReady("1.1.1.1", rf)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Auth.SecretPath = "redis-secret"
			secretErr := errors.New("secret unavailable")
			ms := &mK8SService.Services{}
			if test.pods != nil {
				ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(test.pods, nil)
			}
			ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, secretErr)
			mr := &mRedisService.Client{}
			checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)

			require.ErrorIs(t, test.call(checker, rf), secretErr)
			mr.AssertExpectations(t)
		})
	}
}
