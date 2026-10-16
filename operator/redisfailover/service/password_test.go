package service_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
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

// withAuth gives rf the auth Secret auth, and each pod a template with it.
func withAuth(rf *redisfailoverv1.RedisFailover, pods []corev1.Pod) {
	rf.Spec.Auth.SecretPath = "auth"
	for i := range pods {
		pods[i].Spec.Containers = []corev1.Container{{Name: "redis", Env: []corev1.EnvVar{passwordEnv("auth")}}}
	}
}

func TestApplyPassword(t *testing.T) {
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	nopass := errors.New("ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?")
	redises := &corev1.PodList{Items: []corev1.Pod{runningPod("rfr-0", "10.0.0.1"), runningPod("rfr-1", "10.0.0.2")}}

	tests := []struct {
		name     string
		previous []string
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
			previous:    []string{"new"},
			expComplete: true,
		},
		{
			name:        "a pod on the previous password is changed in place",
			previous:    []string{"old"},
			refuse:      map[string]error{"10.0.0.2": wrongpass},
			expSet:      map[string]string{"10.0.0.2": "old"},
			expComplete: true,
		},
		{
			name:        "a pod without a password is changed without knowing the previous one",
			previous:    []string{"new"},
			refuse:      map[string]error{"10.0.0.1": nopass},
			expSet:      map[string]string{"10.0.0.1": ""},
			expComplete: true,
		},
		{
			name:        "a pod yet to start leaves it incomplete",
			previous:    []string{"new"},
			pending:     true,
			expComplete: false,
		},
		{
			name:        "a pod being deleted is skipped",
			previous:    []string{"new"},
			deleting:    true,
			expComplete: true,
		},
		{
			name:     "a refused password with no previous one to use",
			previous: []string{"new"},
			refuse:   map[string]error{"10.0.0.1": wrongpass},
			expErr:   "put the previous password back in the secret",
		},
		{
			name:     "a pod that refuses each previous password is unknown",
			previous: []string{"old"},
			refuse:   map[string]error{"10.0.0.2": wrongpass},
			setErr:   wrongpass,
			expSet:   map[string]string{"10.0.0.2": "old"},
			expErr:   "redis pod rfr-1 refuses the configured password and the operator doesn't know the one it runs with; put the previous password back in the secret",
		},
		{
			name:     "a failed change is returned",
			previous: []string{"old"},
			refuse:   map[string]error{"10.0.0.2": wrongpass},
			setErr:   errors.New("i/o timeout"),
			expSet:   map[string]string{"10.0.0.2": "old"},
			expErr:   "changing the password of redis pod rfr-1",
		},
		{
			name:        "an unreachable pod leaves it incomplete",
			previous:    []string{"old"},
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
			withAuth(rf, pods.Items)
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(pods, nil)
			mr := &mRedisService.Client{}
			for _, p := range redises.Items {
				mr.On("IsMaster", p.Status.PodIP, "0", "new").Return(false, test.refuse[p.Status.PodIP])
			}
			for ip, current := range test.expSet {
				mr.On("SetPassword", ip, "0", current, "new").Once().Return(test.setErr)
			}

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			complete, stale, err := healer.ApplyPassword(rf, "new", test.previous)
			if test.expErr != "" {
				require.ErrorContains(t, err, test.expErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, test.expComplete, complete)
			assert.False(t, stale)
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
	complete, _, err := healer.ApplyPassword(rf, "new", []string{"old"})
	require.ErrorIs(t, err, boom)
	assert.False(t, complete)
}

// TestApplyPasswordTriesEachPreviousPassword covers a second change while a
// pod is yet to start: the running pods are on the password applied last, not
// on the one that all pods accepted.
func TestApplyPasswordTriesEachPreviousPassword(t *testing.T) {
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	rf := generateRF()
	ms := &mK8SService.Services{}
	ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: []corev1.Pod{runningPod("rfr-0", "10.0.0.1")}}, nil)
	mr := &mRedisService.Client{}
	mr.On("IsMaster", "10.0.0.1", "0", "v3").Return(false, wrongpass)
	mr.On("SetPassword", "10.0.0.1", "0", "v1", "v3").Once().Return(wrongpass)
	mr.On("SetPassword", "10.0.0.1", "0", "v2", "v3").Once().Return(nil)

	healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
	complete, _, err := healer.ApplyPassword(rf, "v3", []string{"v1", "v2"})
	require.NoError(t, err)
	assert.True(t, complete)
	mr.AssertExpectations(t)
}

// TestApplyPasswordRemovalHint covers the recovery that the error tells the
// user after the removal of auth.secretPath. An auth Secret with an empty
// password is not a removal.
func TestApplyPasswordRemovalHint(t *testing.T) {
	tests := []struct {
		name string
		auth bool
		hint string
	}{
		{name: "removal", hint: "set auth.secretPath to a Secret with the previous password until the RedisFailover is healthy, then remove it again"},
		{name: "empty password", auth: true, hint: "put the previous password back in the secret until the RedisFailover is healthy, then change it again"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			pods := []corev1.Pod{runningPod("rfr-0", "10.0.0.1")}
			if test.auth {
				withAuth(rf, pods)
			}
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: pods}, nil)
			mr := &mRedisService.Client{}
			mr.On("IsMaster", "10.0.0.1", "0", "").Return(false, errors.New("NOAUTH Authentication required."))

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			complete, _, err := healer.ApplyPassword(rf, "", []string{""})
			require.EqualError(t, err, "redis pod rfr-0 refuses the configured password and the operator doesn't know the one it runs with; "+test.hint)
			assert.False(t, complete)
			mr.AssertNotCalled(t, "SetPassword", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// TestApplyPasswordStalePodYetToStart covers a pod that does not run: its
// template alone tells whether a start gives Redis another password.
func TestApplyPasswordStalePodYetToStart(t *testing.T) {
	tests := []struct {
		name     string
		env      []corev1.EnvVar
		expStale bool
	}{
		{name: "current template", env: []corev1.EnvVar{passwordEnv("auth")}},
		{name: "stale template", env: []corev1.EnvVar{passwordEnv("old")}, expStale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Auth.SecretPath = "auth"
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-0"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "redis", Env: test.env}}},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			}
			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: []corev1.Pod{pod}}, nil)

			healer := rfservice.NewRedisFailoverHealer(ms, &mRedisService.Client{}, log.DummyLogger{})
			complete, stale, err := healer.ApplyPassword(rf, "v1", []string{"v1"})
			require.NoError(t, err)
			assert.False(t, complete)
			assert.Equal(t, test.expStale, stale)
		})
	}
}

// passwordEnv is the REDIS_PASSWORD env var from the key password of the
// Secret secret.
func passwordEnv(secret string) corev1.EnvVar {
	return corev1.EnvVar{Name: "REDIS_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "password",
	}}}
}

// TestApplyPasswordStalePodTemplate covers a running pod whose template gives
// redis-server the password of another Secret than the RedisFailover. A
// container restart gives Redis that password again, so the change is not
// complete, and the Secret of the template is a candidate.
func TestApplyPasswordStalePodTemplate(t *testing.T) {
	noauth := errors.New("NOAUTH Authentication required.")
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	tests := []struct {
		name       string
		secretPath string
		redisEnv   []corev1.EnvVar
		podEnv     []corev1.EnvVar
		sidecar    bool
		password   string
		previous   []string
		refuse     error
		// the value of the Secret old, or an error to read it
		secret    string
		secretErr error
		// the passwords SetPassword logs in with, in order
		expSet      []string
		expRead     bool
		expComplete bool
		expErr      string
	}{
		{
			name:        "the pod template has the password of the RedisFailover",
			secretPath:  "auth",
			podEnv:      []corev1.EnvVar{passwordEnv("auth")},
			password:    "v1",
			previous:    []string{"v1"},
			expComplete: true,
		},
		{
			name:        "a user env var with the same Secret as the pod template",
			redisEnv:    []corev1.EnvVar{passwordEnv("auth")},
			podEnv:      []corev1.EnvVar{passwordEnv("auth")},
			previous:    []string{""},
			expComplete: true,
		},
		{
			name:        "a later env var without a Secret replaces an earlier one",
			podEnv:      []corev1.EnvVar{passwordEnv("old"), {Name: "REDIS_PASSWORD", Value: "literal"}},
			previous:    []string{""},
			expComplete: true,
		},
		{
			name:        "a pod without a redis container has no password",
			podEnv:      []corev1.EnvVar{passwordEnv("old")},
			sidecar:     true,
			previous:    []string{""},
			expComplete: true,
		},
		{
			name:     "a stale pod that accepts the password",
			podEnv:   []corev1.EnvVar{passwordEnv("old")},
			previous: []string{""},
		},
		{
			name:     "a stale pod on a known password does not read its Secret",
			podEnv:   []corev1.EnvVar{passwordEnv("old")},
			previous: []string{"v1"},
			refuse:   noauth,
			expSet:   []string{"v1"},
		},
		{
			name:     "a stale pod on the password of its Secret",
			podEnv:   []corev1.EnvVar{passwordEnv("old")},
			previous: []string{"v0", ""},
			refuse:   noauth,
			secret:   "v1",
			expSet:   []string{"v0", "v1"},
			expRead:  true,
		},
		{
			name:       "a stale pod of another Secret after a change of auth.secretPath",
			secretPath: "auth",
			podEnv:     []corev1.EnvVar{passwordEnv("old")},
			password:   "v2",
			previous:   []string{"v2"},
			refuse:     wrongpass,
			secret:     "v1",
			expSet:     []string{"v1"},
			expRead:    true,
		},
		{
			name:     "a stale pod whose Secret has a password that it refused",
			podEnv:   []corev1.EnvVar{passwordEnv("old")},
			previous: []string{"v1"},
			refuse:   noauth,
			secret:   "v1",
			expSet:   []string{"v1"},
			expRead:  true,
			expErr:   "set auth.secretPath to a Secret with the previous password",
		},
		{
			name:      "a stale pod whose Secret is missing",
			podEnv:    []corev1.EnvVar{passwordEnv("old")},
			previous:  []string{""},
			refuse:    noauth,
			secretErr: errors.New(`secrets "old" not found`),
			expRead:   true,
			expErr:    "set auth.secretPath to a Secret with the previous password",
		},
		{
			name:       "a stale pod without a Secret has no candidate",
			secretPath: "auth",
			password:   "v2",
			previous:   []string{"v2"},
			refuse:     wrongpass,
			expErr:     "put the previous password back in the secret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Auth.SecretPath = test.secretPath
			rf.Spec.Redis.Env = test.redisEnv
			pod := runningPod("rfr-0", "10.0.0.1")
			name := "redis"
			if test.sidecar {
				name = "sidecar"
			}
			pod.Spec.Containers = []corev1.Container{{Name: name, Env: test.podEnv}}

			ms := &mK8SService.Services{}
			ms.On("GetStatefulSetPods", namespace, rfservice.GetRedisName(rf)).Return(&corev1.PodList{Items: []corev1.Pod{pod}}, nil)
			ms.On("GetSecret", namespace, "old").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte(test.secret)}}, test.secretErr)
			mr := &mRedisService.Client{}
			mr.On("IsMaster", "10.0.0.1", "0", test.password).Return(false, test.refuse)
			for i, current := range test.expSet {
				var err error
				if i < len(test.expSet)-1 || test.expErr != "" {
					err = test.refuse
				}
				mr.On("SetPassword", "10.0.0.1", "0", current, test.password).Once().Return(err)
			}

			logger, logs := newRecordingLogger()
			healer := rfservice.NewRedisFailoverHealer(ms, mr, logger)
			complete, stale, err := healer.ApplyPassword(rf, test.password, test.previous)
			if test.expErr != "" {
				require.ErrorContains(t, err, test.expErr)
			} else {
				require.NoError(t, err)
				// Each case without an error and without completion has a stale pod.
				assert.Equal(t, !test.expComplete, stale)
			}
			assert.Equal(t, test.expComplete, complete)
			mr.AssertExpectations(t)
			mr.AssertNumberOfCalls(t, "SetPassword", len(test.expSet))
			if test.expRead {
				ms.AssertCalled(t, "GetSecret", namespace, "old")
			} else {
				ms.AssertNotCalled(t, "GetSecret", mock.Anything, mock.Anything)
			}
			var debugs []string
			for _, e := range logs.all() {
				if e.level == "debug" {
					debugs = append(debugs, e.msg)
				}
				if test.secret != "" {
					assert.NotContains(t, e.msg, test.secret)
				}
			}
			if test.secretErr != nil {
				assert.Equal(t, []string{`Unable to read Secret old of a stale redis pod template: secrets "old" not found`}, debugs)
				assertRFFields(t, logs.all())
			} else {
				assert.Empty(t, debugs)
			}
		})
	}
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
			rf.Spec.Sentinel.Enabled = new(test.sentinel)

			ms := &mK8SService.Services{}
			ms.On("GetDeploymentPods", namespace, rfservice.GetSentinelName(rf)).Return(sentinels, test.listErr)
			mr := &mRedisService.Client{}
			mr.On("SetSentinelAuthPass", "10.0.1.1", "new").Return(test.setErr)
			mr.On("SetSentinelAuthPass", "10.0.1.2", "new").Return(nil)

			healer := rfservice.NewRedisFailoverHealer(ms, mr, log.DummyLogger{})
			complete, err := healer.ApplySentinelPassword(rf, "new")
			if test.expErr != "" {
				require.ErrorContains(t, err, test.expErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, test.expComplete, complete)
			mr.AssertNumberOfCalls(t, "SetSentinelAuthPass", test.expCalls)
		})
	}
}
