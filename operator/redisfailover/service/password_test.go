package service_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func runningPod(name, ip string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning},
	}
}

func TestApplyPassword(t *testing.T) {
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	nopass := errors.New("ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?")
	redises := &corev1.PodList{Items: []corev1.Pod{runningPod("rfr-0", "10.0.0.1"), runningPod("rfr-1", "10.0.0.2")}}

	tests := []struct {
		name     string
		previous string
		pending  bool
		deleting bool
		// errors IsMaster returns with the new password, per pod IP
		refuse map[string]error
		setErr error
		// the password SetPassword logs in with, per pod IP
		expSet      map[string]string
		expComplete bool
		expErr      string
	}{
		{
			name:        "every pod accepts the password",
			previous:    "new",
			expComplete: true,
		},
		{
			name:        "a pod on the previous password is changed in place",
			previous:    "old",
			refuse:      map[string]error{"10.0.0.2": wrongpass},
			expSet:      map[string]string{"10.0.0.2": "old"},
			expComplete: true,
		},
		{
			name:        "a pod without a password is changed without knowing the previous one",
			previous:    "new",
			refuse:      map[string]error{"10.0.0.1": nopass},
			expSet:      map[string]string{"10.0.0.1": ""},
			expComplete: true,
		},
		{
			name:        "a pod yet to start leaves it incomplete",
			previous:    "new",
			pending:     true,
			expComplete: false,
		},
		{
			name:        "a pod being deleted is skipped",
			previous:    "new",
			deleting:    true,
			expComplete: true,
		},
		{
			name:     "a refused password with no previous one to use",
			previous: "new",
			refuse:   map[string]error{"10.0.0.1": wrongpass},
			expErr:   "put the previous password back in the secret",
		},
		{
			name:     "a failed change is returned",
			previous: "old",
			refuse:   map[string]error{"10.0.0.2": wrongpass},
			setErr:   errors.New("i/o timeout"),
			expSet:   map[string]string{"10.0.0.2": "old"},
			expErr:   "changing the password of redis pod rfr-1",
		},
		{
			name:        "an unreachable pod leaves it incomplete",
			previous:    "old",
			refuse:      map[string]error{"10.0.0.1": errors.New("i/o timeout")},
			expComplete: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()

			ms := &mK8SService.Services{}
			pods := redises.DeepCopy()
			if test.pending {
				pods.Items = append(pods.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfr-2"}, Status: corev1.PodStatus{Phase: corev1.PodPending}})
			}
			if test.deleting {
				deleting := runningPod("rfr-2", "10.0.0.3")
				deleting.DeletionTimestamp = &metav1.Time{}
				pods.Items = append(pods.Items, deleting)
			}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
			mr := &mRedisService.Client{}
			for _, p := range redises.Items {
				mr.On("IsMaster", p.Status.PodIP, "0", "new").Return(false, test.refuse[p.Status.PodIP])
			}
			for ip, current := range test.expSet {
				mr.On("SetPassword", ip, "0", current, "new").Once().Return(test.setErr)
			}

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			complete, err := healer.ApplyPassword(rf, "new", test.previous)
			if test.expErr != "" {
				assert.ErrorContains(t, err, test.expErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, test.expComplete, complete)
			mr.AssertExpectations(t)
			mr.AssertNumberOfCalls(t, "SetPassword", len(test.expSet))
		})
	}
}

func TestApplyPasswordListError(t *testing.T) {
	boom := errors.New("boom")
	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(nil, boom)

	healer := rfservice.NewRedisFailoverHealer(ms, &mRedisService.Client{}, log.DummyLogger{})
	complete, err := healer.ApplyPassword(rf, "new", "old")
	assert.ErrorIs(t, err, boom)
	assert.False(t, complete)
}

func TestApplySentinelPassword(t *testing.T) {
	stopped := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfs-2"}, Status: corev1.PodStatus{PodIP: "10.0.1.3", Phase: corev1.PodFailed}}
	deleting := runningPod("rfs-3", "10.0.1.4")
	deleting.DeletionTimestamp = &metav1.Time{}
	sentinels := &corev1.PodList{Items: []corev1.Pod{runningPod("rfs-0", "10.0.1.1"), runningPod("rfs-1", "10.0.1.2"), stopped, deleting}}

	tests := []struct {
		name        string
		sentinel    bool
		listErr     error
		setErr      error
		expCalls    int
		expComplete bool
		expErr      string
	}{
		{
			name:        "without sentinels there is nothing to do",
			expComplete: true,
		},
		{
			name:        "every running sentinel gets the password",
			sentinel:    true,
			expCalls:    2,
			expComplete: true,
		},
		{
			name:        "an unreachable sentinel leaves it incomplete",
			sentinel:    true,
			setErr:      errors.New("dial tcp 10.0.1.1:26379: i/o timeout"),
			expCalls:    2,
			expComplete: false,
		},
		{
			name:     "a sentinel refusing the change is returned",
			sentinel: true,
			setErr:   errors.New("ERR No such master with that name"),
			expCalls: 1,
			expErr:   "changing the password of sentinel pod rfs-0",
		},
		{
			name:     "a failed pod list is returned",
			sentinel: true,
			listErr:  errors.New("boom"),
			expErr:   "boom",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Sentinel.Enabled = ptr.To(test.sentinel)

			ms := &mK8SService.Services{}
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Return(sentinels, test.listErr)
			mr := &mRedisService.Client{}
			mr.On("SetSentinelAuthPass", "10.0.1.1", "new").Return(test.setErr)
			mr.On("SetSentinelAuthPass", "10.0.1.2", "new").Return(nil)

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			complete, err := healer.ApplySentinelPassword(rf, "new")
			if test.expErr != "" {
				assert.ErrorContains(t, err, test.expErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, test.expComplete, complete)
			mr.AssertNumberOfCalls(t, "SetSentinelAuthPass", test.expCalls)
		})
	}
}
