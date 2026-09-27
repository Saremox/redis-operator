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

func TestApplyPassword(t *testing.T) {
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	running := func(name, ip string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning},
		}
	}
	redises := &corev1.PodList{Items: []corev1.Pod{running("rfr-0", "10.0.0.1"), running("rfr-1", "10.0.0.2")}}
	sentinels := &corev1.PodList{Items: []corev1.Pod{running("rfs-0", "10.0.1.1")}}

	tests := []struct {
		name            string
		previous        string
		sentinel        bool
		pending         bool
		deleting        bool
		stoppedSentinel bool
		// errors IsMaster returns with the new password, per pod IP
		refuse      map[string]error
		setErr      error
		sentinelErr error
		expSet      []string
		expSentinel bool
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
			expSet:      []string{"10.0.0.2"},
			expComplete: true,
		},
		{
			name:        "the sentinels get the changed password",
			previous:    "old",
			sentinel:    true,
			refuse:      map[string]error{"10.0.0.1": wrongpass, "10.0.0.2": wrongpass},
			expSet:      []string{"10.0.0.1", "10.0.0.2"},
			expSentinel: true,
			expComplete: true,
		},
		{
			name:        "the sentinels get the password when the previous one is unknown",
			previous:    "new",
			sentinel:    true,
			expSentinel: true,
			expComplete: true,
		},
		{
			name:        "a pod yet to start leaves it incomplete",
			previous:    "new",
			sentinel:    true,
			pending:     true,
			expComplete: false,
		},
		{
			name:     "a refused password with no previous one to use",
			previous: "new",
			refuse:   map[string]error{"10.0.0.1": wrongpass},
			expErr:   "delete the redis pods",
		},
		{
			name:        "a pod being deleted is skipped",
			previous:    "new",
			deleting:    true,
			expComplete: true,
		},
		{
			name:            "a sentinel that isn't running is skipped",
			previous:        "new",
			sentinel:        true,
			stoppedSentinel: true,
			expSentinel:     true,
			expComplete:     true,
		},
		{
			name:     "a failed change is returned",
			previous: "old",
			refuse:   map[string]error{"10.0.0.2": wrongpass},
			setErr:   errors.New("i/o timeout"),
			expSet:   []string{"10.0.0.2"},
			expErr:   "changing the password of redis pod rfr-1",
		},
		{
			name:        "a sentinel refusing the change is returned",
			previous:    "new",
			sentinel:    true,
			sentinelErr: errors.New("i/o timeout"),
			expSentinel: true,
			expErr:      "changing the password of sentinel pod rfs-0",
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
			rf.Spec.Auth.SecretPath = "redis-auth"
			rf.Spec.Sentinel.Enabled = ptr.To(test.sentinel)

			ms := &mK8SService.Services{}
			ms.On("GetSecret", namespace, "redis-auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("new")}}, nil)
			pods := redises.DeepCopy()
			if test.pending {
				pods.Items = append(pods.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfr-2"}, Status: corev1.PodStatus{Phase: corev1.PodPending}})
			}
			if test.deleting {
				deleting := running("rfr-2", "10.0.0.3")
				deleting.DeletionTimestamp = &metav1.Time{}
				pods.Items = append(pods.Items, deleting)
			}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
			sps := sentinels.DeepCopy()
			if test.stoppedSentinel {
				sps.Items = append(sps.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfs-1"}, Status: corev1.PodStatus{PodIP: "10.0.1.2", Phase: corev1.PodFailed}})
			}
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Return(sps, nil)
			mr := &mRedisService.Client{}
			for _, p := range redises.Items {
				mr.On("IsMaster", p.Status.PodIP, "0", "new").Return(false, test.refuse[p.Status.PodIP])
			}
			for _, ip := range test.expSet {
				mr.On("SetPassword", ip, "0", test.previous, "new").Once().Return(test.setErr)
			}
			if test.expSentinel {
				mr.On("SetSentinelAuthPass", "10.0.1.1", "new").Once().Return(test.sentinelErr)
			}

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			complete, err := healer.ApplyPassword(rf, test.previous)
			if test.expErr != "" {
				assert.ErrorContains(t, err, test.expErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, test.expComplete, complete)
			mr.AssertExpectations(t)
			mr.AssertNumberOfCalls(t, "SetPassword", len(test.expSet))
			if !test.expSentinel {
				mr.AssertNotCalled(t, "SetSentinelAuthPass", "10.0.1.1", "new")
			}
		})
	}
}

func TestApplyPasswordListErrors(t *testing.T) {
	boom := errors.New("boom")
	pods := &corev1.PodList{Items: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "rfr-0"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning},
	}}}

	tests := []struct {
		name      string
		secretErr error
		podsErr   error
		sentErr   error
	}{
		{name: "secret", secretErr: boom},
		{name: "redis pods", podsErr: boom},
		{name: "sentinel pods", sentErr: boom},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Auth.SecretPath = "redis-auth"
			rf.Spec.Sentinel.Enabled = ptr.To(true)

			ms := &mK8SService.Services{}
			ms.On("GetSecret", namespace, "redis-auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("new")}}, test.secretErr)
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, test.podsErr)
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Return(&corev1.PodList{}, test.sentErr)
			mr := &mRedisService.Client{}
			mr.On("IsMaster", "10.0.0.1", "0", "new").Return(true, nil)

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			complete, err := healer.ApplyPassword(rf, "new")
			assert.ErrorIs(t, err, boom)
			assert.False(t, complete)
		})
	}
}
