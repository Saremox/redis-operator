package redisfailover

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	fakekubernetes "k8s.io/client-go/kubernetes/fake"

	kooperlog "github.com/spotahome/kooper/v2/log"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	mRedisService "github.com/saremox/redis-operator/mocks/service/redis"
)

// This file is deliberately in package redisfailover (white-box) rather than
// redisfailover_test, because kooperlogger and its WithKV method are
// unexported and can't be reached from outside the package.

// -----------------------------------------------------------------------
// NewRedisFailoverRetriever - List
// -----------------------------------------------------------------------

func TestNewRedisFailoverRetrieverListFiltersByNamespace(t *testing.T) {
	cfg := Config{SupportedNamespacesRegex: "^allowed$"}
	mk := &mK8SService.Services{}

	rfList := &redisfailoverv1.RedisFailoverList{
		Items: []redisfailoverv1.RedisFailover{
			{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "allowed"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "other"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "allowed"}},
		},
	}
	mk.On("ListRedisFailovers", mock.Anything, "", mock.Anything).Once().Return(rfList, nil)

	retriever := NewRedisFailoverRetriever(cfg, mk)
	obj, err := retriever.List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	got, ok := obj.(*redisfailoverv1.RedisFailoverList)
	require.True(t, ok)

	names := make([]string, 0, len(got.Items))
	for _, rf := range got.Items {
		names = append(names, rf.Name)
	}
	assert.Equal(t, []string{"a", "c"}, names)
	mk.AssertExpectations(t)
}

func TestNewRedisFailoverRetrieverListPropagatesError(t *testing.T) {
	cfg := Config{SupportedNamespacesRegex: ".*"}
	mk := &mK8SService.Services{}
	wantErr := errors.New("boom")

	mk.On("ListRedisFailovers", mock.Anything, "", mock.Anything).Once().Return(nil, wantErr)

	retriever := NewRedisFailoverRetriever(cfg, mk)
	obj, err := retriever.List(context.Background(), metav1.ListOptions{})

	assert.Equal(t, wantErr, err)
	assert.Nil(t, obj)
	mk.AssertExpectations(t)
}

// -----------------------------------------------------------------------
// NewRedisFailoverRetriever - Watch
// -----------------------------------------------------------------------

func TestNewRedisFailoverRetrieverWatchFiltersByNamespace(t *testing.T) {
	cfg := Config{SupportedNamespacesRegex: "^allowed$"}
	mk := &mK8SService.Services{}

	fakeWatcher := watch.NewFake()
	mk.On("WatchRedisFailovers", mock.Anything, "", mock.Anything).Once().Return(fakeWatcher, nil)

	retriever := NewRedisFailoverRetriever(cfg, mk)
	w, err := retriever.Watch(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.NotNil(t, w)

	go func() {
		// An object of an unexpected type (not *RedisFailover) must be
		// dropped by the type assertion in the filter func rather than
		// panicking or passing through.
		fakeWatcher.Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "not-a-redisfailover"}})
		fakeWatcher.Add(&redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "other"}})
		fakeWatcher.Add(&redisfailoverv1.RedisFailover{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "allowed"}})
		fakeWatcher.Stop()
	}()

	var received []redisfailoverv1.RedisFailover
	for event := range w.ResultChan() {
		rf, ok := event.Object.(*redisfailoverv1.RedisFailover)
		require.True(t, ok)
		received = append(received, *rf)
	}

	require.Len(t, received, 1)
	assert.Equal(t, "allowed", received[0].Namespace)
	assert.Equal(t, "a", received[0].Name)
	mk.AssertExpectations(t)
}

func TestNewRedisFailoverRetrieverWatchPropagatesError(t *testing.T) {
	cfg := Config{SupportedNamespacesRegex: ".*"}
	mk := &mK8SService.Services{}
	wantErr := errors.New("boom")

	mk.On("WatchRedisFailovers", mock.Anything, "", mock.Anything).Once().Return(nil, wantErr)

	retriever := NewRedisFailoverRetriever(cfg, mk)
	w, err := retriever.Watch(context.Background(), metav1.ListOptions{})

	assert.Equal(t, wantErr, err)
	assert.Nil(t, w)
	mk.AssertExpectations(t)
}

// TestNewRedisFailoverRetrieverWatchNilWatcherNoError mirrors the historical
// nil-pointer panic bug in WatchFunc: WatchRedisFailovers can return a nil
// watch.Interface with a nil error. The current code guards against passing
// that nil watcher into watch.Filter, so this must not panic.
func TestNewRedisFailoverRetrieverWatchNilWatcherNoError(t *testing.T) {
	cfg := Config{SupportedNamespacesRegex: ".*"}
	mk := &mK8SService.Services{}

	mk.On("WatchRedisFailovers", mock.Anything, "", mock.Anything).Once().Return(nil, nil)

	retriever := NewRedisFailoverRetriever(cfg, mk)

	var w watch.Interface
	var err error
	assert.NotPanics(t, func() {
		w, err = retriever.Watch(context.Background(), metav1.ListOptions{})
	})
	assert.NoError(t, err)
	assert.Nil(t, w)
	mk.AssertExpectations(t)
}

// -----------------------------------------------------------------------
// kooperlogger.WithKV
// -----------------------------------------------------------------------

// kvCapturingLogger is a minimal log.Logger test double that records the
// values passed to WithFields so we can assert kooperlogger.WithKV forwards
// its kooperlog.KV argument correctly.
type kvCapturingLogger struct {
	log.DummyLogger
	lastKV map[string]interface{}
}

func (l *kvCapturingLogger) WithFields(values map[string]interface{}) log.Logger {
	l.lastKV = values
	return l
}

func TestKooperLoggerWithKV(t *testing.T) {
	fake := &kvCapturingLogger{}
	kl := kooperlogger{Logger: fake}

	kv := kooperlog.KV{"foo": "bar", "n": 1}
	result := kl.WithKV(kv)

	wrapped, ok := result.(kooperlogger)
	require.True(t, ok)

	assert.Equal(t, map[string]interface{}{"foo": "bar", "n": 1}, fake.lastKV)
	assert.Same(t, fake, wrapped.Logger)
}

// -----------------------------------------------------------------------
// New
// -----------------------------------------------------------------------

// TestNewBuildsController exercises the real construction path of New()
// using a fake kubernetes.Interface (satisfies what
// leaderelection.NewDefault needs to build its resourcelock.Interface: only
// CoreV1()/CoordinationV1() clients, no live API calls happen during
// construction) and mocked k8s.Services/redis.Client (New only stores these,
// it never calls methods on them until the controller actually Runs).
func TestNewBuildsController(t *testing.T) {
	cfg := Config{
		SyncInterval:             30,
		Concurrency:              2,
		SupportedNamespacesRegex: ".*",
	}
	mk := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	k8sClient := fakekubernetes.NewClientset()

	ctrl, err := New(cfg, mk, k8sClient, "test-namespace", mr, metrics.Dummy, log.Dummy)

	require.NoError(t, err)
	assert.NotNil(t, ctrl)
	mk.AssertExpectations(t)
	mr.AssertExpectations(t)
}

// TestNewPropagatesLeaderElectionError covers the error path returned by
// leaderelection.NewDefault: it validates its namespace argument and errors
// out when it's empty, before ever touching the kubernetes client. This lets
// us deterministically trigger New()'s `if err != nil { return nil, err }`
// branch without needing a live cluster.
func TestNewPropagatesLeaderElectionError(t *testing.T) {
	cfg := Config{
		SyncInterval:             30,
		Concurrency:              2,
		SupportedNamespacesRegex: ".*",
	}
	mk := &mK8SService.Services{}
	mr := &mRedisService.Client{}
	k8sClient := fakekubernetes.NewClientset()

	ctrl, err := New(cfg, mk, k8sClient, "", mr, metrics.Dummy, log.Dummy)

	assert.Error(t, err)
	assert.Nil(t, ctrl)
	mk.AssertExpectations(t)
	mr.AssertExpectations(t)
}
