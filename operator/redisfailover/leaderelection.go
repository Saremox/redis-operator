package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/saremox/redis-operator/log"
)

const (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
)

var (
	hostname          = os.Hostname
	errLeadershipLost = errors.New("leadership lost")
)

// leaderRunner runs a function only while this instance holds the lease.
type leaderRunner interface {
	Run(ctx context.Context, f func(context.Context) error) error
}

type leaseRunner struct {
	lock                                      resourcelock.Interface
	leaseDuration, renewDeadline, retryPeriod time.Duration
	logger                                    log.Logger
}

func newLeaseRunner(key, namespace string, k8scli kubernetes.Interface, logger log.Logger) (*leaseRunner, error) {
	if namespace == "" {
		return nil, fmt.Errorf("running in leader election mode requires the namespace running")
	}
	if key == "" {
		return nil, fmt.Errorf("running in leader election mode requires a key for identification the different instances")
	}
	host, err := hostname()
	if err != nil {
		return nil, err
	}

	return &leaseRunner{
		lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Namespace: namespace, Name: key},
			Client:     k8scli.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: host + "_" + string(uuid.NewUUID())},
		},
		leaseDuration: leaseDuration,
		renewDeadline: renewDeadline,
		retryPeriod:   retryPeriod,
		logger:        logger.WithField("source-service", "leader-election").WithField("leader-election-id", namespace+"/"+key),
	}, nil
}

// Run runs f once this instance holds the lease, until ctx is done or the
// lease is lost. When ctx is done, Run waits for f and then releases the
// lease. When the lease is lost, Run returns errLeadershipLost at once so the
// process can exit before another replica takes over, even if f is still
// running.
func (r *leaseRunner) Run(ctx context.Context, f func(context.Context) error) error {
	// The election has its own context: once this instance leads, it keeps
	// renewing the lease until f has stopped, even if ctx is already done.
	electionCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElection()

	leading := make(chan context.Context, 1)
	electionDone := make(chan struct{})
	go func() {
		defer close(electionDone)
		// No ReleaseOnCancel: client-go would also release a lost lease.
		leaderelection.RunOrDie(electionCtx, leaderelection.LeaderElectionConfig{
			Lock:          r.lock,
			LeaseDuration: r.leaseDuration,
			RenewDeadline: r.renewDeadline,
			RetryPeriod:   r.retryPeriod,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leaderCtx context.Context) { leading <- leaderCtx },
				OnStoppedLeading: func() {},
			},
		})
	}()
	r.logger.Infof("running in leader election mode, waiting to acquire leadership...")

	// Without ctx being done, the election only ends after leading, and then
	// leading always receives the leader context, done if the lease is lost.
	var err error
	select {
	case leaderCtx := <-leading:
		if ctx.Err() == nil {
			err = r.lead(ctx, leaderCtx, f)
		}
	case <-ctx.Done():
	}
	if errors.Is(err, errLeadershipLost) {
		r.logger.Warningf("leader lease lost")
		return err
	}
	stopElection()
	<-electionDone
	r.release()
	return err
}

// lead runs f until ctx is done or the lease is lost.
func (r *leaseRunner) lead(ctx, leaderCtx context.Context, f func(context.Context) error) error {
	r.logger.Infof("lead acquire, starting...")
	runCtx, cancelRun := context.WithCancel(leaderCtx)
	defer cancelRun()
	stopRun := context.AfterFunc(ctx, cancelRun)
	defer stopRun()

	done := make(chan error, 1)
	go func() { done <- f(runCtx) }()
	var err error
	select {
	case err = <-done:
		r.logger.Infof("lead execution stopped")
	case <-leaderCtx.Done():
	}
	if leaderCtx.Err() != nil {
		return errLeadershipLost
	}
	return err
}

// release gives up the lease if this instance holds it, so another replica
// takes over at once instead of waiting for it to expire.
func (r *leaseRunner) release() {
	ctx, cancel := context.WithTimeout(context.Background(), r.renewDeadline)
	defer cancel()
	current, _, err := r.lock.Get(ctx)
	if apierrors.IsNotFound(err) || (err == nil && current.HolderIdentity != r.lock.Identity()) {
		return
	}
	if err == nil {
		now := metav1.NewTime(time.Now())
		err = r.lock.Update(ctx, resourcelock.LeaderElectionRecord{
			LeaderTransitions:    current.LeaderTransitions,
			LeaseDurationSeconds: 1,
			RenewTime:            now,
			AcquireTime:          now,
		})
	}
	if err != nil {
		r.logger.Warningf("could not release the leader lease, it expires in %s: %v", r.leaseDuration, err)
		return
	}
	r.logger.Infof("released the leader lease")
}
