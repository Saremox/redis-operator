package redisfailover

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mRFService "github.com/saremox/redis-operator/mocks/operator/redisfailover/service"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// appliedSecretEnv is a RedisFailover whose Redis pods run with the passwords
// in running, one for each pod IP. A pod with an empty IP is yet to start.
type appliedSecretEnv struct {
	rf       *redisfailoverv1.RedisFailover
	ms       *mK8SService.Services
	password string
	running  map[string]string
	stored   map[string]*corev1.Secret
	tried    []string
}

func newAppliedSecretEnv(t *testing.T, password string, running map[string]string, pending bool) *appliedSecretEnv {
	t.Helper()
	e := &appliedSecretEnv{
		rf: &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns", UID: "rf-uid"},
			Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
		},
		ms:       &mK8SService.Services{},
		password: password,
		running:  running,
	}
	pods := &corev1.PodList{}
	for ip := range running {
		pods.Items = append(pods.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-" + ip}, Status: corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning}})
	}
	if pending {
		pods.Items = append(pods.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-pending"}, Status: corev1.PodStatus{Phase: corev1.PodPending}})
	}
	e.ms.On("GetSecret", "testns", "redis-auth").Return(func(string, string) (*corev1.Secret, error) {
		return &corev1.Secret{Data: map[string][]byte{"password": []byte(e.password)}}, nil
	})
	e.ms.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(e.rf)).Return(pods, nil)
	e.ms.On("GetDeploymentPods", "testns", rfservice.GetSentinelName(e.rf)).Return(&corev1.PodList{}, nil)
	e.stored = mockAppliedSecret(e.ms)
	return e
}

// handler returns a handler with no passwordState, as after an operator start.
func (e *appliedSecretEnv) handler(logger log.Logger) *RedisFailoverHandler {
	wrongpass := errors.New("WRONGPASS invalid username-password pair or user is disabled.")
	mr := &mRedisService.Client{}
	mr.On("IsMaster", mock.Anything, "0", mock.Anything).Return(func(ip, _, pw string) (bool, error) {
		if pw != e.running[ip] {
			return false, wrongpass
		}
		return true, nil
	})
	mr.On("SetPassword", mock.Anything, "0", mock.Anything, mock.Anything).Return(func(ip, _, pw, newPw string) error {
		e.tried = append(e.tried, ip+":"+pw)
		if pw != e.running[ip] {
			return wrongpass
		}
		e.running[ip] = newPw
		return nil
	})
	healer := rfservice.NewRedisFailoverHealer(e.ms, mr, log.Dummy)
	return NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, healer, e.ms, metrics.Dummy, logger)
}

func (e *appliedSecretEnv) storeApplied(redis, applied string) {
	name := rfservice.GetRedisAuthAppliedName(e.rf)
	e.stored[name] = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(e.rf, redisfailoverv1.VersionKind(redisfailoverv1.RFKind))}},
		Data:       map[string][]byte{"redis": []byte(redis), "applied": []byte(applied)},
	}
}

func (e *appliedSecretEnv) applied() (redis, applied string) {
	s := e.stored[rfservice.GetRedisAuthAppliedName(e.rf)]
	if s == nil {
		return "", ""
	}
	return string(s.Data["redis"]), string(s.Data["applied"])
}

func (e *appliedSecretEnv) writes() int {
	n := 0
	for _, c := range e.ms.Calls {
		if c.Method == "CreateOrUpdateSecret" {
			n++
		}
	}
	return n
}

// TestApplyPasswordAfterRestartUsesTheAppliedSecret covers a secret change
// before the first reconcile of a started operator. Without the applied
// Secret, no pod accepts a known password and the RedisFailover stays
// NotHealthy.
func TestApplyPasswordAfterRestartUsesTheAppliedSecret(t *testing.T) {
	e := newAppliedSecretEnv(t, "v2", map[string]string{"10.0.0.1": "v1", "10.0.0.2": "v1"}, false)
	e.storeApplied("v1", "v1")

	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))
	assert.Equal(t, map[string]string{"10.0.0.1": "v2", "10.0.0.2": "v2"}, e.running)
	redis, applied := e.applied()
	assert.Equal(t, []string{"v2", "v2"}, []string{redis, applied})
}

// TestApplyPasswordTriesEachSavedPassword covers a pod on applied and a pod on
// redis, each one tried once, and the order of the candidates.
func TestApplyPasswordTriesEachSavedPassword(t *testing.T) {
	e := newAppliedSecretEnv(t, "v3", map[string]string{"10.0.0.1": "v1", "10.0.0.2": "v2", "10.0.0.3": "v3"}, false)
	e.storeApplied("v1", "v2")

	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))
	assert.Equal(t, map[string]string{"10.0.0.1": "v3", "10.0.0.2": "v3", "10.0.0.3": "v3"}, e.running)
	assert.ElementsMatch(t, []string{"10.0.0.1:v1", "10.0.0.2:v1", "10.0.0.2:v2"}, e.tried)
}

// TestApplyPasswordIgnoresAStaleAppliedSecret covers a Secret value that no
// pod runs with, while every pod accepts the configured password.
func TestApplyPasswordIgnoresAStaleAppliedSecret(t *testing.T) {
	e := newAppliedSecretEnv(t, "v2", map[string]string{"10.0.0.1": "v2"}, false)
	e.storeApplied("v0", "v0")

	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))
	assert.Equal(t, "v2", e.running["10.0.0.1"])
	assert.Empty(t, e.tried)
	redis, applied := e.applied()
	assert.Equal(t, []string{"v2", "v2"}, []string{redis, applied})
}

func TestAppliedPasswordCandidates(t *testing.T) {
	tests := []struct {
		name           string
		redis, applied string
		want           []string
	}{
		{name: "no Secret", want: []string{"v2"}},
		{name: "same as the secret", redis: "v2", applied: "v2", want: []string{"v2"}},
		{name: "one old password", redis: "v1", applied: "v1", want: []string{"v2", "v1"}},
		{name: "two old passwords", redis: "v0", applied: "v1", want: []string{"v2", "v0", "v1"}},
		{name: "applied is the secret", redis: "v1", applied: "v2", want: []string{"v2", "v1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := newAppliedSecretEnv(t, "v2", nil, false)
			if test.redis != "" {
				e.storeApplied(test.redis, test.applied)
			}
			rf := e.rf
			mrfh := &mRFService.RedisFailoverHeal{}
			handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, e.ms, metrics.Dummy, log.Dummy)
			mrfh.On("ApplyPassword", rf, "v2", test.want).Once().Return(true, nil)
			mrfh.On("ApplySentinelPassword", rf, "v2").Once().Return(true, nil)
			require.NoError(t, handler.applyPassword(rf))
			mrfh.AssertExpectations(t)
		})
	}
}

// TestApplyPasswordKeepsTheSavedPasswordOfAPendingPod covers a restart while
// a pod is yet to start. The pod can come up with the saved redis password,
// so the operator must keep it until the pod runs.
func TestApplyPasswordKeepsTheSavedPasswordOfAPendingPod(t *testing.T) {
	e := newAppliedSecretEnv(t, "v2", map[string]string{"10.0.0.1": "v2"}, true)
	e.storeApplied("v1", "v2")

	handler := e.handler(log.Dummy)
	require.NoError(t, handler.applyPassword(e.rf))
	v, _ := handler.passwords.Load(passwordKey(e.rf))
	assert.Equal(t, "v1", v.(passwordState).redis)
	assert.Equal(t, 0, e.writes())
}

// TestApplyPasswordSavesOncePerChange covers the writes of the applied
// Secret: one for each change, none for an unchanged reconcile, and none
// after a restart with unchanged passwords.
func TestApplyPasswordSavesOncePerChange(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, false)
	handler := e.handler(log.Dummy)

	for range 3 {
		require.NoError(t, handler.applyPassword(e.rf))
	}
	assert.Equal(t, 1, e.writes())
	s := e.stored[rfservice.GetRedisAuthAppliedName(e.rf)]
	assert.Equal(t, "rfr-auth-applied-test", s.Name)
	assert.Equal(t, "testns", s.Namespace)
	assert.Equal(t, handler.getLabels(e.rf), s.Labels)
	if assert.Len(t, s.OwnerReferences, 1) {
		assert.Equal(t, e.rf.UID, s.OwnerReferences[0].UID)
	}

	e.password = "v2"
	for range 3 {
		require.NoError(t, handler.applyPassword(e.rf))
	}
	assert.Equal(t, 2, e.writes())
	redis, applied := e.applied()
	assert.Equal(t, []string{"v2", "v2"}, []string{redis, applied})

	// An operator restart.
	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))
	assert.Equal(t, 2, e.writes())
}

// TestApplyPasswordGoesOnWhenTheSaveFails covers an operator without the
// right to write Secrets: the reconcile goes on, and the same error logs one
// Warning only.
func TestApplyPasswordGoesOnWhenTheSaveFails(t *testing.T) {
	password := "v1"
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
	}
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "redis-auth").Return(func(string, string) (*corev1.Secret, error) {
		return &corev1.Secret{Data: map[string][]byte{"password": []byte(password)}}, nil
	})
	ms.On("GetSecret", "testns", "rfr-auth-applied-test").Return(nil, apierrors.NewNotFound(corev1.Resource("secrets"), "rfr-auth-applied-test"))
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "rfr-auth-applied-test", errors.New("no create"))
	ms.On("CreateOrUpdateSecret", "testns", mock.Anything).Return(forbidden)
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", rf, mock.Anything, mock.Anything).Return(false, nil)
	mrfh.On("ApplySentinelPassword", rf, mock.Anything).Return(true, nil)
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, logger)

	require.NoError(t, handler.applyPassword(rf))
	v, ok := handler.passwords.Load(passwordKey(rf))
	require.True(t, ok)
	assert.Equal(t, "v1", v.(passwordState).applied)

	password = "v2"
	require.NoError(t, handler.applyPassword(rf))
	password = "v3"
	require.NoError(t, handler.applyPassword(rf))
	v, _ = handler.passwords.Load(passwordKey(rf))
	assert.Equal(t, "v3", v.(passwordState).applied)

	ms.AssertNumberOfCalls(t, "CreateOrUpdateSecret", 3)
	assert.Len(t, logger.warnings, 1)
	assert.Len(t, logger.debugs, 2)
}

// TestApplyPasswordRetriesAFailedSave covers an API error that can go away:
// the next reconcile writes the Secret again, and the error logs one Warning.
func TestApplyPasswordRetriesAFailedSave(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, false)
	boom := errors.New("etcdserver: request timed out")
	fails := 2
	e.ms.On("CreateOrUpdateSecret", "testns", mock.Anything).Return(func(string, *corev1.Secret) error {
		if fails > 0 {
			fails--
			return boom
		}
		return nil
	})
	e.ms.ExpectedCalls = slices.DeleteFunc(e.ms.ExpectedCalls, func(c *mock.Call) bool {
		return c.Method == "CreateOrUpdateSecret" && c.Arguments[0] == mock.Anything
	})
	logger := &warnLogger{}
	handler := e.handler(logger)

	for range 5 {
		require.NoError(t, handler.applyPassword(e.rf))
	}
	assert.Equal(t, 3, e.writes())
	assert.Len(t, logger.warnings, 1)
	v, _ := handler.passwords.Load(passwordKey(e.rf))
	assert.False(t, v.(passwordState).unsaved)
}

// TestApplyPasswordKeepsAForeignSecret covers a Secret with the name of the
// applied Secret that the RedisFailover does not control: the operator neither
// reads nor changes it.
func TestApplyPasswordKeepsAForeignSecret(t *testing.T) {
	tests := map[string][]metav1.OwnerReference{
		"no controller": nil,
		"another controller": {*metav1.NewControllerRef(&redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "other"}},
			redisfailoverv1.VersionKind(redisfailoverv1.RFKind))},
	}
	for name, owners := range tests {
		t.Run(name, func(t *testing.T) {
			e := newAppliedSecretEnv(t, "v2", map[string]string{"10.0.0.1": "v1"}, false)
			foreign := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-auth-applied-test", OwnerReferences: owners},
				Data:       map[string][]byte{"redis": []byte("v1"), "applied": []byte("v1")},
			}
			e.stored[foreign.Name] = foreign
			logger := &warnLogger{}
			handler := e.handler(logger)

			// The behavior without the Secret: the pod stays on its password.
			assert.ErrorContains(t, handler.applyPassword(e.rf), "put the previous password back")
			assert.Equal(t, "v1", e.running["10.0.0.1"])
			assert.Equal(t, 0, e.writes())

			e.password = "v1"
			require.NoError(t, handler.applyPassword(e.rf))
			e.password = "v3"
			require.NoError(t, handler.applyPassword(e.rf))
			assert.Same(t, foreign, e.stored[foreign.Name])
			assert.Len(t, logger.warnings, 1)
		})
	}
}

// TestApplyPasswordSavesARemovedPassword covers the removal of
// auth.secretPath: the Secret must not keep the old password.
func TestApplyPasswordSavesARemovedPassword(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, false)
	handler := e.handler(log.Dummy)
	require.NoError(t, handler.applyPassword(e.rf))

	e.rf.Spec.Auth.SecretPath = ""
	for range 3 {
		require.NoError(t, handler.applyPassword(e.rf))
	}
	assert.Equal(t, "", e.running["10.0.0.1"])
	redis, applied := e.applied()
	assert.Equal(t, []string{"", ""}, []string{redis, applied})
	assert.Equal(t, 2, e.writes())
}

// TestApplyPasswordGoesOnWhenTheReadFails covers a read error other than
// NotFound: the operator logs it, tries only the configured password, and
// does not replace the Secret that it could not read.
func TestApplyPasswordGoesOnWhenTheReadFails(t *testing.T) {
	password := "v1"
	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
		Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
	}
	ms := &mK8SService.Services{}
	ms.On("GetSecret", "testns", "redis-auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte(password)}}, nil)
	ms.On("GetSecret", "testns", "rfr-auth-applied-test").Return(nil, errors.New("boom"))
	ms.On("CreateOrUpdateSecret", "testns", mock.Anything).Return(nil)
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", rf, "v1", []string{"v1"}).Once().Return(true, nil)
	mrfh.On("ApplySentinelPassword", rf, "v1").Return(true, nil)
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, logger)

	require.NoError(t, handler.applyPassword(rf))
	mrfh.AssertExpectations(t)
	assert.Len(t, logger.warnings, 1)
	ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
	_, cached := handler.passwords.Load(passwordKey(rf))
	assert.True(t, cached)
}

// TestApplyPasswordWithoutAuthUsesNoAppliedSecret covers a RedisFailover
// without a password: there is nothing to keep.
func TestApplyPasswordWithoutAuthUsesNoAppliedSecret(t *testing.T) {
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	ms := &mK8SService.Services{}
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", rf, "", []string{""}).Return(true, nil)
	mrfh.On("ApplySentinelPassword", rf, "").Return(true, nil)
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, log.Dummy)

	for range 3 {
		require.NoError(t, handler.applyPassword(rf))
	}
	mrfh.AssertNumberOfCalls(t, "ApplyPassword", 1)
	ms.AssertNotCalled(t, "GetSecret", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
}
