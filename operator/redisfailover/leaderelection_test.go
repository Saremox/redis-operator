package redisfailover

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekubernetes "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/saremox/redis-operator/log"
)

func TestNewLeaseRunnerValidates(t *testing.T) {
	_, err := newLeaseRunner(lockKey, "", fakekubernetes.NewClientset(), log.Dummy)
	assert.EqualError(t, err, "running in leader election mode requires the namespace running")

	_, err = newLeaseRunner("", "ns", fakekubernetes.NewClientset(), log.Dummy)
	assert.EqualError(t, err, "running in leader election mode requires a key for identification the different instances")

	defer func(h func() (string, error)) { hostname = h }(hostname)
	hostname = func() (string, error) { return "", errors.New("no hostname") }
	_, err = newLeaseRunner(lockKey, "ns", fakekubernetes.NewClientset(), log.Dummy)
	assert.EqualError(t, err, "no hostname")
}

func TestNewLeaseRunnerKeepsKooperTimings(t *testing.T) {
	r, err := newLeaseRunner(lockKey, "ns", fakekubernetes.NewClientset(), log.Dummy)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, r.leaseDuration)
	assert.Equal(t, 10*time.Second, r.renewDeadline)
	assert.Equal(t, 2*time.Second, r.retryPeriod)
}

func fastLeaseRunner(t *testing.T, kube *fakekubernetes.Clientset) *leaseRunner {
	t.Helper()
	r, err := newLeaseRunner(lockKey, "ns", kube, log.Dummy)
	require.NoError(t, err)
	r.leaseDuration, r.renewDeadline, r.retryPeriod = 300*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond
	return r
}

func leaseHolder(t *testing.T, kube *fakekubernetes.Clientset) string {
	t.Helper()
	lease, err := kube.CoordinationV1().Leases("ns").Get(context.Background(), lockKey, metav1.GetOptions{})
	require.NoError(t, err)
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

func TestLeaseRunnerReleasesTheLeaseWhenFReturns(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	r := fastLeaseRunner(t, kube)

	var holder string
	err := r.Run(context.Background(), func(context.Context) error {
		holder = leaseHolder(t, kube)
		return errors.New("done")
	})

	assert.EqualError(t, err, "done")
	assert.Equal(t, r.lock.Identity(), holder)
	assert.Empty(t, leaseHolder(t, kube))
}

func TestLeaseRunnerStopsFThenReleasesTheLeaseOnShutdown(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	r := fastLeaseRunner(t, kube)
	ctx, cancel := context.WithCancel(context.Background())

	var holderWhenStopped string
	err := r.Run(ctx, func(runCtx context.Context) error {
		cancel()
		<-runCtx.Done()
		holderWhenStopped = leaseHolder(t, kube)
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, r.lock.Identity(), holderWhenStopped, "the lease is held until f has returned")
	assert.Empty(t, leaseHolder(t, kube))
	events, err := kube.CoreV1().Events("").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, events.Items, "like kooper, no leader election events are written")
}

func TestLeaseRunnerReturnsAtOnceWhenLeadershipIsLost(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	r := fastLeaseRunner(t, kube)
	var leading atomic.Bool
	kube.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if leading.Load() {
			return true, nil, apierrors.NewServiceUnavailable("down")
		}
		return false, nil, nil
	})
	fStopping := make(chan struct{})
	finishF := make(chan struct{})
	defer close(finishF)

	err := r.Run(context.Background(), func(runCtx context.Context) error {
		leading.Store(true)
		<-runCtx.Done()
		close(fStopping)
		<-finishF // a reconcile that is still running
		return nil
	})

	assert.ErrorIs(t, err, errLeadershipLost)
	select {
	case <-fStopping:
	case <-time.After(5 * time.Second):
		t.Fatal("f's context was not cancelled")
	}
}

func TestLeaseRunnerReturnsWhenCancelledBeforeLeading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := fastLeaseRunner(t, fakekubernetes.NewClientset())

	err := r.Run(ctx, func(context.Context) error {
		t.Error("f must not run")
		return nil
	})

	assert.NoError(t, err)
}

func TestLeaseRunnerStopsWaitingWhenAnotherReplicaLeads(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	other := fastLeaseRunner(t, kube)
	// Leases store whole seconds, so the holder needs at least one.
	other.leaseDuration, other.renewDeadline = 2*time.Second, time.Second
	otherCtx, stopOther := context.WithCancel(context.Background())
	otherDone := make(chan error)
	otherLeading := make(chan struct{})
	go func() {
		otherDone <- other.Run(otherCtx, func(ctx context.Context) error {
			close(otherLeading)
			<-ctx.Done()
			return nil
		})
	}()
	<-otherLeading
	defer func() {
		stopOther()
		require.NoError(t, <-otherDone)
	}()

	r := fastLeaseRunner(t, kube)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.Run(ctx, func(context.Context) error {
		t.Error("f must not run while another replica leads")
		return nil
	})

	assert.NoError(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Equal(t, other.lock.Identity(), leaseHolder(t, kube))
}

func TestLeaseRunnerReleasesALeaseAcquiredWhileStopping(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	r := fastLeaseRunner(t, kube)
	ctx, cancel := context.WithCancel(context.Background())
	kube.PrependReactor("create", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})

	err := r.Run(ctx, func(runCtx context.Context) error {
		<-runCtx.Done()
		return nil
	})

	assert.NoError(t, err)
	assert.Empty(t, leaseHolder(t, kube))
}

func TestLeaseRunnerDoesNotReleaseALostLease(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	r := fastLeaseRunner(t, kube)
	var leading atomic.Bool
	var releases atomic.Int32
	kube.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			releases.Add(1)
		}
		if leading.Load() {
			return true, nil, apierrors.NewServiceUnavailable("down")
		}
		return false, nil, nil
	})

	err := r.Run(context.Background(), func(runCtx context.Context) error {
		leading.Store(true)
		<-runCtx.Done()
		return nil
	})

	assert.ErrorIs(t, err, errLeadershipLost)
	assert.Zero(t, releases.Load())
}

func TestLeaseRunnerKeepsGoingWhenReleaseFails(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	r := fastLeaseRunner(t, kube)
	var stopped atomic.Bool
	kube.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if stopped.Load() {
			return true, nil, apierrors.NewServiceUnavailable("down")
		}
		return false, nil, nil
	})

	err := r.Run(context.Background(), func(context.Context) error {
		stopped.Store(true)
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, r.lock.Identity(), leaseHolder(t, kube), "the lease is left to expire")
}

func TestLeaseRunnerLeavesALeaseTakenOverByAnotherReplica(t *testing.T) {
	kube := fakekubernetes.NewClientset()
	r := fastLeaseRunner(t, kube)
	// The fake client has no optimistic locking; reject this runner's renewals
	// once the other replica holds the lease, as the API server would.
	var takenOver atomic.Bool
	kube.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if takenOver.Load() && ptr.Deref(lease.Spec.HolderIdentity, "") == r.lock.Identity() {
			return true, nil, apierrors.NewConflict(coordinationv1.Resource("leases"), lockKey, errors.New("taken over"))
		}
		return false, nil, nil
	})

	err := r.Run(context.Background(), func(context.Context) error {
		takenOver.Store(true)
		lease, err := kube.CoordinationV1().Leases("ns").Get(context.Background(), lockKey, metav1.GetOptions{})
		require.NoError(t, err)
		lease.Spec.HolderIdentity = ptr.To("other")
		_, err = kube.CoordinationV1().Leases("ns").Update(context.Background(), lease, metav1.UpdateOptions{})
		require.NoError(t, err)
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, "other", leaseHolder(t, kube))
}
