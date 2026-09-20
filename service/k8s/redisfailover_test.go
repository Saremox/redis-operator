package k8s_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	redisfailoverfake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

func TestRedisFailoverServiceListRedisFailovers(t *testing.T) {
	testns := "testns"

	rf1 := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rf1",
			Namespace: testns,
		},
	}
	rf2 := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rf2",
			Namespace: testns,
		},
	}
	otherNsRf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rf3",
			Namespace: "otherns",
		},
	}

	crdcli := redisfailoverfake.NewSimpleClientset(rf1, rf2, otherNsRf)
	service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

	list, err := service.ListRedisFailovers(context.TODO(), testns, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, list)
	assert.Len(t, list.Items, 2)

	gotNames := map[string]bool{}
	for _, rf := range list.Items {
		gotNames[rf.Name] = true
	}
	assert.True(t, gotNames["rf1"])
	assert.True(t, gotNames["rf2"])
	assert.False(t, gotNames["rf3"], "redisfailover in a different namespace must be excluded")
}

func TestRedisFailoverServiceWatchRedisFailovers(t *testing.T) {
	testns := "testns"

	crdcli := redisfailoverfake.NewSimpleClientset()
	service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

	watcher, err := service.WatchRedisFailovers(context.TODO(), testns, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, watcher)
	watcher.Stop()
}

func TestRedisFailoverServiceUpdateRedisFailoverStatus(t *testing.T) {
	testns := "testns"

	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rf1",
			Namespace: testns,
		},
		Status: redisfailoverv1.RedisFailoverStatus{
			State:       redisfailoverv1.HealthyState,
			LastChanged: "2026-08-23T00:00:00Z",
			Message:     "all good",
		},
	}

	t.Run("patches the status of an existing RedisFailover", func(t *testing.T) {
		crdcli := redisfailoverfake.NewSimpleClientset(rf)
		service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

		// UpdateRedisFailoverStatus does not return an error, it only logs on
		// failure, so we assert it does not panic and check the patch went
		// through via the underlying clientset.
		assert.NotPanics(t, func() {
			service.UpdateRedisFailoverStatus(context.TODO(), testns, rf, metav1.PatchOptions{})
		})

		got, err := crdcli.DatabasesV1().RedisFailovers(testns).Get(context.TODO(), "rf1", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Equal(t, redisfailoverv1.HealthyState, got.Status.State)
	})

	t.Run("does not panic when patching a non-existent RedisFailover", func(t *testing.T) {
		crdcli := redisfailoverfake.NewSimpleClientset()
		service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

		assert.NotPanics(t, func() {
			service.UpdateRedisFailoverStatus(context.TODO(), testns, rf, metav1.PatchOptions{})
		})
	})

	t.Run("a message containing quotes and backslashes is patched verbatim, not mangled into invalid JSON", func(t *testing.T) {
		// The patch used to be built with fmt.Sprintf directly into a JSON
		// string literal: a Message like this one would have produced invalid
		// JSON and made the whole patch call fail silently (logged, not
		// returned). json.Marshal escapes it correctly instead.
		trickyRF := &redisfailoverv1.RedisFailover{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rf-tricky",
				Namespace: testns,
			},
			Status: redisfailoverv1.RedisFailoverStatus{
				State:       redisfailoverv1.NotHealthyState,
				LastChanged: "2026-08-23T00:00:00Z",
				Message:     `error: "connection refused" on host\path`,
			},
		}

		crdcli := redisfailoverfake.NewSimpleClientset(trickyRF)
		service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

		assert.NotPanics(t, func() {
			service.UpdateRedisFailoverStatus(context.TODO(), testns, trickyRF, metav1.PatchOptions{})
		})

		got, err := crdcli.DatabasesV1().RedisFailovers(testns).Get(context.TODO(), "rf-tricky", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Equal(t, trickyRF.Status.Message, got.Status.Message)
	})
}

func TestRedisFailoverServicePatchRedisFailoverFinalizers(t *testing.T) {
	testns := "testns"

	rf := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rf1",
			Namespace:  testns,
			Finalizers: []string{"some.other/finalizer"},
		},
	}

	t.Run("patches the finalizers of an existing RedisFailover", func(t *testing.T) {
		crdcli := redisfailoverfake.NewSimpleClientset(rf)
		service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

		err := service.PatchRedisFailoverFinalizers(context.TODO(), testns, rf.Name,
			[]string{"some.other/finalizer", "redisfailovers.databases.spotahome.com/finalizer"}, metav1.PatchOptions{})
		assert.NoError(t, err)

		got, err := crdcli.DatabasesV1().RedisFailovers(testns).Get(context.TODO(), rf.Name, metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Equal(t, []string{"some.other/finalizer", "redisfailovers.databases.spotahome.com/finalizer"}, got.Finalizers)
	})

	t.Run("a nil finalizers list patches to an empty array rather than leaving finalizers untouched", func(t *testing.T) {
		crdcli := redisfailoverfake.NewSimpleClientset(rf)
		service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

		err := service.PatchRedisFailoverFinalizers(context.TODO(), testns, rf.Name, nil, metav1.PatchOptions{})
		assert.NoError(t, err)

		got, err := crdcli.DatabasesV1().RedisFailovers(testns).Get(context.TODO(), rf.Name, metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Empty(t, got.Finalizers)
	})

	t.Run("returns an error when patching a non-existent RedisFailover", func(t *testing.T) {
		crdcli := redisfailoverfake.NewSimpleClientset()
		service := k8s.NewRedisFailoverService(crdcli, log.Dummy, metrics.Dummy)

		err := service.PatchRedisFailoverFinalizers(context.TODO(), testns, "does-not-exist", []string{"some/finalizer"}, metav1.PatchOptions{})
		assert.Error(t, err)
	})
}
