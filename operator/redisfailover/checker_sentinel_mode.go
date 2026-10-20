package redisfailover

import (
	"errors"
	"fmt"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// checkAndHealSentinelMode checks and heals, in this order:
//   - a quorum of Redis pods and of Sentinel pods,
//   - a report of the Sentinel peers and masters that are no pods of the
//     RedisFailover,
//   - exactly one Redis master, a wait while the Sentinels give another pod
//     as the master, and each replica on that master,
//   - the custom Redis config and maxmemory,
//   - the Redis pod rollout,
//   - a wait while the master pod stops,
//   - the master that each Sentinel monitors,
//   - the Sentinel and replica counts in each Sentinel, and the custom
//     Sentinel config.
func (r *RedisFailoverHandler) checkAndHealSentinelMode(rf *redisfailoverv1.RedisFailover, allRunning bool) error {
	if !r.redisQuorumRunning(rf, allRunning) || !r.sentinelQuorumRunning(rf) {
		r.skipSentinelAddrs(rf)
		return nil
	}
	// Before the count of the masters, because two masters end the reconcile.
	r.observeSentinelAddrs(rf)
	nMasters, done, err := r.healSentinelModeMasters(rf)
	if err != nil || done {
		return err
	}
	// After an election in this reconcile, the Sentinels give the old master
	// until the operator monitors the elected master below.
	master, err := r.sentinelModeMaster(rf, nMasters == 1)
	if err != nil || master == "" {
		return err
	}
	if err := r.applyConfigAndRollout(rf, master, "unable to update redis PODs"); err != nil {
		return err
	}

	// A SENTINEL RESET makes Sentinel forget the replicas until the master
	// tells it again. A stopping master may not do that before it exits, and
	// then the failover finds no replica to promote (NOGOODSLAVE).
	if stopping, err := r.stoppingMasterStatus(rf, masterPodStoppingMsg); err != nil || stopping {
		return err
	}
	return r.healSentinelMonitors(rf, master)
}

// sentinelQuorumRunning reports whether a quorum of the Sentinel pods runs.
func (r *RedisFailoverHandler) sentinelQuorumRunning(rf *redisfailoverv1.RedisFailover) bool {
	if r.rfChecker.IsSentinelRunningQuorum(rf) {
		return true
	}
	errorMsg := "sentinel quorum not running"
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: errorMsg,
	}
	setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Sentinel quorum not running, waiting for sentinel deployment reconcile")
	return false
}

// healSentinelModeMasters counts the masters and heals a count other than
// one. It returns the count, and reports whether the reconcile ends.
func (r *RedisFailoverHandler) healSentinelModeMasters(rf *redisfailoverv1.RedisFailover) (int, bool, error) {
	nMasters, err := r.rfChecker.GetNumberMasters(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get number of masters",
		}
		return nMasters, true, err
	}

	switch nMasters {
	case 0:
		done, err := r.healNoMasterSentinelMode(rf)
		return nMasters, done, err
	case 1:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, nil)
		return nMasters, false, nil
	default:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, errors.New("multiple masters detected"))
		errorMsg := "more than one master, fix manually"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		return nMasters, true, errors.New(errorMsg)
	}
}

// healNoMasterSentinelMode handles a RedisFailover without a master. It
// reports whether the reconcile ends.
func (r *RedisFailoverHandler) healNoMasterSentinelMode(rf *redisfailoverv1.RedisFailover) (bool, error) {
	// A master pod in deletion is not counted, but it can still accept
	// writes. When CLIENT PAUSE WRITE returns OK, its shutdown script waits for
	// the replicas and asks Sentinel to fail over. An election at this time
	// can lose writes.
	if stopping, err := r.stoppingMasterStatus(rf, masterStoppingMsg); err != nil || stopping {
		return true, err
	}
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no masters detected"))
	// With one Redis pod, Sentinel has no replica to promote, so the
	// operator makes that pod the master.
	if rf.Spec.Redis.Replicas == 1 {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("Resource spec with standalone master - operator will set the master")
		master, err := r.setOldestAsMaster(rf)
		// Other pods stay only while the StatefulSet removes them. The
		// election already gives master the master label, so a wait does
		// not protect its writes.
		if master != "" {
			r.recordElection(rf, master)
		}
		return true, err
	}
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Number of Masters running is 0")
	maxUptime, err := r.rfChecker.GetMaxRedisPodTime(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get Redis POD time",
		}
		return true, err
	}

	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("No master available, the max pod uptime is %s", maxUptime.Round(time.Second))
	return r.electMasterWhenSentinelCannot(rf)
}

// electMasterWhenSentinelCannot elects a master when Sentinel cannot fail
// over:
//   - the Sentinels have no quorum (CheckSentinelQuorum),
//   - each Redis pod has 127.0.0.1 as master, after the first start or
//     after all the pods restarted (CheckIfMasterLocalhost),
//   - no Sentinel monitors a pod or knows a replica pod
//     (CheckSentinelsCannotFailover).
//
// It reports whether the reconcile ends.
func (r *RedisFailoverHandler) electMasterWhenSentinelCannot(rf *redisfailoverv1.RedisFailover) (bool, error) {
	noqrmCnt, err := r.rfChecker.CheckSentinelQuorum(rf)
	if err != nil {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Quorum not available for sentinel to choose master, estimated unhealthy sentinels: %d, operator to step in: %v", noqrmCnt, err)
		if _, err := r.setOldestAsMaster(rf); err != nil {
			return true, err
		}
		return false, nil
	}
	status, err := r.rfChecker.CheckIfMasterLocalhost(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to check if master localhost",
		}
		return true, err
	}
	if status {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("each available redis has 127.0.0.1 as master, the operator elects a master")
		if _, err := r.setOldestAsMaster(rf); err != nil {
			return true, err
		}
		return false, nil
	}
	cannot, err := r.rfChecker.CheckSentinelsCannotFailover(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to check whether Sentinel can fail over",
		}
		return true, err
	}
	if cannot {
		elected, err := r.electMasterForSentinel(rf)
		if err != nil || !elected {
			return true, err
		}
		return false, nil
	}

	// Sentinel can fail over, so the operator waits for it.
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("no master found, wait until failover or fix manually")
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no master not fixed, wait until failover or fix manually"))
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: "no master, waiting for the Sentinel failover",
	}
	return true, nil
}

// setOldestAsMaster makes the oldest Redis pod the master, and returns its IP.
func (r *RedisFailoverHandler) setOldestAsMaster(rf *redisfailoverv1.RedisFailover) (string, error) {
	master, err := r.rfHealer.SetOldestAsMaster(rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		errorMsg := "Error in Setting oldest Pod as master"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Error(errorMsg)
		return "", err
	}
	return master, nil
}

// sentinelModeMaster returns the IP of the master, and makes each replica a
// replica of it. With checkSentinels, it returns an empty IP while it waits
// for the Sentinels.
func (r *RedisFailoverHandler) sentinelModeMaster(rf *redisfailoverv1.RedisFailover, checkSentinels bool) (string, error) {
	master, err := r.rfChecker.GetMasterIP(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get master IP",
		}
		return "", err
	}
	if checkSentinels {
		// A failed read can hide a quorum for another pod, so the roles stay.
		wait, err := r.waitForSentinelMaster(rf, master)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to check the master of the Sentinels",
			}
			return "", err
		}
		if wait {
			return "", nil
		}
	}

	err = r.rfChecker.CheckAllSlavesFromMaster(master, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.SLAVE_WRONG_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		r.logWrongMaster(rf, err)
		// Read the master again just before the change, because the pods can
		// change after the first read. Then SetMasterOnAll refuses a stale IP
		// less often.
		freshMaster, ferr := r.rfChecker.GetMasterIP(rf)
		if ferr != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to re-verify master IP",
			}
			return "", ferr
		}
		if err = r.rfHealer.SetMasterOnAll(freshMaster, rf); err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State: redisfailoverv1.NotHealthyState,
			}
			return "", err
		}
	}
	return master, nil
}

// healSentinelMonitors makes each Sentinel monitor the master, and then heals
// the Sentinels.
func (r *RedisFailoverHandler) healSentinelMonitors(rf *redisfailoverv1.RedisFailover, master string) error {
	sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get sentinels IPs",
		}
		return err
	}

	heal := r.startSentinelHeal(rf)
	defer r.saveSentinelResets(heal, sentinels)

	// master can be stale after applyRedisCustomConfig and UpdateRedisesPods.
	// Read it again only when a Sentinel needs a fix, and only one time.
	target := &monitorTarget{master: master, port: getRedisPort(rf.Spec.Redis.Port)}
	for _, sip := range sentinels {
		if err := r.healSentinelMonitor(rf, sentinels, heal, sip, target); err != nil {
			return err
		}
	}
	return r.checkAndHealSentinels(rf, sentinels, heal)
}

// monitorTarget is the master that the Sentinels must monitor. refreshed is
// true after the master was read again.
type monitorTarget struct {
	master    string
	port      string
	refreshed bool
}

// healSentinelMonitor makes the Sentinel sip monitor the master of target.
func (r *RedisFailoverHandler) healSentinelMonitor(rf *redisfailoverv1.RedisFailover, sentinels []string, heal *sentinelHeal, sip string, target *monitorTarget) error {
	err := r.rfChecker.CheckSentinelMonitor(sip, target.master, target.port)
	setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
	if heal.skip(sip, err) || err == nil {
		return nil
	}
	r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Fixing sentinel not monitoring expected master: %s", err.Error())
	if !target.refreshed {
		freshMaster, ferr := r.rfChecker.GetMasterIP(rf)
		if ferr != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to re-verify master IP",
			}
			return ferr
		}
		target.refreshed = true
		if freshMaster != target.master {
			target.master = freshMaster
			// The Sentinel can already monitor the new master. A new
			// monitor then only resets its state.
			err = r.rfChecker.CheckSentinelMonitor(sip, target.master, target.port)
			setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
			if err == nil || heal.skip(sip, err) {
				return nil
			}
		}
	}
	if !r.sentinelChangeAllowed(rf, sentinels, heal, sip) {
		return nil
	}
	if err := r.rfHealer.NewSentinelMonitor(sip, target.master, rf); err != nil {
		if heal.skip(sip, err) {
			return nil
		}
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State: redisfailoverv1.NotHealthyState,
		}
		return err
	}
	heal.resets[sip] = r.now()
	return nil
}

// electMasterForSentinel promotes the best replica when no Sentinel can fail
// over. It first counts the masters and checks for a stopping master again,
// because a change since the first checks gives two masters. It reports
// whether it elected a master.
func (r *RedisFailoverHandler) electMasterForSentinel(rf *redisfailoverv1.RedisFailover) (bool, error) {
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	nMasters, err := r.rfChecker.GetNumberMasters(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get number of masters",
		}
		return false, err
	}
	if nMasters != 0 {
		logger.Infof("A master appeared before the election, the operator does not elect a master")
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "a master appeared before the election, checking again",
		}
		return false, nil
	}
	// GetNumberMasters does not count a pod in deletion.
	if stopping, err := r.stoppingMasterStatus(rf, masterStoppingMsg); err != nil || stopping {
		return false, err
	}
	best, err := r.rfChecker.GetBestReplicaForPromotion(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "Sentinel cannot fail over and no replica is available for promotion",
		}
		return false, err
	}
	logger.Warningf("No Sentinel knows a replica pod or monitors a pod, so Sentinel cannot fail over. The operator promotes %s (%s)", best.PodName, best.IP)
	err = r.rfHealer.PromoteBestReplica(best.IP, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		// The promoted master works when only its label or a replica change
		// failed.
		if errors.Is(err, rfservice.ErrPartialReconciliation) || errors.Is(err, rfservice.ErrMasterLabel) {
			r.recordElection(rf, best.IP)
		}
		setPromotionFailedStatus(rf, err, "Sentinel cannot fail over and the promotion of a replica failed")
		return false, err
	}
	r.recordElection(rf, best.IP)
	// The checks below make the Sentinels monitor the new master. The status
	// stays NotHealthy until the next check finds them healthy.
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: fmt.Sprintf("Sentinel knew no replica to fail over to, the operator promoted %s", best.PodName),
	}
	return true, nil
}
