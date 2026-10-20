package redisfailover

import (
	"cmp"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
)

// abortOrphanedFailover ends a FAILOVER on the pod labelled master that no
// reconcile watches, for example after an operator restart in a handover.
// Without the abort, the old master stays a replica without writes, and the
// election below can promote a second master. The handover of this reconcile
// starts later, in UpdateRedisesPods. The check also aborts a FAILOVER that a
// user sends by hand.
func (r *RedisFailoverHandler) abortOrphanedFailover(rf *redisfailoverv1.RedisFailover) (bool, error) {
	// The checks below list the pods again and report a failed list.
	pod, _ := r.labelledMasterPod(rf)
	if pod == nil || pod.Status.PodIP == "" {
		return false, nil
	}
	aborted, err := r.rfHealer.AbortOrphanedFailover(pod.Status.PodIP, rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: "unable to abort a FAILOVER that no reconcile watches"}
		return false, err
	}
	if aborted {
		rf.Status = redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.NotHealthyState, Message: fmt.Sprintf("aborted a FAILOVER of pod %s that no reconcile watched", pod.Name)}
		// The checks run again on the roles after the abort.
		if r.requeue != nil {
			r.requeue(failoverKey(rf), time.Second)
		}
	}
	return aborted, nil
}

// checkAndHealOperatorManagedMode handles failover when Sentinel is disabled.
// The operator directly manages master election and failover.
func (r *RedisFailoverHandler) checkAndHealOperatorManagedMode(rf *redisfailoverv1.RedisFailover, allRunning bool) error {
	// A quorum (majority) of Running pods is sufficient to heal, as in
	// CheckAndHeal. One Pending pod must not block the master election in
	// this mode, which is the default.
	if !r.redisQuorumRunning(rf, allRunning) {
		return nil
	}

	if aborted, err := r.abortOrphanedFailover(rf); err != nil || aborted {
		return err
	}

	nMasters, err := r.rfChecker.GetNumberMasters(rf)
	if err != nil {
		r.recordMasterNotAnswering(rf, err)
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get number of masters",
		}
		return err
	}

	var master string
	switch nMasters {
	case 0:
		return r.healNoMasterOperatorMode(rf)
	case 1:
		var done bool
		if master, done, err = r.healOneMasterOperatorMode(rf); err != nil || done {
			return err
		}
	default:
		return r.healMultipleMastersOperatorMode(rf)
	}

	return r.applyConfigAndRollout(rf, master, "unable to update redis pods")
}

// healNoMasterOperatorMode elects a master when no pod is the master.
func (r *RedisFailoverHandler) healNoMasterOperatorMode(rf *redisfailoverv1.RedisFailover) error {
	// A master pod in deletion is not counted, but it can still accept
	// writes. Wait until it stops, so that a promoted replica does not
	// lose them.
	if stopping, err := r.stoppingMasterStatus(rf, masterStoppingMsg); err != nil || stopping {
		return err
	}
	// A master pod that does not answer gets failoverTimeout to recover.
	// Without a master pod, a wait only makes the outage longer.
	pod, err := r.unreachableMasterPod(rf)
	if wait, err := r.waitForMasterPod(rf, pod, err); err != nil || wait {
		return err
	}
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no masters detected"))
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("No master available, operator will elect one")

	bestReplica, err := r.rfChecker.GetBestReplicaForPromotion(rf)
	if err != nil {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).
			Warnf("Could not determine best replica: %v, falling back to oldest", err)
		_, err = r.rfHealer.SetOldestAsMaster(rf)
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "failed to elect master",
			}
			return err
		}
	} else {
		err = r.rfHealer.PromoteBestReplica(bestReplica.IP, rf)
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
		if err != nil {
			setPromotionFailedStatus(rf, err, "failed to promote replica")
			return err
		}
	}
	r.clearMasterUnreachable(rf)
	return nil
}

// healOneMasterOperatorMode checks the health of the master, fails over an
// unhealthy master, and makes each replica a replica of a healthy master. It
// returns the IP of the healthy master, and reports whether the reconcile
// ends.
func (r *RedisFailoverHandler) healOneMasterOperatorMode(rf *redisfailoverv1.RedisFailover) (string, bool, error) {
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, nil)

	healthy, masterIP, err := r.rfChecker.CheckMasterHealth(rf)
	if err != nil {
		r.recordMasterNotAnswering(rf, err)
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to check master health",
		}
		return "", true, err
	}

	if !healthy {
		return "", true, r.failOverUnhealthyMaster(rf, masterIP)
	}
	r.clearMasterUnreachable(rf)

	err = r.rfChecker.CheckAllSlavesFromMaster(masterIP, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.SLAVE_WRONG_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		r.logWrongMaster(rf, err)
		if err = r.rfHealer.SetMasterOnAll(masterIP, rf); err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "failed to configure slaves",
			}
			return "", true, err
		}
	}
	return masterIP, false, nil
}

// failOverUnhealthyMaster promotes the best replica when the master at
// masterIP is unhealthy and its failoverTimeout has passed.
func (r *RedisFailoverHandler) failOverUnhealthyMaster(rf *redisfailoverv1.RedisFailover, masterIP string) error {
	var pod *corev1.Pod
	var err error
	if masterIP == "" {
		// The master counted before can be in deletion at this time.
		// Then it is not found, but it can still accept writes, as in
		// case 0.
		if stopping, err := r.stoppingMasterStatus(rf, masterStoppingMsg); err != nil || stopping {
			return err
		}
		// The second count does not count a master pod that is not
		// ready. As in case 0, that pod gets failoverTimeout to recover.
		pod, err = r.unreachableMasterPod(rf)
	} else {
		pod, err = r.findRedisPod(rf, func(pod *corev1.Pod) bool { return pod.Status.PodIP == masterIP })
	}
	if wait, err := r.waitForMasterPod(rf, pod, err); err != nil || wait {
		return err
	}
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	if pod != nil {
		logger = logger.WithField("pod", pod.Name)
	}
	logger.Warningf("Master %s is unhealthy, initiating failover", cmp.Or(masterIP, "(unknown IP)"))

	bestReplica, err := r.rfChecker.GetBestReplicaForPromotion(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "no healthy replica available for failover",
		}
		return err
	}

	err = r.rfHealer.PromoteBestReplica(bestReplica.IP, rf)
	if err != nil {
		setPromotionFailedStatus(rf, err, "failover failed")
		return err
	}
	r.clearMasterUnreachable(rf)
	return nil
}

// healMultipleMastersOperatorMode handles more than one master.
func (r *RedisFailoverHandler) healMultipleMastersOperatorMode(rf *redisfailoverv1.RedisFailover) error {
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, errors.New("multiple masters detected"))
	// An old master that did not answer during a failover can come back
	// as a master. The pod labelled master is the master that the
	// operator elected, so the other masters become its replicas.
	labelled, err := r.labelledMasterPod(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: masterPodLookupFailed,
		}
		return err
	}
	if labelled != nil {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).
			Warningf("Multiple masters detected, making all pods replicas of the labelled master %s", labelled.Name)
		if err := r.rfHealer.SetMasterOnAll(labelled.Status.PodIP, rf); err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to make the other masters replicas of the labelled master",
			}
			return err
		}
		// A master that refused REPLICAOF is skipped, so the next
		// reconcile counts the masters again before the status is Healthy.
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: fmt.Sprintf("multiple masters detected, made the other masters replicas of %s", labelled.Name),
		}
		return nil
	}
	errorMsg := "multiple masters detected, fix manually"
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: errorMsg,
	}
	return errors.New(errorMsg)
}
