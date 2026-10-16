package service_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// TestCheckSentinelsCannotFailoverCallsAndLogs pins the order of the
// Sentinel calls and the log lines of CheckSentinelsCannotFailover.
func TestCheckSentinelsCannotFailoverCallsAndLogs(t *testing.T) {
	const gone = "10.0.0.9"
	type view struct {
		master   string
		down     bool
		replicas []string
	}
	stuck := view{master: gone, down: true}
	sentinelCalls := func(ip string, masterDown bool) []string {
		calls := []string{"GetSentinelMonitor " + ip}
		if masterDown {
			calls = append(calls, "SentinelMasterDown "+ip)
		}
		return append(calls, "GetSentinelReplicas "+ip)
	}
	notAPod := func(ip, master string, replicas int) string {
		return fmt.Sprintf("warning Sentinel %s monitors %s, which is not a pod, and knows no replica pod of %d replicas", ip, master, replicas)
	}
	tests := []struct {
		name      string
		views     []view
		want      bool
		wantCalls []string
		wantLogs  []string
	}{
		{
			name:      "no Sentinel can fail over",
			views:     []view{stuck, {master: "127.0.0.1", replicas: []string{"10.0.0.8"}}, stuck},
			want:      true,
			wantCalls: append(append(sentinelCalls("10.1.0.1", true), sentinelCalls("10.1.0.2", false)...), sentinelCalls("10.1.0.3", true)...),
			wantLogs:  []string{notAPod("10.1.0.1", gone, 0), notAPod("10.1.0.2", "127.0.0.1", 1), notAPod("10.1.0.3", gone, 0)},
		},
		{
			name:      "the second Sentinel reaches its master",
			views:     []view{stuck, {master: gone}, stuck},
			wantCalls: append(sentinelCalls("10.1.0.1", true), "GetSentinelMonitor 10.1.0.2", "SentinelMasterDown 10.1.0.2"),
			wantLogs: []string{
				notAPod("10.1.0.1", gone, 0),
				"info Sentinel 10.1.0.2 does not flag its master 10.0.0.9 as down, so the master can still answer",
			},
		},
		{
			name:      "the first Sentinel knows a replica pod",
			views:     []view{{master: gone, down: true, replicas: []string{"10.0.0.2"}}, stuck, stuck},
			wantCalls: sentinelCalls("10.1.0.1", true),
			wantLogs:  []string{"info Sentinel 10.1.0.1 knows the replica pod rfr-1 (10.0.0.2), so Sentinel can fail over"},
		},
		{
			name:      "the third Sentinel monitors a pod",
			views:     []view{stuck, stuck, {master: "10.0.0.1"}},
			wantCalls: append(append(sentinelCalls("10.1.0.1", true), sentinelCalls("10.1.0.2", true)...), "GetSentinelMonitor 10.1.0.3"),
			wantLogs: []string{
				notAPod("10.1.0.1", gone, 0), notAPod("10.1.0.2", gone, 0),
				"info Sentinel 10.1.0.3 monitors the pod rfr-0 (10.0.0.1), so Sentinel can fail over",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{
				readyPod("rfr-0", "10.0.0.1"), readyPod("rfr-1", "10.0.0.2"),
			}}, nil)
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{
				runningPod("rfs-a", "10.1.0.1"), runningPod("rfs-b", "10.1.0.2"), runningPod("rfs-c", "10.1.0.3"),
			}}, nil)
			mr := &mRedisService.Client{}
			for i, v := range test.views {
				ip := fmt.Sprintf("10.1.0.%d", i+1)
				mr.On("GetSentinelMonitor", ip).Maybe().Return(v.master, "6379", nil)
				mr.On("SentinelMasterDown", ip).Maybe().Return(v.down, nil)
				mr.On("GetSentinelReplicas", ip).Maybe().Return(v.replicas, nil)
			}
			logger, logs := newRecordingLogger()

			got, err := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy).CheckSentinelsCannotFailover(rf)

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			calls := make([]string, 0, len(mr.Calls))
			for _, c := range mr.Calls {
				calls = append(calls, fmt.Sprintf("%s %v", c.Method, c.Arguments[0]))
			}
			assert.Equal(t, test.wantCalls, calls)
			entries := logs.all()
			lines := make([]string, 0, len(entries))
			for _, e := range entries {
				lines = append(lines, e.level+" "+e.msg)
			}
			assert.Equal(t, test.wantLogs, lines)
		})
	}
}
