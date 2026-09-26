package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/saremox/redis-operator/service/k8s"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/redis"
)

// UpdateRedisesPods if the running version of pods is equal to the statefulset one
func (r *RedisFailoverHandler) UpdateRedisesPods(rf *redisfailoverv1.RedisFailover) error {
	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return err
	}

	masterIP := ""
	if !rf.Bootstrapping() {
		masterIP, _ = r.rfChecker.GetMasterIP(rf)
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("masterIP", masterIP).Debug("got master IP")
	}
	// No performed updates when nodes are syncing, still not connected, etc.
	for _, rip := range redises {
		if rip != masterIP {
			ready, err := r.rfChecker.CheckRedisSlavesReady(rip, rf)
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("ready", ready).Debug("got secondary state")
			if err != nil {
				return err
			}
			if !ready {
				return nil
			}
		}
	}

	ssUR, err := r.rfChecker.GetStatefulSetUpdateRevision(rf)
	if err != nil {
		return err
	}
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("ssUR", ssUR).Debug("got StatefulSet update revision")

	redisesPods, err := r.rfChecker.GetRedisesSlavesPods(rf)
	if err != nil {
		return err
	}

	// Update stale pods with a slave role
	for _, pod := range redisesPods {
		revision, err := r.rfChecker.GetRedisRevisionHash(pod, rf)
		if err != nil {
			return err
		}
		if revision != ssUR {
			if settled, err := r.redisPodsSettled(rf, ssUR); err != nil || !settled {
				return err
			}
			if recreate, err := r.resizeInPlace(rf, pod, ssUR); err != nil || !recreate {
				return err
			}
			//Delete pod and wait next round to check if the new one is synced
			err = r.rfHealer.DeletePod(pod, rf)
			if err != nil {
				return err
			}
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("revision", revision).WithField("pod", pod).Debug("deleted secondary pod")
			return nil
		}
	}

	if !rf.Bootstrapping() {
		// Update stale pod with role master
		master, err := r.rfChecker.GetRedisesMasterPod(rf)
		if err != nil {
			return err
		}

		masterRevision, err := r.rfChecker.GetRedisRevisionHash(master, rf)
		if err != nil {
			return err
		}
		if masterRevision != ssUR {
			// Resizing in place needs no failover, so it skips the gate below.
			if settled, err := r.redisPodsSettled(rf, ssUR); err != nil || !settled {
				return err
			}
			if recreate, err := r.resizeInPlace(rf, master, ssUR); err != nil || !recreate {
				return err
			}

			// Deleting the master makes sentinel run a failover. Only do that once
			// every sentinel has a quorum (majority) of the freshly (re)started
			// slaves in memory - the redis-side readiness checked above is not
			// enough, because sentinel fails over from its own view and its slave
			// discovery lags. Replacing the master before then leaves the failover
			// with no promotable replica and it dies with NOGOODSLAVE until manual
			// repair. A quorum, rather than the full expected count, is required
			// so one permanently unavailable replica (e.g. a PVC stuck in a dead
			// zone) cannot block master replacement forever when a safe failover
			// is available via the reachable majority.
			//
			// This gate only applies when Sentinel is actually managing
			// failover. In operator-managed mode (sentinel.enabled: false)
			// there is no Sentinel Deployment to query - GetSentinelsIPs would
			// just 404 against it - and the operator's own election logic in
			// checkAndHealOperatorManagedMode (the "no master" branch) already
			// takes over on the very next reconcile once this delete leaves the
			// RedisFailover without a master, using the same replication-offset
			// based selection this gate exists to protect.
			if !rf.OperatorManagedFailover() {
				sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
				if err != nil {
					return err
				}
				for _, sip := range sentinels {
					if err := r.rfChecker.CheckSentinelSlavesNumberQuorumInMemory(sip, rf); err != nil {
						r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("Waiting for sentinels to see a quorum of slaves before replacing the master: %s", err.Error())
						return nil
					}
				}
			}

			err = r.rfHealer.DeletePod(master, rf)
			if err != nil {
				return err
			}
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("revision", masterRevision).WithField("pod", master).Debug("deleted primary pod")
			return nil
		}
	}

	return nil
}

// resizeInPlace tries to move a stale pod to the update revision without
// recreating it. It reports whether the pod has to be recreated instead.
func (r *RedisFailoverHandler) resizeInPlace(rf *redisfailoverv1.RedisFailover, pod, updateRevision string) (bool, error) {
	result, err := r.rfHealer.ResizePodInPlace(rf, pod, updateRevision)
	if err != nil {
		return false, err
	}
	if result.Action == rfservice.ResizeWaiting && result.Message != "" {
		rf.Status.Message = result.Message
	}
	return result.Action == rfservice.ResizeRecreate, nil
}

// redisPodsSettled reports whether the last redis pod replacement has
// finished: the StatefulSet has all its pods, none is being deleted, and every
// pod already on the update revision is ready. Pod events start the next
// reconcile right after a delete, so without this check a rollout would delete
// several pods at once.
func (r *RedisFailoverHandler) redisPodsSettled(rf *redisfailoverv1.RedisFailover, updateRevision string) (bool, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return false, err
	}
	wait := func(reason string) (bool, error) {
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Infof("redis rollout waits: %s", reason)
		return false, nil
	}
	if len(pods.Items) < int(rf.Spec.Redis.Replicas) {
		return wait(fmt.Sprintf("%d of %d pods exist", len(pods.Items), rf.Spec.Redis.Replicas))
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			return wait("pod " + pod.Name + " is terminating")
		}
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] == updateRevision && !util.PodIsReady(pod) {
			return wait("pod " + pod.Name + " is not ready")
		}
	}
	return true, nil
}

// masterPodStopping reports whether the master's pod is being deleted but
// still ready, i.e. still taking writes. A pod on a lost node is not ready,
// so it doesn't block anything.
func (r *RedisFailoverHandler) masterPodStopping(rf *redisfailoverv1.RedisFailover) (bool, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil && rfservice.IsMasterPod(pod) && util.PodIsReady(pod) {
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("pod", pod.Name).Info("waiting for the stopping master pod to exit before electing a master")
			return true, nil
		}
	}
	return false, nil
}

// CheckAndHeal runs verifcation checks to ensure the RedisFailover is in an expected and healthy state.
// If the checks do not match up to expectations, an attempt will be made to "heal" the RedisFailover into a healthy state.
func (r *RedisFailoverHandler) CheckAndHeal(rf *redisfailoverv1.RedisFailover) error {

	oldState := rf.Status.State
	oldLastChanged := rf.Status.LastChanged

	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State: redisfailoverv1.HealthyState,
	}

	defer updateStatus(r.k8sservice, rf, oldState, oldLastChanged)

	if rf.Bootstrapping() {
		return r.checkAndHealBootstrapMode(rf)
	}

	// Route to operator-managed mode when Sentinel is disabled
	if rf.OperatorManagedFailover() {
		return r.checkAndHealOperatorManagedMode(rf)
	}

	// From here on, sentinel-managed mode checks and heals: a quorum of Redis
	// and Sentinel pods running, exactly one Redis master with every slave
	// replicating from it, and the custom Redis config applied. These are
	// quorum-based (a majority, not an exact headcount match against the RF
	// spec) - see the comment below on IsRedisRunningQuorum for why.

	// Heal as long as a quorum (majority) of pods is running rather than requiring
	// the full set. A single Pending pod (unschedulable affinity, AZ loss) must not
	// block master election and sentinel reconfiguration for the survivors; the
	// downstream heal logic already operates only on the running/reachable pods.
	if !r.rfChecker.IsRedisRunningQuorum(rf) {
		errorMsg := "redis quorum not running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Redis quorum not running, waiting for redis statefulset reconcile")
		return nil
	}

	if !r.rfChecker.IsSentinelRunningQuorum(rf) {
		errorMsg := "sentinel quorum not running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Sentinel quorum not running, waiting for sentinel deployment reconcile")
		return nil
	}

	nMasters, err := r.rfChecker.GetNumberMasters(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get number of masters",
		}
		return err
	}

	switch nMasters {
	case 0:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no masters detected"))
		//when number of redis replicas is 1 , the redis is configured for standalone master mode
		//Configure to master
		if rf.Spec.Redis.Replicas == 1 {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("Resource spec with standalone master - operator will set the master")
			err = r.rfHealer.SetOldestAsMaster(rf)
			setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
			if err != nil {
				errorMsg := "Error in Setting oldest Pod as master"
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: errorMsg,
				}
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Error(errorMsg)
				return err
			}
			return nil
		}
		//During the First boot(New deployment or all pods of the statefulsets have restarted),
		//Sentinesl will not be able to choose the master , so operator should select a master
		//Also in scenarios where Sentinels is not in a position to choose a master like , No quorum reached
		//Operator can choose a master , These scenarios can be checked by asking the all the sentinels
		//if its in a postion to choose a master also check if the redis is configured with local host IP as master.
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Number of Masters running is 0")
		maxUptime, err := r.rfChecker.GetMaxRedisPodTime(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to get Redis POD time",
			}
			return err
		}

		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("No master avaiable but max pod up time is : %f", maxUptime.Round(time.Second).Seconds())
		//Check If Sentinel has quorum to take a failover decision
		noqrmCnt, err := r.rfChecker.CheckSentinelQuorum(rf)
		if err != nil {
			// Sentinels are not in a situation to choose a master we pick one
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Quorum not available for sentinel to choose master,estimated unhealthy sentinels :%d , Operator to step-in", noqrmCnt)
			err2 := r.rfHealer.SetOldestAsMaster(rf)
			setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err2)
			if err2 != nil {
				errorMsg := "Error in Setting oldest Pod as master"
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: errorMsg,
				}
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Error(errorMsg)
				return err2
			}
		} else {
			//sentinels are having a quorum to make a failover , but check if redis are not having local hostip (first boot) as master
			status, err2 := r.rfChecker.CheckIfMasterLocalhost(rf)
			if err2 != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to check if master localhost",
				}
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("CheckIfMasterLocalhost failed retry later")
				return err2
			} else if status {
				// all avaialable redis pods have local host ip as master
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Errorf("all available redis is having local loop back as master , operator initiates master selection")
				err3 := r.rfHealer.SetOldestAsMaster(rf)
				setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err3)
				if err3 != nil {
					errorMsg := "Error in Setting oldest Pod as master"
					rf.Status = redisfailoverv1.RedisFailoverStatus{
						State:   redisfailoverv1.NotHealthyState,
						Message: errorMsg,
					}
					r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Error(errorMsg)
					return err3
				}

			} else {

				// We'll wait until failover is done
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("no master found, wait until failover or fix manually")
				setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no master not fixed, wait until failover or fix manually"))
				return nil
			}

		}

	case 1:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, nil)
	default:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, errors.New("multiple masters detected"))
		errorMsg := "more than one master, fix manually"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		return errors.New(errorMsg)
	}

	master, err := r.rfChecker.GetMasterIP(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get master IP",
		}
		return err
	}

	err = r.rfChecker.CheckAllSlavesFromMaster(master, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.SLAVE_WRONG_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Slave not associated to master: %s", err.Error())
		// Re-resolve master right before acting on it: `master` was captured
		// above and pod churn since then could have moved it. Narrowing this
		// window reduces how often SetMasterOnAll's own ownership check has
		// to reject a stale IP and wait for the next reconcile.
		freshMaster, ferr := r.rfChecker.GetMasterIP(rf)
		if ferr != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to re-verify master IP",
			}
			return ferr
		}
		if err = r.rfHealer.SetMasterOnAll(freshMaster, rf); err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State: redisfailoverv1.NotHealthyState,
			}
			return err
		}
	}

	err = r.applyRedisCustomConfig(rf)
	var holdRollout bool
	if err == nil {
		holdRollout, err = r.ensureRedisMaxMemory(rf, master)
	}
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to apply custom config",
		}
		return err
	}

	if !holdRollout {
		err = r.UpdateRedisesPods(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to update redis PODs",
			}
			return err
		}
	}

	sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get sentinels IPs",
		}
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	// `master` may be stale by now (resolved above, before applyRedisCustomConfig
	// and UpdateRedisesPods ran). Re-resolve it lazily, once, only if a sentinel
	// actually needs fixing, and reuse that fresh value for the rest of the loop.
	sentinelMonitorMaster := master
	masterRefreshed := false
	for _, sip := range sentinels {
		err = r.rfChecker.CheckSentinelMonitor(sip, master, port)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Fixing sentinel not monitoring expected master: %s", err.Error())
			if !masterRefreshed {
				freshMaster, ferr := r.rfChecker.GetMasterIP(rf)
				if ferr != nil {
					rf.Status = redisfailoverv1.RedisFailoverStatus{
						State:   redisfailoverv1.NotHealthyState,
						Message: "unable to re-verify master IP",
					}
					return ferr
				}
				sentinelMonitorMaster = freshMaster
				masterRefreshed = true
			}
			if err := r.rfHealer.NewSentinelMonitor(sip, sentinelMonitorMaster, rf); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State: redisfailoverv1.NotHealthyState,
				}
				return err
			}
		}
	}
	return r.checkAndHealSentinels(rf, sentinels)
}

// checkAndHealOperatorManagedMode handles failover when Sentinel is disabled.
// The operator directly manages master election and failover.
func (r *RedisFailoverHandler) checkAndHealOperatorManagedMode(rf *redisfailoverv1.RedisFailover) error {
	// Heal as long as a quorum (majority) of pods is running rather than requiring
	// the full set, matching the Sentinel-managed path (CheckAndHeal above): a
	// single Pending pod (unschedulable affinity, AZ loss) must not permanently
	// block the operator's own master election in this - the default - mode.
	if !r.rfChecker.IsRedisRunningQuorum(rf) {
		errorMsg := "redis quorum not running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Redis quorum not running, waiting for redis statefulset reconcile")
		return nil
	}

	nMasters, err := r.rfChecker.GetNumberMasters(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to get number of masters",
		}
		return err
	}

	var master string
	switch nMasters {
	case 0:
		// A master whose pod is being deleted is no longer counted but may
		// still take writes. Wait for it to stop so a promoted replica
		// doesn't lose them.
		if stopping, err := r.masterPodStopping(rf); err != nil || stopping {
			return err
		}
		// No master available - elect one
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no masters detected"))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("No master available, operator will elect one")

		// Try to select best replica by replication offset
		bestReplica, err := r.rfChecker.GetBestReplicaForPromotion(rf)
		if err != nil {
			// Fall back to oldest pod if we can't determine best replica
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).
				Warnf("Could not determine best replica: %v, falling back to oldest", err)
			err = r.rfHealer.SetOldestAsMaster(rf)
			setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
			if err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "failed to elect master",
				}
				return err
			}
		} else {
			// Promote the best replica
			err = r.rfHealer.PromoteBestReplica(bestReplica.IP, rf)
			setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, err)
			if err != nil {
				msg := "failed to promote replica"
				if errors.Is(err, rfservice.ErrPartialReconciliation) {
					msg = "failover incomplete: replica reconfiguration failed"
				}
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: msg,
				}
				return err
			}
		}
		return nil

	case 1:
		// Exactly one master - check its health
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, nil)

		healthy, masterIP, err := r.rfChecker.CheckMasterHealth(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to check master health",
			}
			return err
		}

		if !healthy {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).
				Warningf("Master %s is unhealthy, initiating failover", masterIP)

			// Master is unhealthy - promote a replica
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
				msg := "failover failed"
				if errors.Is(err, rfservice.ErrPartialReconciliation) {
					msg = "failover incomplete: replica reconfiguration failed"
				}
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: msg,
				}
				return err
			}
			return nil
		}

		master = masterIP

		// Master is healthy - ensure all slaves are connected to it
		err = r.rfChecker.CheckAllSlavesFromMaster(masterIP, rf)
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.SLAVE_WRONG_MASTER, metrics.NOT_APPLICABLE, err)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).
				Warningf("Slave not associated to master: %s", err.Error())
			if err = r.rfHealer.SetMasterOnAll(masterIP, rf); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "failed to configure slaves",
				}
				return err
			}
		}

	default:
		// Multiple masters - error state
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, errors.New("multiple masters detected"))
		errorMsg := "multiple masters detected, fix manually"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		return errors.New(errorMsg)
	}

	// Apply custom Redis configuration
	err = r.applyRedisCustomConfig(rf)
	var holdRollout bool
	if err == nil {
		holdRollout, err = r.ensureRedisMaxMemory(rf, master)
	}
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to apply custom config",
		}
		return err
	}

	// Update stale pods
	if !holdRollout {
		err = r.UpdateRedisesPods(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to update redis pods",
			}
			return err
		}
	}

	return nil
}

func (r *RedisFailoverHandler) checkAndHealBootstrapMode(rf *redisfailoverv1.RedisFailover) error {

	if !r.rfChecker.IsRedisRunning(rf) {
		errorMsg := "not all replicas running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		r.k8sservice.UpdateRedisFailoverStatus(context.Background(), rf.Namespace, rf, metav1.PatchOptions{})
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of redis mismatch, waiting for redis statefulset reconcile")
		return nil
	}

	// Before UpdateRedisesPods, so a lowered memory limit can hold the rollout.
	holdRollout, err := r.ensureRedisMaxMemory(rf, "")
	if err != nil {
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to set Redis maxmemory",
		}
		return err
	}
	if !holdRollout {
		err = r.UpdateRedisesPods(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to update Redis PODs",
			}
			return err
		}
	}
	err = r.applyRedisCustomConfig(rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_REDIS_CONFIG, metrics.NOT_APPLICABLE, err)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to set Redis custom config",
		}
		return err
	}

	bootstrapSettings := rf.Spec.BootstrapNode
	err = r.rfHealer.SetExternalMasterOnAll(bootstrapSettings.Host, bootstrapSettings.Port, rf)
	setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.APPLY_EXTERNAL_MASTER, metrics.NOT_APPLICABLE, err)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to set external master to all",
		}
		return err
	}

	if rf.SentinelsAllowed() {
		if !r.rfChecker.IsSentinelRunning(rf) {
			errorMsg := "not all replicas running"
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: errorMsg,
			}
			r.k8sservice.UpdateRedisFailoverStatus(context.Background(), rf.Namespace, rf, metav1.PatchOptions{})
			setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of sentinel mismatch, waiting for sentinel deployment reconcile")
			return nil
		} else {
			r.k8sservice.UpdateRedisFailoverStatus(context.Background(), rf.Namespace, rf, metav1.PatchOptions{})
		}

		sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to get sentinels IPs",
			}
			return err
		}
		for _, sip := range sentinels {
			err = r.rfChecker.CheckSentinelMonitor(sip, bootstrapSettings.Host, bootstrapSettings.Port)
			setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
			if err != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Fixing sentinel not monitoring expected master: %s", err.Error())
				if err := r.rfHealer.NewSentinelMonitorWithPort(sip, bootstrapSettings.Host, bootstrapSettings.Port, rf); err != nil {
					rf.Status = redisfailoverv1.RedisFailoverStatus{
						State:   redisfailoverv1.NotHealthyState,
						Message: "unable to check sentinel monitor",
					}
					return err
				}
			}
		}
		return r.checkAndHealSentinels(rf, sentinels)
	}
	return nil
}

func (r *RedisFailoverHandler) applyRedisCustomConfig(rf *redisfailoverv1.RedisFailover) error {
	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return err
	}
	for _, rip := range redises {
		if err := r.rfHealer.SetRedisCustomConfig(rip, rf); err != nil {
			// A pod on a downed node cannot be configured; skip it rather than
			// aborting the whole reconcile, so the reachable pods and the rest of
			// the heal still run. A non-connection error (bad config value, auth)
			// is a real problem and still stops here.
			if redis.IsUnreachableError(err) {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Skipping custom config on unreachable redis %s: %s", rip, err.Error())
				continue
			}
			return err
		}
	}
	return nil
}

// ensureRedisMaxMemory applies the managed maxmemory. master is "" when
// bootstrapping. It reports whether the pod rollout must be held.
func (r *RedisFailoverHandler) ensureRedisMaxMemory(rf *redisfailoverv1.RedisFailover, master string) (bool, error) {
	if rf.Spec.Redis.MaxMemory == nil {
		return false, nil
	}
	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return false, err
	}
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	if master != "" {
		// A failover earlier in this reconcile would make the check run against
		// a replica. Without a master nothing is checked, so hold the rollout.
		if master, err = r.rfChecker.GetMasterIP(rf); err != nil {
			logger.Warningf("Skipping maxmemory and the pod rollout, unable to resolve the master: %s", err.Error())
			return true, nil
		}
	}
	result, err := r.rfHealer.EnsureRedisMaxMemory(rf, master, redises)
	if err != nil {
		return false, err
	}
	if result.Message != "" {
		logger.Warningf("%s", result.Message)
		rf.Status.Message = result.Message
	}
	if result.HoldRollout {
		logger.Warningf("Holding the pod rollout until maxmemory fits the lowered memory limit")
	}
	return result.HoldRollout, nil
}

func (r *RedisFailoverHandler) checkAndHealSentinels(rf *redisfailoverv1.RedisFailover, sentinels []string) error {
	for _, sip := range sentinels {
		err := r.rfChecker.CheckSentinelNumberInMemory(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_NUMBER_IN_MEMORY_MISMATCH, sip, err)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Sentinel %s mismatch number of sentinels in memory. resetting", sip)
			if err := r.rfHealer.RestoreSentinel(sip); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to restore sentinel",
				}
				return err
			}
		}

	}
	for _, sip := range sentinels {
		err := r.rfChecker.CheckSentinelSlavesNumberInMemory(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH, sip, err)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Sentinel %s mismatch number of expected slaves in memory. resetting", sip)
			if err := r.rfHealer.RestoreSentinel(sip); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to restore sentinel",
				}
				return err
			}
		}
	}
	for _, sip := range sentinels {
		err := r.rfHealer.SetSentinelCustomConfig(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.APPLY_SENTINEL_CONFIG, sip, err)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to set sentinel custom config",
			}
			return err
		}
	}
	return nil
}

func getRedisPort(p int32) string {
	return strconv.Itoa(int(p))
}

func setRedisCheckerMetrics(metricsClient metrics.Recorder, mode /* redis or sentinel? */ string, rfNamespace string, rfName string, property string, IP string, err error) {
	switch mode {
	case "sentinel":
		if err != nil {
			metricsClient.RecordSentinelCheck(rfNamespace, rfName, property, IP, metrics.STATUS_UNHEALTHY)
		} else {
			metricsClient.RecordSentinelCheck(rfNamespace, rfName, property, IP, metrics.STATUS_HEALTHY)
		}
	case "redis":
		if err != nil {
			metricsClient.RecordRedisCheck(rfNamespace, rfName, property, IP, metrics.STATUS_UNHEALTHY)
		} else {
			metricsClient.RecordRedisCheck(rfNamespace, rfName, property, IP, metrics.STATUS_HEALTHY)
		}
	}
}

// updateStatus patches rf's status to the API server, stamping LastChanged
// with the current time only when the health state actually transitioned.
// The branches leading up to this (checkAndHeal*) each rebuild rf.Status
// from scratch (State/Message only) without carrying LastChanged forward,
// so oldLastChanged - captured before any of those run - is what restores
// it on a non-transition; otherwise every steady-state reconcile would
// patch LastChanged back to empty, erasing the last recorded transition.
func updateStatus(k8sservice k8s.Services, rf *redisfailoverv1.RedisFailover, oldState string, oldLastChanged string) {
	if oldState != rf.Status.State {
		rf.Status.LastChanged = time.Now().Format(time.RFC3339)
	} else {
		rf.Status.LastChanged = oldLastChanged
	}
	k8sservice.UpdateRedisFailoverStatus(context.Background(), rf.Namespace, rf, metav1.PatchOptions{})
}
