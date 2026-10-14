package redisfailover

import (
	"errors"
	"fmt"
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
// in running, one for each pod IP. With pending, one more pod is yet to start.
// The template of a pod in stale names the Secret in stale, and the template
// of every other pod is the one of rf. The key of the pod yet to start is
// "pending". A pod in down is unreachable.
type appliedSecretEnv struct {
	rf       *redisfailoverv1.RedisFailover
	ms       *mK8SService.Services
	password string
	running  map[string]string
	pending  bool
	stale    map[string]string
	down     map[string]bool
	stored   map[string]*corev1.Secret
	tried    []string
}

func (e *appliedSecretEnv) template(key string) corev1.PodSpec {
	secretPath, ok := e.stale[key]
	if !ok {
		secretPath = e.rf.Spec.Auth.SecretPath
	}
	return corev1.PodSpec{Containers: []corev1.Container{redisContainer(secretPath)}}
}

// redisContainer returns the Redis container of a pod template that gives
// redis-server the password of the Secret secretPath.
func redisContainer(secretPath string) corev1.Container {
	c := corev1.Container{Name: "redis"}
	if secretPath != "" {
		c.Env = []corev1.EnvVar{{Name: "REDIS_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secretPath}, Key: "password"},
		}}}
	}
	return c
}

// authError returns the error of a Redis that runs with running and gets
// password, as go-redis gives it.
func authError(running, password string) error {
	switch {
	case running == password:
		return nil
	case running == "":
		return errors.New("ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?")
	case password == "":
		return errors.New("NOAUTH Authentication required.")
	}
	return errors.New("WRONGPASS invalid username-password pair or user is disabled.")
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
		pending:  pending,
		stale:    map[string]string{},
		down:     map[string]bool{},
	}
	e.ms.On("GetSecret", "testns", "redis-auth").Return(func(string, string) (*corev1.Secret, error) {
		return &corev1.Secret{Data: map[string][]byte{"password": []byte(e.password)}}, nil
	})
	e.ms.On("GetStatefulSetPods", "testns", rfservice.GetRedisName(e.rf)).Return(func(string, string) (*corev1.PodList, error) {
		pods := &corev1.PodList{}
		for ip := range e.running {
			pods.Items = append(pods.Items, corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-" + ip},
				Spec:       e.template(ip),
				Status:     corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning},
			})
		}
		if e.pending {
			pods.Items = append(pods.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rfr-test-pending"}, Spec: e.template("pending"), Status: corev1.PodStatus{Phase: corev1.PodPending}})
		}
		return pods, nil
	})
	e.ms.On("GetDeploymentPods", "testns", rfservice.GetSentinelName(e.rf)).Return(&corev1.PodList{}, nil)
	e.stored = mockAppliedSecret(e.ms)
	return e
}

// handler returns a handler with no passwordState, as after an operator start.
func (e *appliedSecretEnv) handler(logger log.Logger) *RedisFailoverHandler {
	mr := &mRedisService.Client{}
	mr.On("IsMaster", mock.Anything, "0", mock.Anything).Return(func(ip, _, pw string) (bool, error) {
		if e.down[ip] {
			return false, errors.New("dial tcp " + ip + ":0: i/o timeout")
		}
		err := authError(e.running[ip], pw)
		return err == nil, err
	})
	mr.On("SetPassword", mock.Anything, "0", mock.Anything, mock.Anything).Return(func(ip, _, pw, newPw string) error {
		e.tried = append(e.tried, ip+":"+pw)
		if err := authError(e.running[ip], pw); err != nil {
			return err
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
			mrfh.On("ApplyPassword", rf, "v2", test.want).Once().Return(true, false, nil)
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
	mrfh.On("ApplyPassword", rf, mock.Anything, mock.Anything).Return(false, false, nil)
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

// TestApplyPasswordRetriesAFailedSave covers a temporary API error:
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
	assert.False(t, v.(passwordState).retrySecret)
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
			require.ErrorContains(t, handler.applyPassword(e.rf), "put the previous password back")
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
	assert.Empty(t, e.running["10.0.0.1"])
	redis, applied := e.applied()
	assert.Equal(t, []string{"", ""}, []string{redis, applied})
	assert.Equal(t, 2, e.writes())
}

// TestApplyPasswordGoesOnWhenTheReadFails covers a read error other than
// NotFound: the operator logs it, tries only the configured password, and
// does not replace the Secret that it could not read. After a temporary error,
// it keeps no state, so that the next reconcile reads the Secret again. After
// Forbidden, a read at each reconcile cannot succeed.
func TestApplyPasswordGoesOnWhenTheReadFails(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		cached  bool
		applies int
	}{
		{name: "temporary error", err: errors.New("boom"), applies: 2},
		{name: "forbidden", err: apierrors.NewForbidden(corev1.Resource("secrets"), "rfr-auth-applied-test", errors.New("no get")), cached: true, applies: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := &redisfailoverv1.RedisFailover{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"},
				Spec:       redisfailoverv1.RedisFailoverSpec{Auth: redisfailoverv1.AuthSettings{SecretPath: "redis-auth"}},
			}
			ms := &mK8SService.Services{}
			ms.On("GetSecret", "testns", "redis-auth").Return(&corev1.Secret{Data: map[string][]byte{"password": []byte("v1")}}, nil)
			ms.On("GetSecret", "testns", "rfr-auth-applied-test").Return(nil, test.err)
			ms.On("CreateOrUpdateSecret", "testns", mock.Anything).Return(nil)
			mrfh := &mRFService.RedisFailoverHeal{}
			mrfh.On("ApplyPassword", rf, "v1", []string{"v1"}).Return(true, false, nil)
			mrfh.On("ApplySentinelPassword", rf, "v1").Return(true, nil)
			logger := &warnLogger{}
			handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, logger)

			require.NoError(t, handler.applyPassword(rf))
			_, cached := handler.passwords.Load(passwordKey(rf))
			assert.Equal(t, test.cached, cached)
			require.NoError(t, handler.applyPassword(rf))

			mrfh.AssertNumberOfCalls(t, "ApplyPassword", test.applies)
			ms.AssertNumberOfCalls(t, "GetSecret", 2+test.applies)
			ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
			assert.Len(t, logger.warnings, 1)
		})
	}
}

// TestApplyPasswordReadErrorWithAPendingPod covers a read error at the first
// reconcile after a restart, while a pod yet to start keeps the saved redis
// password. The next reconcile must not replace that password in the Secret,
// and the pod must change when it starts with it.
func TestApplyPasswordReadErrorWithAPendingPod(t *testing.T) {
	e := newAppliedSecretEnv(t, "v2", map[string]string{"10.0.0.1": "v2"}, true)
	e.storeApplied("v1", "v2")
	e.ms.On("GetSecret", "testns", "rfr-auth-applied-test").Once().Return(nil, errors.New("etcdserver: request timed out"))
	e.ms.ExpectedCalls = append(e.ms.ExpectedCalls[len(e.ms.ExpectedCalls)-1:], e.ms.ExpectedCalls[:len(e.ms.ExpectedCalls)-1]...)
	handler := e.handler(log.Dummy)

	require.NoError(t, handler.applyPassword(e.rf))
	require.NoError(t, handler.applyPassword(e.rf))
	redis, applied := e.applied()
	assert.Equal(t, []string{"v1", "v2"}, []string{redis, applied})

	e.pending = false
	e.running["10.0.0.2"] = "v1"
	require.NoError(t, handler.applyPassword(e.rf))
	assert.Equal(t, "v2", e.running["10.0.0.2"])
}

// TestApplyPasswordLogsAnErrorAgainAfterASuccess covers the log of the same
// error after a successful read or write between them: it is a new Warning.
func TestApplyPasswordLogsAnErrorAgainAfterASuccess(t *testing.T) {
	boom := errors.New("etcdserver: request timed out")
	for _, found := range []bool{true, false} {
		t.Run(fmt.Sprintf("read, found %t", found), func(t *testing.T) {
			e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, false)
			if found {
				e.storeApplied("v1", "v1")
			} else {
				// Without a password, a missing Secret needs no write.
				e.rf.Spec.Auth.SecretPath = ""
				e.running["10.0.0.1"] = ""
			}
			failRead := true
			e.ms.On("GetSecret", "testns", "rfr-auth-applied-test").Return(func(_, name string) (*corev1.Secret, error) {
				if failRead {
					return nil, boom
				}
				if s, ok := e.stored[name]; ok {
					return s, nil
				}
				return nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
			})
			e.ms.ExpectedCalls = append(e.ms.ExpectedCalls[len(e.ms.ExpectedCalls)-1:], e.ms.ExpectedCalls[:len(e.ms.ExpectedCalls)-1]...)
			logger := &warnLogger{}
			handler := e.handler(logger)

			require.NoError(t, handler.applyPassword(e.rf))
			failRead = false
			require.NoError(t, handler.applyPassword(e.rf))
			assert.Equal(t, 0, e.writes())
			failRead = true
			handler.passwords.Delete(passwordKey(e.rf))
			require.NoError(t, handler.applyPassword(e.rf))
			assert.Len(t, logger.warnings, 2)
		})
	}
	t.Run("write", func(t *testing.T) {
		e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, false)
		failWrite := true
		e.ms.ExpectedCalls = slices.DeleteFunc(e.ms.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "CreateOrUpdateSecret" })
		e.ms.On("CreateOrUpdateSecret", "testns", mock.Anything).Return(func(string, *corev1.Secret) error {
			if failWrite {
				return boom
			}
			return nil
		})
		logger := &warnLogger{}
		handler := e.handler(logger)

		require.NoError(t, handler.applyPassword(e.rf))
		failWrite = false
		require.NoError(t, handler.applyPassword(e.rf))
		failWrite = true
		e.password = "v2"
		require.NoError(t, handler.applyPassword(e.rf))
		assert.Equal(t, 3, e.writes())
		assert.Len(t, logger.warnings, 2)
	})
}

// TestApplyPasswordWithoutAuthUsesNoAppliedSecret covers a RedisFailover that
// never had a password: one read at the operator start, and nothing more.
func TestApplyPasswordWithoutAuthUsesNoAppliedSecret(t *testing.T) {
	rf := &redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "testns"}}
	ms := noAppliedSecret(&mK8SService.Services{})
	mrfh := &mRFService.RedisFailoverHeal{}
	mrfh.On("ApplyPassword", rf, "", []string{""}).Return(true, false, nil)
	mrfh.On("ApplySentinelPassword", rf, "").Return(true, nil)
	logger := &warnLogger{}
	handler := NewRedisFailoverHandler(Config{}, &mRFService.RedisFailoverClient{}, &mRFService.RedisFailoverCheck{}, mrfh, ms, metrics.Dummy, logger)

	for range 3 {
		require.NoError(t, handler.applyPassword(rf))
	}
	mrfh.AssertNumberOfCalls(t, "ApplyPassword", 1)
	ms.AssertNumberOfCalls(t, "GetSecret", 1)
	ms.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything)
	assert.Empty(t, logger.warnings)
}

// TestApplyPasswordRestartDuringARemoval covers an operator restart after the
// removal of auth.secretPath reached the running pods but not a pod yet to
// start. That pod comes up with the old password, and the operator must
// still know it.
func TestApplyPasswordRestartDuringARemoval(t *testing.T) {
	e := newAppliedSecretEnv(t, "", map[string]string{"10.0.0.1": "v1"}, true)
	e.rf.Spec.Auth.SecretPath = ""
	e.storeApplied("v1", "v1")

	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))
	assert.Empty(t, e.running["10.0.0.1"])
	redis, applied := e.applied()
	assert.Equal(t, []string{"v1", ""}, []string{redis, applied})

	// After a second restart, the pod starts with the old password.
	handler := e.handler(log.Dummy)
	require.NoError(t, handler.applyPassword(e.rf))
	e.pending = false
	e.running["10.0.0.2"] = "v1"
	require.NoError(t, handler.applyPassword(e.rf))
	assert.Equal(t, map[string]string{"10.0.0.1": "", "10.0.0.2": ""}, e.running)
	redis, applied = e.applied()
	assert.Equal(t, []string{"", ""}, []string{redis, applied})
}

// TestApplyPasswordRetriesAFailedRead covers a read error that goes away while
// every pod accepts the password: the next reconcile reads and writes the
// Secret, and then the operator makes no more calls.
func TestApplyPasswordRetriesAFailedRead(t *testing.T) {
	e := newAppliedSecretEnv(t, "v2", map[string]string{"10.0.0.1": "v2"}, false)
	e.storeApplied("v1", "v1")
	e.ms.On("GetSecret", "testns", "rfr-auth-applied-test").Once().Return(nil, errors.New("etcdserver: request timed out"))
	e.ms.ExpectedCalls = append(e.ms.ExpectedCalls[len(e.ms.ExpectedCalls)-1:], e.ms.ExpectedCalls[:len(e.ms.ExpectedCalls)-1]...)
	logger := &warnLogger{}
	handler := e.handler(logger)

	for range 4 {
		require.NoError(t, handler.applyPassword(e.rf))
	}
	redis, applied := e.applied()
	assert.Equal(t, []string{"v2", "v2"}, []string{redis, applied})
	assert.Equal(t, 1, e.writes())
	reads := 0
	for _, c := range e.ms.Calls {
		if c.Method == "GetSecret" && c.Arguments[1] == "rfr-auth-applied-test" {
			reads++
		}
	}
	assert.Equal(t, 2, reads)
	assert.Len(t, logger.warnings, 1)
}

// TestApplyPasswordDoesNotRetryAForbiddenSave covers an operator without the
// right to write Secrets: a retry at each reconcile cannot succeed.
func TestApplyPasswordDoesNotRetryAForbiddenSave(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, false)
	forbidden := apierrors.NewForbidden(corev1.Resource("secrets"), "rfr-auth-applied-test", errors.New("no create"))
	e.ms.ExpectedCalls = slices.DeleteFunc(e.ms.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "CreateOrUpdateSecret" })
	e.ms.On("CreateOrUpdateSecret", "testns", mock.Anything).Return(forbidden)
	handler := e.handler(log.Dummy)

	for range 3 {
		require.NoError(t, handler.applyPassword(e.rf))
	}
	assert.Equal(t, 1, e.writes())
}

// remove removes auth.secretPath. The pods keep the template that names the
// Secret redis-auth until the rollout.
func (e *appliedSecretEnv) remove() {
	e.rf.Spec.Auth.SecretPath = ""
	for ip := range e.running {
		e.stale[ip] = "redis-auth"
	}
}

func (e *appliedSecretEnv) podLists() int {
	n := 0
	for _, c := range e.ms.Calls {
		if c.Method == "GetStatefulSetPods" {
			n++
		}
	}
	return n
}

// forbidSave makes the operator unable to write the applied Secret.
func (e *appliedSecretEnv) forbidSave() {
	e.ms.ExpectedCalls = slices.DeleteFunc(e.ms.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "CreateOrUpdateSecret" })
	e.ms.On("CreateOrUpdateSecret", "testns", mock.Anything).Return(apierrors.NewForbidden(corev1.Resource("secrets"), "rfr-auth-applied-test", errors.New("no create")))
}

// TestApplyPasswordRemovalWithoutAppliedSecretUsesThePodSecretRef covers the
// removal of auth.secretPath at the first reconcile of an operator that has no
// applied Secret, for example after an upgrade. The pods run with the password
// of the Secret that their template names.
func TestApplyPasswordRemovalWithoutAppliedSecretUsesThePodSecretRef(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1", "10.0.0.2": "v1"}, false)
	e.remove()

	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))
	assert.Equal(t, map[string]string{"10.0.0.1": "", "10.0.0.2": ""}, e.running)
	assert.ElementsMatch(t, []string{"10.0.0.1:v1", "10.0.0.2:v1"}, e.tried)
}

// TestApplyPasswordRemovalWithoutAppliedSecretOrPodSecretFails covers a
// removal that no known password can apply: the error tells how to recover.
func TestApplyPasswordRemovalWithoutAppliedSecretOrPodSecretFails(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, false)
	e.remove()
	e.ms.On("GetSecret", "testns", "redis-auth").Return(nil, apierrors.NewNotFound(corev1.Resource("secrets"), "redis-auth"))
	e.ms.ExpectedCalls = append(e.ms.ExpectedCalls[len(e.ms.ExpectedCalls)-1:], e.ms.ExpectedCalls[:len(e.ms.ExpectedCalls)-1]...)

	err := e.handler(log.Dummy).applyPassword(e.rf)
	require.EqualError(t, err, "redis pod rfr-test-10.0.0.1 refuses the configured password and the operator doesn't know the one it runs with; set auth.secretPath to a Secret with the previous password until the RedisFailover is healthy, then remove it again")
	assert.Equal(t, "v1", e.running["10.0.0.1"])
}

// TestApplyPasswordRemovalForbiddenSave covers an operator without the right
// to write Secrets that restarts before the removal of auth.secretPath.
func TestApplyPasswordRemovalForbiddenSave(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1", "10.0.0.2": "v1"}, false)
	e.forbidSave()
	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))

	e.remove()
	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))
	assert.Equal(t, map[string]string{"10.0.0.1": "", "10.0.0.2": ""}, e.running)
	assert.Empty(t, e.stored)
}

// TestApplyPasswordKeepsThePreviousPasswordUntilTheRollout covers the state
// during the rollout after a removal: a pod on the old template can restart
// with the old password, so the operator keeps it, also in the applied Secret.
// After the rollout, a reconcile lists no pods.
func TestApplyPasswordKeepsThePreviousPasswordUntilTheRollout(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1", "10.0.0.2": "v1"}, false)
	handler := e.handler(log.Dummy)
	require.NoError(t, handler.applyPassword(e.rf))

	e.remove()
	for range 2 {
		require.NoError(t, handler.applyPassword(e.rf))
	}
	redis, applied := e.applied()
	assert.Equal(t, []string{"v1", ""}, []string{redis, applied})

	delete(e.stale, "10.0.0.1")
	require.NoError(t, handler.applyPassword(e.rf))
	redis, _ = e.applied()
	assert.Equal(t, "v1", redis)

	delete(e.stale, "10.0.0.2")
	require.NoError(t, handler.applyPassword(e.rf))
	redis, applied = e.applied()
	assert.Equal(t, []string{"", ""}, []string{redis, applied})
	lists := e.podLists()
	require.NoError(t, handler.applyPassword(e.rf))
	assert.Equal(t, lists, e.podLists())
}

// TestApplyPasswordFixesAStalePodThatRestartedAfterARemoval covers a container
// restart of a pod on the old template after the removal of auth.secretPath:
// Redis starts again with the old password, and the operator removes it again.
func TestApplyPasswordFixesAStalePodThatRestartedAfterARemoval(t *testing.T) {
	tests := []struct {
		name      string
		restart   bool
		forbidden bool
	}{
		{name: "same process"},
		{name: "after an operator restart", restart: true},
		{name: "after an operator restart without the applied Secret", restart: true, forbidden: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1", "10.0.0.2": "v1"}, false)
			if test.forbidden {
				e.forbidSave()
			}
			handler := e.handler(log.Dummy)
			require.NoError(t, handler.applyPassword(e.rf))
			e.remove()
			require.NoError(t, handler.applyPassword(e.rf))
			if test.restart {
				handler = e.handler(log.Dummy)
				require.NoError(t, handler.applyPassword(e.rf))
			}

			e.running["10.0.0.2"] = "v1"
			require.NoError(t, handler.applyPassword(e.rf))
			assert.Equal(t, map[string]string{"10.0.0.1": "", "10.0.0.2": ""}, e.running)
		})
	}
}

// TestApplyPasswordFixesAStalePodThatRestartedAfterAnAddition covers a
// container restart of a pod on the old template after the addition of
// auth.secretPath: Redis starts again without a password, and the operator sets
// it again.
func TestApplyPasswordFixesAStalePodThatRestartedAfterAnAddition(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("operator restart %t", restart), func(t *testing.T) {
			e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "", "10.0.0.2": ""}, false)
			e.rf.Spec.Auth.SecretPath = ""
			handler := e.handler(log.Dummy)
			require.NoError(t, handler.applyPassword(e.rf))

			e.rf.Spec.Auth.SecretPath = "redis-auth"
			e.stale = map[string]string{"10.0.0.1": "", "10.0.0.2": ""}
			require.NoError(t, handler.applyPassword(e.rf))
			if restart {
				handler = e.handler(log.Dummy)
				require.NoError(t, handler.applyPassword(e.rf))
			}

			e.running["10.0.0.2"] = ""
			require.NoError(t, handler.applyPassword(e.rf))
			assert.Equal(t, map[string]string{"10.0.0.1": "v1", "10.0.0.2": "v1"}, e.running)
		})
	}
}

// TestApplyPasswordKeepsTheFastPathWithAPodOnTheCurrentTemplate covers a pod
// yet to start or an unreachable pod on the current template, without a
// password change: after the first reconcile, the operator lists no pods.
func TestApplyPasswordKeepsTheFastPathWithAPodOnTheCurrentTemplate(t *testing.T) {
	for _, auth := range []bool{true, false} {
		for _, pending := range []bool{true, false} {
			t.Run(fmt.Sprintf("auth %t, pending %t", auth, pending), func(t *testing.T) {
				password := ""
				if auth {
					password = "v1"
				}
				e := newAppliedSecretEnv(t, password, map[string]string{"10.0.0.1": password, "10.0.0.2": password}, pending)
				if !auth {
					e.rf.Spec.Auth.SecretPath = ""
				}
				if !pending {
					e.down["10.0.0.2"] = true
				}
				handler := e.handler(log.Dummy)
				for range 6 {
					require.NoError(t, handler.applyPassword(e.rf))
				}
				assert.Equal(t, 1, e.podLists())
			})
		}
	}
}

// TestApplyPasswordFixesAStalePodYetToStartAfterARemoval covers a pod on the
// old template that starts after the removal of auth.secretPath, while the
// operator knows no old password. The operator checks the pods until this
// pod runs, and then removes its password.
func TestApplyPasswordFixesAStalePodYetToStartAfterARemoval(t *testing.T) {
	e := newAppliedSecretEnv(t, "v1", map[string]string{"10.0.0.1": "v1"}, true)
	e.forbidSave()
	require.NoError(t, e.handler(log.Dummy).applyPassword(e.rf))

	e.remove()
	e.stale["pending"] = "redis-auth"
	handler := e.handler(log.Dummy)
	require.NoError(t, handler.applyPassword(e.rf))
	delete(e.stale, "10.0.0.1")
	require.NoError(t, handler.applyPassword(e.rf))

	e.pending = false
	e.running["10.0.0.2"] = "v1"
	e.stale["10.0.0.2"] = "redis-auth"
	require.NoError(t, handler.applyPassword(e.rf))
	assert.Equal(t, map[string]string{"10.0.0.1": "", "10.0.0.2": ""}, e.running)
}
