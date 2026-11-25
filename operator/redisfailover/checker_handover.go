package redisfailover

import (
	"errors"
	"fmt"
	"time"

	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
)

// handOverMaster moves the master role to the best synced replica with
// FAILOVER, and does not delete the stale master pod. The demoted master is
// then a stale replica, so a later reconcile replaces it after the same checks
// as each other replica. It reports false when the caller must delete the
// master pod: the master has no FAILOVER command, or it refused
// handoverMaxRefusals attempts in a row.
func (r *RedisFailoverHandler) handOverMaster(rf *redisfailoverv1.RedisFailover, masterIP, master string) (bool, error) {
	key := failoverKey(rf)
	v, _ := r.handoverRetries.Load(key)
	last, _ := v.(handoverRetry)
	// A wait or a count of an earlier master pod does not apply to a new one,
	// for example after a failover.
	if last.master != master {
		last = handoverRetry{}
	}
	if r.now().Before(last.at) {
		setHandoverStatus(rf, last.message)
		return true, nil
	}
	best, err := r.rfChecker.GetBestReplicaForPromotion(rf)
	if err != nil {
		return true, err
	}
	// An unsynced replica can lack data that the healthy master has.
	if !best.Synced {
		return true, fmt.Errorf("no synced replica to take the master role from pod %s", master)
	}
	result, err := r.rfHealer.HandOverMaster(masterIP, best.IP, rf)
	if err != nil {
		return true, err
	}
	next := handoverRetry{master: master, delay: last.delay}
	switch result {
	case rfservice.HandoverUnsupported:
		r.logHandoverFallback(rf, fmt.Sprintf("pod %s has no FAILOVER command (Redis before 6.2, or renamed), so the rollout deletes it", master))
		return false, nil
	case rfservice.HandoverAborted:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.MASTER_HANDOVER_ABORTED, metrics.NOT_APPLICABLE, errors.New("handover aborted"))
		r.retryHandover(rf, next, fmt.Sprintf("the handover of the master role to pod %s was aborted, because the replica did not take the master role in time", best.PodName))
		return true, nil
	case rfservice.HandoverRefused:
		// The master can see its replicas at an address other than the pod
		// IP, for example behind a service mesh sidecar, or an ACL can deny
		// FAILOVER. Then each attempt fails, also with another target, and
		// only the delete of the master pod continues the rollout.
		next.refusals = last.refusals + 1
		if next.refusals >= handoverMaxRefusals {
			r.handoverRetries.Store(key, next)
			r.logHandoverFallback(rf, fmt.Sprintf("the master refused the handover of the master role %d times in a row, so the rollout deletes the master pod %s", handoverMaxRefusals, master))
			return false, nil
		}
		r.retryHandover(rf, next, fmt.Sprintf("the master refused the handover of the master role to pod %s (%d of %d)", best.PodName, next.refusals, handoverMaxRefusals))
		return true, nil
	case rfservice.HandoverDone:
		// The target is the master. The code after the switch continues.
	}
	r.handoverRetries.Delete(key)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.MASTER_HANDOVER_ABORTED, metrics.NOT_APPLICABLE, nil)

	// The old master is a replica already, so the labels change after the
	// role. The new master gets its label first: until the old master loses
	// its label, the master Service also routes to a pod that refuses writes.
	err = r.rfHealer.PromoteBestReplica(best.IP, rf)
	// A partial reconciliation leaves a replica on the old master or a wrong
	// label. The next reconcile corrects both.
	if err != nil && !errors.Is(err, rfservice.ErrPartialReconciliation) {
		return true, err
	}
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("pod", master)
	if err != nil {
		logger.Warningf("handed the master role over to %s, but not all replicas changed: %v", best.PodName, err)
		return true, nil
	}
	logger.Infof("handed the master role over to %s before the replacement of the stale master pod", best.PodName)
	return true, nil
}

// handoverRetry is the state after a handover from the master pod master
// that did not move the role. Each attempt can pause the writes, so the wait
// doubles up to handoverRetryMax. refusals counts the refusals in a row. The
// state is in memory: after an operator restart, the next reconcile tries at
// once and counts from zero.
type handoverRetry struct {
	at       time.Time
	delay    time.Duration
	message  string
	master   string
	refusals int
}

const (
	handoverRetryMin    = 30 * time.Second
	handoverRetryMax    = 5 * time.Minute
	handoverMaxRefusals = 3
)

func (r *RedisFailoverHandler) retryHandover(rf *redisfailoverv1.RedisFailover, next handoverRetry, reason string) {
	key := failoverKey(rf)
	next.delay = min(max(2*next.delay, handoverRetryMin), handoverRetryMax)
	next.message = fmt.Sprintf("%s, the next attempt is after %s", reason, next.delay)
	next.at = r.now().Add(next.delay)
	r.handoverRetries.Store(key, next)
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Warningf("%s", next.message)
	setHandoverStatus(rf, next.message)
	// No pod event comes at the end of the wait.
	if r.requeue != nil {
		r.requeue(key, next.delay)
	}
}

// logHandoverFallback logs the fallback to a delete of the master pod at Info
// once, and at Debug at each later reconcile with the same message.
func (r *RedisFailoverHandler) logHandoverFallback(rf *redisfailoverv1.RedisFailover, message string) {
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	if last, _ := r.handoverFallbackLogged.Swap(failoverKey(rf), message); last == message {
		logger.Debugf("%s", message)
		return
	}
	logger.Infof("%s", message)
}

func setHandoverStatus(rf *redisfailoverv1.RedisFailover, message string) {
	rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: message}
}

// redisPodNamesByIP leaves out terminating pods, because the rollout must not
