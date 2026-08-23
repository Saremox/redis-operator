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
}
