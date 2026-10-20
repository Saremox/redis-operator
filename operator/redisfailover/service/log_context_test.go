package service_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/service/redis"
)

type logEntry struct {
	level  string
	fields map[string]any
	msg    string
}

type recordedLogs struct {
	mu      sync.Mutex
	entries []logEntry
}

func (r *recordedLogs) all() []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]logEntry(nil), r.entries...)
}

// recordingLogger records the level, the fields and the text of each line.
type recordingLogger struct {
	log.DummyLogger
	logs   *recordedLogs
	fields map[string]any
}

func newRecordingLogger() (recordingLogger, *recordedLogs) {
	logs := &recordedLogs{}
	return recordingLogger{logs: logs, fields: map[string]any{}}, logs
}

func (l recordingLogger) WithFields(values map[string]any) log.Logger {
	fields := make(map[string]any, len(l.fields)+len(values))
	maps.Copy(fields, l.fields)
	maps.Copy(fields, values)
	l.fields = fields
	return l
}

func (l recordingLogger) WithField(key string, value any) log.Logger {
	return l.WithFields(map[string]any{key: value})
}

func (l recordingLogger) With(key string, value any) log.Logger {
	return l.WithField(key, value)
}

func (l recordingLogger) add(level, msg string) {
	l.logs.mu.Lock()
	defer l.logs.mu.Unlock()
	l.logs.entries = append(l.logs.entries, logEntry{level: level, fields: l.fields, msg: msg})
}

func (l recordingLogger) Debugf(format string, args ...any) {
	l.add("debug", fmt.Sprintf(format, args...))
}

func (l recordingLogger) Info(args ...any) {
	l.add("info", fmt.Sprint(args...))
}

func (l recordingLogger) Infof(format string, args ...any) {
	l.add("info", fmt.Sprintf(format, args...))
}

func (l recordingLogger) Warnf(format string, args ...any) {
	l.add("warning", fmt.Sprintf(format, args...))
}

func (l recordingLogger) Warningf(format string, args ...any) {
	l.add("warning", fmt.Sprintf(format, args...))
}

func (l recordingLogger) Error(args ...any) {
	l.add("error", fmt.Sprint(args...))
}

func (l recordingLogger) Errorf(format string, args ...any) {
	l.add("error", fmt.Sprintf(format, args...))
}

// assertRFFields asserts the fields that a Loki query uses to find the lines
// of one RedisFailover.
func assertRFFields(t *testing.T, entries []logEntry) {
	t.Helper()
	for _, e := range entries {
		assert.Equal(t, name, e.fields["redisfailover"], e.msg)
		assert.Equal(t, namespace, e.fields["namespace"], e.msg)
	}
}

func readyPod(name, ip string) corev1.Pod {
	pod := runningPod(name, ip)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return pod
}

func TestCheckAllSlavesFromMasterLogContext(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		readyPod("rfr-0", "10.0.0.1"),
		runningPod("rfr-1", "10.0.0.2"),
		readyPod("rfr-2", "10.0.0.3"),
		readyPod("rfr-3", "10.0.0.4"),
		readyPod("rfr-4", "10.0.0.5"),
		readyPod("rfr-5", ""),
	}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, mock.Anything, mock.Anything).Return(nil)
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "10.0.0.1", "0", "").Once().Return("", nil)
	mr.On("GetSlaveOf", "10.0.0.2", "0", "").Once().Return("", errors.New("connection refused"))
	mr.On("GetSlaveOf", "10.0.0.3", "0", "").Once().Return("", errors.New("i/o timeout"))
	mr.On("GetSlaveOf", "10.0.0.4", "0", "").Once().Return("127.0.0.1", nil)
	mr.On("GetSlaveOf", "10.0.0.5", "0", "").Once().Return("10.0.0.9", nil)
	logger, logs := newRecordingLogger()

	checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
	err := checker.CheckAllSlavesFromMaster("10.0.0.1", rf)

	// The caller logs the returned error. A different master comes first,
	// because it can be a second master.
	require.EqualError(t, err, "slave 10.0.0.5 (pod rfr-4) don't have the master 10.0.0.1, has 10.0.0.9")
	entries := logs.all()
	assertRFFields(t, entries)
	// A pod that is not Ready usually restarts.
	assert.Equal(t, []string{"debug rfr-1", "warning rfr-2", "debug rfr-5", "info rfr-3"}, levelsAndPods(entries))
	assert.Contains(t, entries[0].msg, "connection refused")
	assert.Contains(t, entries[1].msg, "i/o timeout")
	assert.Contains(t, entries[3].msg, "has 127.0.0.1")
}

func levelsAndPods(entries []logEntry) []string {
	var got []string
	for _, e := range entries {
		got = append(got, fmt.Sprintf("%s %v", e.level, e.fields["pod"]))
	}
	return got
}

// Each replica with a wrong master is in the returned error or in one log
// line, never in both.
func TestCheckAllSlavesFromMasterLogsEachWrongMasterOnce(t *testing.T) {
	tests := []struct {
		name    string
		masters []string
		expErr  string
		expPod  string
		expLogs []string
	}{
		{
			name:    "only new replicas",
			masters: []string{"127.0.0.1", "127.0.0.1"},
			expErr:  "slave 10.0.0.2 (pod rfr-1) don't have the master 10.0.0.1, has 127.0.0.1",
			expPod:  "rfr-1",
			expLogs: []string{"info rfr-2"},
		},
		{
			name:    "two different masters after a new replica",
			masters: []string{"127.0.0.1", "10.0.0.9", "10.0.0.8"},
			expErr:  "slave 10.0.0.3 (pod rfr-2) don't have the master 10.0.0.1, has 10.0.0.9",
			expPod:  "rfr-2",
			expLogs: []string{"info rfr-1", "warning rfr-3"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			pods := &corev1.PodList{Items: []corev1.Pod{readyPod("rfr-0", "10.0.0.1")}}
			mr := &mRedisService.Client{}
			mr.On("GetSlaveOf", "10.0.0.1", "0", "").Once().Return("", nil)
			for i, master := range test.masters {
				ip := fmt.Sprintf("10.0.0.%d", i+2)
				pods.Items = append(pods.Items, readyPod(fmt.Sprintf("rfr-%d", i+1), ip))
				mr.On("GetSlaveOf", ip, "0", "").Once().Return(master, nil)
			}
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			ms.On("UpdatePodLabels", namespace, mock.Anything, mock.Anything).Return(nil)
			logger, logs := newRecordingLogger()

			checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
			err := checker.CheckAllSlavesFromMaster("10.0.0.1", rf)

			require.EqualError(t, err, test.expErr)
			// The caller logs the pod as a field.
			var wrong *rfservice.WrongMasterError
			require.ErrorAs(t, err, &wrong)
			assert.Equal(t, test.expPod, wrong.Pod)
			assert.Equal(t, test.expLogs, levelsAndPods(logs.all()))
			assertRFFields(t, logs.all())
		})
	}
}

// A failed label update returns at once, so the replicas with a wrong
// master found before it need their own lines.
func TestCheckAllSlavesFromMasterLogsTheWrongMastersBeforeALabelError(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{
		readyPod("rfr-0", "10.0.0.1"),
		readyPod("rfr-1", "10.0.0.2"),
		readyPod("rfr-2", "10.0.0.3"),
	}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
	ms.On("UpdatePodLabels", namespace, "rfr-0", mock.Anything).Return(nil)
	ms.On("UpdatePodLabels", namespace, "rfr-1", mock.Anything).Return(nil)
	ms.On("UpdatePodLabels", namespace, "rfr-2", mock.Anything).Return(errors.New("forbidden"))
	mr := &mRedisService.Client{}
	mr.On("GetSlaveOf", "10.0.0.1", "0", "").Once().Return("", nil)
	mr.On("GetSlaveOf", "10.0.0.2", "0", "").Once().Return("10.0.0.9", nil)
	logger, logs := newRecordingLogger()

	checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
	err := checker.CheckAllSlavesFromMaster("10.0.0.1", rf)

	require.EqualError(t, err, "forbidden")
	assert.Equal(t, []string{"warning rfr-1"}, levelsAndPods(logs.all()))
	assertRFFields(t, logs.all())
}

func TestCheckSentinelQuorumLeavesTheLogToTheCaller(t *testing.T) {
	sentinels := &corev1.PodList{Items: []corev1.Pod{runningPod("rfs-0", "1.1.1.1"), runningPod("rfs-1", "2.2.2.2")}}
	tests := []struct {
		name     string
		replicas int32
		ckquorum error
	}{
		{name: "too few Sentinels run", replicas: 5},
		{name: "too many Sentinels fail CKQUORUM", replicas: 3, ckquorum: errors.New("NOQUORUM")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Sentinel.Replicas = test.replicas
			ms := &mK8SService.Services{}
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(sentinels, nil)
			mr := &mRedisService.Client{}
			mr.On("SentinelCheckQuorum", mock.Anything).Return(test.ckquorum)
			logger, logs := newRecordingLogger()

			checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
			_, err := checker.CheckSentinelQuorum(rf)

			require.EqualError(t, err, "insufficient sentinels to reach the quorum")
			assert.Empty(t, logs.all())
		})
	}
}

func TestCheckIfMasterLocalhostLogContext(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{runningPod("rfr-0", "10.0.0.1")}}
	tests := []struct {
		name   string
		master string
		err    error
		expErr string
	}{
		{name: "a pod is a master", master: "", expErr: "unexpected master state: redis 10.0.0.1 is a master"},
		{name: "a pod refuses the command", err: errors.New("LOADING Redis is loading the dataset in memory"), expErr: "redis 10.0.0.1: LOADING Redis is loading the dataset in memory"},
		{name: "each pod has localhost as master", master: "127.0.0.1"},
		{name: "a pod has a different master", master: "10.0.0.9"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
			mr := &mRedisService.Client{}
			mr.On("GetSlaveOf", "10.0.0.1", "0", "").Once().Return(test.master, test.err)
			logger, logs := newRecordingLogger()

			checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
			_, err := checker.CheckIfMasterLocalhost(rf)

			entries := logs.all()
			if test.expErr != "" {
				// The caller logs the error, so the function does not.
				require.EqualError(t, err, test.expErr)
				assert.Empty(t, entries)
				return
			}
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, "info", entries[0].level)
			assertRFFields(t, entries)
		})
	}
}

func TestGetMasterIPAndGetNumberMastersLogAWarning(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{readyPod("rfr-0", "10.0.0.1")}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "10.0.0.1", "0", "").Return(false, errors.New("connection refused"))
	logger, logs := newRecordingLogger()
	checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)

	_, err := checker.GetMasterIP(rf)
	require.ErrorIs(t, err, rfservice.ErrAmbiguousMasterCount)
	_, err = checker.GetNumberMasters(rf)
	require.ErrorIs(t, err, rfservice.ErrRedisNotAnswering)

	entries := logs.all()
	require.Len(t, entries, 2)
	assertRFFields(t, entries)
	for _, e := range entries {
		assert.Equal(t, "warning", e.level)
		assert.Contains(t, e.msg, "connection refused")
	}
	assert.Equal(t, "rfr-0", entries[0].fields["pod"])
	assert.Equal(t, "rfr-0", entries[1].fields["pod"])
}

// A pod that is not Ready usually restarts, so each reconcile finds it.
func TestGetNumberMastersAndGetMasterIPLogAPodThatIsNotReadyAtDebug(t *testing.T) {
	rf := generateRF()
	pods := &corev1.PodList{Items: []corev1.Pod{runningPod("rfr-0", "10.0.0.1")}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "10.0.0.1", "0", "").Return(false, errors.New("connection refused"))
	logger, logs := newRecordingLogger()

	checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
	_, err := checker.GetNumberMasters(rf)
	require.NoError(t, err)
	_, err = checker.GetMasterIP(rf)
	require.ErrorIs(t, err, rfservice.ErrAmbiguousMasterCount)

	assert.Equal(t, []string{"debug rfr-0", "debug rfr-0"}, levelsAndPods(logs.all()))
	assertRFFields(t, logs.all())
}

// replyError is an error reply of Redis. The client wraps it with the command.
type replyError string

func (e replyError) Error() string { return string(e) }

func (replyError) RedisError() {}

func TestSetOldestAsMasterLogLevels(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expLevel string
	}{
		{name: "a pod that does not answer", err: errors.New("dial tcp 10.0.0.1:6379: connect: connection refused"), expLevel: "warning"},
		{name: "a pod that loads its dataset", err: errors.New("LOADING Redis is loading the dataset in memory"), expLevel: "warning"},
		{name: "a pod that loads its dataset, wrapped reply", err: fmt.Errorf("SLAVEOF NO ONE: %w", replyError("LOADING Redis is loading the dataset in memory")), expLevel: "warning"},
		{name: "a command error", err: errors.New("ERR unknown command"), expLevel: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			pods := &corev1.PodList{Items: []corev1.Pod{
				readyPod("pod-0", "10.0.0.1"),
				readyPod("pod-1", "10.0.0.2"),
				readyPod("pod-2", "10.0.0.3"),
			}}
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			ms.On("UpdatePodLabels", namespace, mock.Anything, mock.Anything).Return(nil)
			mr := &mRedisService.Client{}
			mr.On("MakeMaster", "10.0.0.1", "0", "").Once().Return(test.err)
			mr.On("MakeMaster", "10.0.0.2", "0", "").Once().Return(nil)
			mr.On("MakeSlaveOfWithPort", "10.0.0.3", "0", "10.0.0.2", "0", "").Once().Return(test.err)
			logger, logs := newRecordingLogger()

			healer := rfservice.NewRedisFailoverHealer(ms, mr, logger)
			_, err := healer.SetOldestAsMaster(rf)
			require.NoError(t, err)

			entries := logs.all()
			assertRFFields(t, entries)
			var failed []string
			for _, e := range entries {
				if strings.Contains(e.msg, "failed") {
					failed = append(failed, fmt.Sprintf("%s %v", e.level, e.fields["pod"]))
				}
			}
			assert.Equal(t, []string{test.expLevel + " pod-0", test.expLevel + " pod-2"}, failed)
		})
	}
}

func TestSetMasterOnAllLogLevels(t *testing.T) {
	tests := []struct {
		name        string
		isMasterErr error
		replicaErr  error
		expLevel    string
	}{
		{name: "a pod that does not answer is skipped", isMasterErr: errors.New("i/o timeout"), expLevel: "warning"},
		{name: "a pod that does not answer REPLICAOF", replicaErr: errors.New("connection refused"), expLevel: "warning"},
		{name: "a pod that loads its dataset", replicaErr: errors.New("LOADING Redis is loading the dataset in memory"), expLevel: "warning"},
		{name: "a pod that loads its dataset, wrapped reply", replicaErr: fmt.Errorf("SLAVEOF: %w", replyError("LOADING Redis is loading the dataset in memory")), expLevel: "warning"},
		{name: "a command error", replicaErr: errors.New("ERR unknown command"), expLevel: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			pods := &corev1.PodList{Items: []corev1.Pod{
				podWithRole("new-master", "0.0.0.0", masterRoleLabel),
				podWithRole("old-master", "1.1.1.1", slaveRoleLabel),
			}}
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(pods, nil)
			mr := &mRedisService.Client{}
			mr.On("IsMaster", "0.0.0.0", "0", "").Return(true, nil)
			mr.On("IsMaster", "1.1.1.1", "0", "").Once().Return(false, test.isMasterErr)
			if test.isMasterErr == nil {
				mr.On("MakeSlaveOfWithPort", "1.1.1.1", "0", "0.0.0.0", "0", "").Once().Return(test.replicaErr)
			}
			logger, logs := newRecordingLogger()

			healer := rfservice.NewRedisFailoverHealer(ms, mr, logger, rfservice.WithClientDisconnector(fakeDisconnector{calls: &[]string{}}))
			require.NoError(t, healer.SetMasterOnAll("0.0.0.0", rf))

			entries := logs.all()
			assertRFFields(t, entries)
			last := entries[len(entries)-1]
			assert.Equal(t, test.expLevel, last.level, last.msg)
			assert.Equal(t, "old-master", last.fields["pod"])
		})
	}
}

func TestClientDisconnectorLogLevel(t *testing.T) {
	const uid = types.UID("old")
	tests := []struct {
		name     string
		current  *corev1.Pod
		noUID    bool
		expLevel string
	}{
		{name: "the pod runs", current: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: uid}}, expLevel: "warning"},
		{name: "the pod runs and its UID is not known", current: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: uid}}, noUID: true, expLevel: "warning"},
		{name: "the pod is deleted", expLevel: "info"},
		{name: "the pod terminates", current: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: uid, DeletionTimestamp: &metav1.Time{Time: time.Now()}, Finalizers: []string{"test"}}}, expLevel: "info"},
		{name: "a new pod has the name", current: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "new"}}, expLevel: "info"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			kubeClient := fake.NewClientset(masterEndpointSlice(rf))
			if test.current != nil {
				test.current.Name, test.current.Namespace = "old-master", namespace
				_, err := kubeClient.CoreV1().Pods(namespace).Create(context.Background(), test.current, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			mr, called := disconnectClientsMock(errors.New("connection refused"))
			logger, logs := newRecordingLogger()
			disconnector := rfservice.NewClientDisconnector(kubeClient, mr, logger, time.Minute, 0)

			demoted := podWithRole("old-master", "1.1.1.1", slaveRoleLabel)
			if !test.noUID {
				demoted.UID = uid
			}
			disconnector.DisconnectDemoted(rf, demoted, "0", "")
			select {
			case <-called:
			case <-time.After(5 * time.Second):
				t.Fatal("clients not disconnected")
			}

			var failed logEntry
			require.Eventually(t, func() bool {
				for _, e := range logs.all() {
					if strings.HasPrefix(e.msg, "Could not disconnect") {
						failed = e
						return true
					}
				}
				return false
			}, 5*time.Second, 10*time.Millisecond)
			assert.Equal(t, test.expLevel, failed.level)
			assert.Equal(t, "old-master", failed.fields["pod"])
			assertRFFields(t, []logEntry{failed})
		})
	}
}

func TestApplyPasswordNamesEachRefusingPodOnce(t *testing.T) {
	tests := []struct {
		name   string
		pods   []corev1.Pod
		expErr string
	}{
		{
			name:   "one pod",
			pods:   []corev1.Pod{runningPod("rfr-0", "10.0.0.1")},
			expErr: "redis pod rfr-0 refuses the configured password and the operator doesn't know the one it runs with; put",
		},
		{
			name:   "two pods",
			pods:   []corev1.Pod{runningPod("rfr-0", "10.0.0.1"), runningPod("rfr-1", "10.0.0.2")},
			expErr: "redis pods rfr-0, rfr-1 refuse the configured password and the operator doesn't know the ones they run with; put",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			withAuth(rf, test.pods)
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: test.pods}, nil)
			mr := &mRedisService.Client{}
			mr.On("IsMaster", mock.Anything, "0", "new").Return(false, errors.New("WRONGPASS invalid username-password pair or user is disabled."))

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			_, _, err := healer.ApplyPassword(rf, "new", []string{"new"})

			require.Error(t, err)
			assert.Equal(t, 1, strings.Count(err.Error(), "put the previous password back in the secret"))
			assert.True(t, strings.HasPrefix(err.Error(), test.expErr), err.Error())
		})
	}
}

func TestGetMasterIPSkipsPodsNotRunningOrTerminating(t *testing.T) {
	rf := generateRF()
	terminating := readyPod("rfr-2", "10.0.0.3")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	pods := &corev1.PodList{Items: []corev1.Pod{
		readyPod("rfr-0", "10.0.0.1"),
		{ObjectMeta: metav1.ObjectMeta{Name: "rfr-1"}, Status: corev1.PodStatus{Phase: corev1.PodPending}},
		terminating,
	}}
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "10.0.0.1", "0", "").Once().Return(true, nil)

	checker := rfservice.NewRedisFailoverChecker(ms, mr, log.DummyLogger{}, metrics.Dummy)
	master, err := checker.GetMasterIP(rf)

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", master)
	mr.AssertExpectations(t)
}

func TestCheckSentinelsCannotFailoverLogContext(t *testing.T) {
	sentinel := &corev1.PodList{Items: []corev1.Pod{runningPod("rfs-0", "1.1.1.1")}}
	tests := []struct {
		name      string
		sentinels int32
		monitor   string
		monErr    error
		down      bool
		downErr   error
		replicas  []string
		replErr   error
		expLevel  string
		expPod    any
	}{
		{name: "too few Sentinels run", sentinels: 3, expLevel: "warning"},
		{name: "the master is unknown", monErr: errors.New("i/o timeout"), expLevel: "warning"},
		{name: "the master is a pod", monitor: "10.0.0.1", expLevel: "info", expPod: "rfr-0"},
		{name: "the master state is unknown", monitor: "10.0.0.9", downErr: errors.New("i/o timeout"), expLevel: "warning"},
		{name: "the master is not down", monitor: "10.0.0.9", expLevel: "info"},
		{name: "the replicas are unknown", monitor: "127.0.0.1", replErr: errors.New("i/o timeout"), expLevel: "warning"},
		{name: "a replica is a pod", monitor: "127.0.0.1", replicas: []string{"10.0.0.1"}, expLevel: "info", expPod: "rfr-0"},
		{name: "no replica is a pod", monitor: "10.0.0.9", down: true, replicas: []string{"10.0.0.8"}, expLevel: "warning"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Sentinel.Replicas = max(test.sentinels, 1)
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{readyPod("rfr-0", "10.0.0.1")}}, nil)
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Once().Return(sentinel, nil)
			mr := &mRedisService.Client{}
			mr.On("GetSentinelMonitor", "1.1.1.1").Maybe().Return(test.monitor, "6379", test.monErr)
			mr.On("SentinelMasterDown", "1.1.1.1").Maybe().Return(test.down, test.downErr)
			mr.On("GetSentinelReplicas", "1.1.1.1").Maybe().Return(test.replicas, test.replErr)
			logger, logs := newRecordingLogger()

			checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
			_, err := checker.CheckSentinelsCannotFailover(rf)

			require.NoError(t, err)
			entries := logs.all()
			require.Len(t, entries, 1)
			assert.Equal(t, test.expLevel, entries[0].level)
			assert.Equal(t, test.expPod, entries[0].fields["pod"])
			assertRFFields(t, entries)
		})
	}
}

func TestCheckMasterHealthLogContext(t *testing.T) {
	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{readyPod("rfr-0", "10.0.0.1")}}, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "10.0.0.1", "0", "").Once().Return(true, nil)
	mr.On("IsMaster", "10.0.0.1", "0", "").Once().Return(false, errors.New("i/o timeout"))
	logger, logs := newRecordingLogger()

	checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
	healthy, _, err := checker.CheckMasterHealth(rf)

	require.NoError(t, err)
	assert.False(t, healthy)
	entries := logs.all()
	require.Len(t, entries, 1)
	assert.Equal(t, "warning", entries[0].level)
	assert.Equal(t, "10.0.0.1", entries[0].fields["ip"])
	assertRFFields(t, entries)
}

func TestGetBestReplicaForPromotionLogContext(t *testing.T) {
	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Once().Return(&corev1.PodList{Items: []corev1.Pod{
		readyPod("rfr-0", "10.0.0.1"),
		readyPod("rfr-1", "10.0.0.2"),
	}}, nil)
	mr := &mRedisService.Client{}
	mr.On("GetReplicationInfo", "10.0.0.1", "0", "").Once().Return(nil, errors.New("i/o timeout"))
	mr.On("GetReplicationInfo", "10.0.0.2", "0", "").Once().Return(&redis.ReplicationInfo{Role: "slave", MasterLinkStatus: "up"}, nil)
	logger, logs := newRecordingLogger()

	checker := rfservice.NewRedisFailoverChecker(ms, mr, logger, metrics.Dummy)
	best, err := checker.GetBestReplicaForPromotion(rf)

	require.NoError(t, err)
	assert.Equal(t, "rfr-1", best.PodName)
	entries := logs.all()
	assertRFFields(t, entries)
	assert.Equal(t, []string{"warning rfr-0", "info <nil>", "info rfr-1"}, levelsAndPods(entries))
	assert.Equal(t, "10.0.0.1", entries[0].fields["ip"])
}

func TestEnsureNotPresentSentinelResourcesLogContext(t *testing.T) {
	rf := generateOwnedRF()
	resName := rfservice.GetSentinelName(rf)
	saName := rfservice.GetSentinelServiceAccountName(rf)
	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, resName).Once().Return(&appsv1.Deployment{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteDeployment", namespace, resName).Once().Return(nil)
	ms.On("GetService", namespace, resName).Once().Return(&corev1.Service{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteService", namespace, resName).Once().Return(nil)
	ms.On("GetConfigMap", namespace, resName).Once().Return(&corev1.ConfigMap{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteConfigMap", namespace, resName).Once().Return(nil)
	ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(&policyv1.PodDisruptionBudget{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeletePodDisruptionBudget", namespace, resName).Once().Return(nil)
	ms.On("GetServiceAccount", namespace, saName).Once().Return(&corev1.ServiceAccount{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteServiceAccount", namespace, saName).Once().Return(nil)
	logger, logs := newRecordingLogger()

	client := rfservice.NewRedisFailoverKubeClient(ms, logger, metrics.Dummy)
	require.NoError(t, client.EnsureNotPresentSentinelResources(rf))

	entries := logs.all()
	assert.Len(t, entries, 5)
	assertRFFields(t, entries)
}
