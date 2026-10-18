package redisfailover

import (
	"errors"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

// UpdateRedisesPods replaces one stale Redis pod in each call: the replicas
// first, and the master last. A pod is stale when its revision is not the
// update revision of the StatefulSet.
func (r *RedisFailoverHandler) UpdateRedisesPods(rf *redisfailoverv1.RedisFailover) (err error) {
	var wait *rolloutWait
	// A replica that is not synced is no wait of a rollout, but an
	// autoscaler step waits for it.
	unsynced := false
	defer func() {
		if err == nil {
			r.reportRolloutWait(rf, wait)
			err = r.recordAutoscaleRollout(rf, wait != nil || unsynced)
		}
	}()

	redises, err := r.rfChecker.GetRedisesIPs(rf)
	if err != nil {
		return err
	}

	masterIP := ""
	if !rf.Bootstrapping() {
		masterIP, _ = r.rfChecker.GetMasterIP(rf)
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("masterIP", masterIP).Debug("got master IP")
	}
	scan, wait, err := r.scanReplicas(rf, redises, masterIP)
	unsynced = scan.unsynced
	if err != nil || scan.done {
		return err
	}
	ssUR := scan.updateRevision
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("ssUR", ssUR).Debug("got StatefulSet update revision")

	redisesPods := scan.unsyncedStale
	if len(redisesPods) == 0 {
		redisesPods, err = r.rfChecker.GetRedisesSlavesPods(rf)
		if err != nil {
			return err
		}
	}
	var done bool
	if wait, done, err = r.replaceStaleReplica(rf, redisesPods, ssUR, len(scan.unsyncedStale) > 0); err != nil || done {
		return err
	}
	if rf.Bootstrapping() {
		return nil
	}
	wait, err = r.replaceStaleMaster(rf, masterIP, ssUR)
	return err
}

// rolloutScan is what the check of the replicas finds before the rollout.
// done is true when UpdateRedisesPods must return.
type rolloutScan struct {
	updateRevision string
	podNames       map[string]string
	unsyncedStale  []string
	unsynced       bool
	done           bool
}

// scanReplicas checks the sync state of each replica. No performed updates
// when nodes are syncing, still not connected, etc. The exception is an
// unsynced replica on a stale revision, for example on an image that cannot
// load the RDB of the master. It has no data to lose, and a wait for it can be
// infinite. Its replacement is on the update revision, so the rollout waits
// for it and does not replace it again.
func (r *RedisFailoverHandler) scanReplicas(rf *redisfailoverv1.RedisFailover, redises []string, masterIP string) (rolloutScan, *rolloutWait, error) {
	var scan rolloutScan
	for _, rip := range redises {
		if rip == masterIP {
			continue
		}
		if wait, err := r.scanReplica(rf, rip, masterIP, &scan); err != nil || scan.done {
			return scan, wait, err
		}
	}
	if scan.podNames == nil {
		var wait *rolloutWait
		var err error
		if scan.updateRevision, wait, err = r.updateRevision(rf); err != nil || wait != nil {
			scan.done = true
			return scan, wait, err
		}
	}
	return scan, nil, nil
}

// scanReplica checks the replica at rip, and records an unsynced replica on a
// stale revision in scan.
func (r *RedisFailoverHandler) scanReplica(rf *redisfailoverv1.RedisFailover, rip, masterIP string, scan *rolloutScan) (*rolloutWait, error) {
	ready, err := r.rfChecker.CheckRedisSlavesReady(rip, rf)
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("ready", ready).Debug("got secondary state")
	if err != nil || ready {
		return nil, err
	}
	scan.unsynced = true
	// Without a known master, rip can be the master. While bootstrapping,
	// rip can hold the only copy of the data. Do not replace it.
	if masterIP == "" {
		scan.done = true
		return r.replicaRolloutWait(rf, rip)
	}
	if scan.podNames == nil {
		var wait *rolloutWait
		if scan.updateRevision, wait, err = r.updateRevision(rf); err != nil || wait != nil {
			scan.done = true
			return wait, err
		}
		if scan.podNames, err = r.redisPodNamesByIP(rf); err != nil {
			return nil, err
		}
	}
	pod, ok := scan.podNames[rip]
	if !ok {
		scan.done = true
		return r.replicaRolloutWait(rf, rip)
	}
	revision, err := r.rfChecker.GetRedisRevisionHash(pod, rf)
	if err != nil {
		return nil, err
	}
	if revision == scan.updateRevision {
		scan.done = true
		return r.replicaRolloutWait(rf, rip)
	}
	scan.unsyncedStale = append(scan.unsyncedStale, pod)
	return nil, nil
}

// replaceStaleReplica replaces the first pod in pods that is not on the
// update revision. It reports whether UpdateRedisesPods must return.
func (r *RedisFailoverHandler) replaceStaleReplica(rf *redisfailoverv1.RedisFailover, pods []string, ssUR string, unsyncedStale bool) (*rolloutWait, bool, error) {
	for _, pod := range pods {
		revision, err := r.rfChecker.GetRedisRevisionHash(pod, rf)
		if err != nil {
			return nil, true, err
		}
		if revision == ssUR {
			continue
		}
		if wait, err := r.redisPodsSettled(rf, ssUR); err != nil || wait != nil {
			return wait, true, err
		}
		if recreate, err := r.resizeInPlace(rf, pod, ssUR); err != nil || !recreate {
			return nil, true, err
		}
		// A master also fails the sync check, and sentinel can promote the
		// candidate after GetMasterIP. Read the role again before the delete.
		if unsyncedStale {
			replicas, err := r.rfChecker.GetRedisesSlavesPods(rf)
			if err != nil || !slices.Contains(replicas, pod) {
				return nil, true, err
			}
		}
		err = r.rfHealer.DeletePod(pod, rf)
		if err != nil {
			return nil, true, err
		}
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("revision", revision).WithField("pod", pod).Debug("deleted secondary pod")
		return nil, true, nil
	}
	return nil, false, nil
}

// replaceStaleMaster replaces the master pod when it is not on the update
// revision.
func (r *RedisFailoverHandler) replaceStaleMaster(rf *redisfailoverv1.RedisFailover, masterIP, ssUR string) (*rolloutWait, error) {
	master, err := r.rfChecker.GetRedisesMasterPod(rf)
	if err != nil {
		return nil, err
	}

	masterRevision, err := r.rfChecker.GetRedisRevisionHash(master, rf)
	if err != nil {
		return nil, err
	}
	if masterRevision == ssUR {
		// The next rollout counts the refusals from zero.
		r.handoverRetries.Delete(failoverKey(rf))
		return nil, nil
	}
	// Resizing in place needs no failover, so it skips the gate below.
	if wait, err := r.redisPodsSettled(rf, ssUR); err != nil || wait != nil {
		return wait, err
	}
	if recreate, err := r.resizeInPlace(rf, master, ssUR); err != nil || !recreate {
		return nil, err
	}
	wait, err := r.sentinelRolloutWait(rf)
	if err != nil || (wait != nil && !wait.passed) {
		return wait, err
	}
	// A passed wait stays tracked, so that the next reconcile does not wait
	// again. Another wait of the rollout replaces it, and then the bound
	// starts again.
	if allowed, err := r.masterDeleteAllowed(rf, masterIP, master); err != nil || !allowed {
		return wait, err
	}

	err = r.rfHealer.DeletePod(master, rf)
	if err != nil {
		return nil, err
	}
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("revision", masterRevision).WithField("pod", master).Debug("deleted primary pod")
	return nil, nil
}

// masterDeleteAllowed reports whether the rollout can delete the stale master
// pod now.
//
// The deletion of the master makes Sentinel fail over. Sentinel uses its own
// list of replicas, and that list is slower than the Redis readiness checked
// before. Thus each Sentinel must know a quorum (majority) of the replicas
// first. Without a known replica, the failover fails with NOGOODSLAVE, and the
// outage is longer until the operator promotes a replica. One replica that
// stays unavailable must not block the master replacement, so a quorum is
// sufficient.
//
// In operator-managed mode, no Sentinel exists. With more than one pod, the
// master hands its role over to a replica first, so that a master exists at
// all times. Without FAILOVER, or with one pod, case 0 of
// checkAndHealOperatorManagedMode waits until the master pod stops, and then
// elects a master.
func (r *RedisFailoverHandler) masterDeleteAllowed(rf *redisfailoverv1.RedisFailover, masterIP, master string) (bool, error) {
	if rf.OperatorManagedFailover() {
		if rf.Spec.Redis.Replicas <= 1 {
			return true, nil
		}
		handled, err := r.handOverMaster(rf, masterIP, master)
		return !handled, err
	}
	sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
	if err != nil {
		return false, err
	}
	for _, sip := range sentinels {
		if err := r.rfChecker.CheckSentinelSlavesNumberQuorumInMemory(sip, rf); err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("Waiting for sentinels to see a quorum of slaves before replacing the master: %s", err.Error())
			return false, nil
		}
	}
	return true, nil
}

const (
	sentinelRolloutMsg = "the master pod replacement waits for the Sentinel rollout"
	// sentinelRolloutRecheck is the next check of a wait on the Sentinel
	// rollout, because a change of the Deployment status starts no reconcile.
	sentinelRolloutRecheck = 5 * time.Second
)

// sentinelRolloutWait returns a wait until the Sentinel Deployment has
// finished its rollout and no Sentinel pod is terminating, or nil.
//
// The preStop hook of the master sends SENTINEL FAILOVER through the Sentinel
// Service. When the Sentinel that accepts it stops, no other Sentinel knows
// the promoted replica. A second failover can then lose the writes that this
// replica accepted.
//
// A Sentinel rollout that does not finish must not hold the master pod on its
// revision for ever. After rolloutStallTimeout, the wait is passed, and the
// master replacement continues. The start of the wait is in memory, so a
// restart of the operator starts the bound again.
func (r *RedisFailoverHandler) sentinelRolloutWait(rf *redisfailoverv1.RedisFailover) (*rolloutWait, error) {
	if rf.OperatorManagedFailover() {
		return nil, nil
	}
	name := rfservice.GetSentinelName(rf)
	deployment, err := r.k8sservice.GetDeployment(rf.Namespace, name)
	if err != nil {
		return nil, err
	}
	reason := sentinelRolloutReason(deployment)
	if reason == "" {
		pods, err := r.k8sservice.GetDeploymentPods(rf.Namespace, name)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				reason = fmt.Sprintf("pod %s of Deployment %s is terminating", pod.Name, name)
				break
			}
		}
	}
	if reason == "" {
		return nil, nil
	}
	// A new generation is a new rollout, so its wait starts from zero.
	wait := &rolloutWait{uid: types.UID(fmt.Sprintf("%s/%d", deployment.UID, deployment.Generation)), reason: reason}
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	key := passwordKey(rf)
	v, _ := r.rolloutWaits.Load(key)
	last, ok := v.(rolloutWait)
	switch {
	case !ok || last.uid != wait.uid:
		logger.Infof("%s: %s", sentinelRolloutMsg, reason)
	case last.passed || r.now().Sub(last.since) >= rolloutStallTimeout:
		// The checks set the status again in each reconcile.
		msg := fmt.Sprintf("rollout waiting for more than %dm: %s, so the master pod replacement continues before the end of the Sentinel rollout", int(rolloutStallTimeout.Minutes()), reason)
		rf.Status.Message = msg
		if last.passed {
			logger.Debugf("%s", msg)
		} else {
			logger.Warningf("%s", msg)
		}
		wait.since, wait.passed = last.since, true
		r.rolloutWaits.Store(key, *wait)
		return wait, nil
	default:
		logger.Debugf("%s: %s", sentinelRolloutMsg, reason)
	}
	rf.Status.Message = sentinelRolloutMsg
	if r.requeue != nil {
		r.requeue(failoverKey(rf), sentinelRolloutRecheck)
	}
	return wait, nil
}

// sentinelRolloutReason returns why the rollout of the Sentinel Deployment d
// is not finished, or "". An available pod is ready, so the ready count needs
// no check. The terminating pods of an old ReplicaSet are not in the status,
// so the caller checks the pods too.
func sentinelRolloutReason(d *appsv1.Deployment) string {
	if d.Status.ObservedGeneration < d.Generation {
		return fmt.Sprintf("the Deployment controller did not observe generation %d of Deployment %s", d.Generation, d.Name)
	}
	want := ptr.Deref(d.Spec.Replicas, 1)
	s := d.Status
	if s.Replicas != want || s.UpdatedReplicas != want || s.AvailableReplicas != want {
		return fmt.Sprintf("Deployment %s has %d pods, %d updated, %d ready and %d available, of %d", d.Name, s.Replicas, s.UpdatedReplicas, s.ReadyReplicas, s.AvailableReplicas, want)
	}
	return ""
}

// updateRevision returns the update revision of the redis StatefulSet, or a
// wait while its status does not show the latest template. Then each pod on
// the latest template looks stale. A resize or a delete toward the revision
// of the status can recreate a pod for a replaced template, for example after
// an autoscaler revert.
func (r *RedisFailoverHandler) updateRevision(rf *redisfailoverv1.RedisFailover) (string, *rolloutWait, error) {
	rev, err := r.rfChecker.GetStatefulSetUpdateRevision(rf)
	if errors.Is(err, rfservice.ErrUpdateRevisionNotObserved) {
		return "", &rolloutWait{reason: err.Error()}, nil
	}
	return rev, nil, err
}

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
// delete a pod two times.
func (r *RedisFailoverHandler) redisPodNamesByIP(rf *redisfailoverv1.RedisFailover) (map[string]string, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, pod := range pods.Items {
		if pod.Status.PodIP != "" && pod.DeletionTimestamp == nil {
			names[pod.Status.PodIP] = pod.Name
		}
	}
	return names, nil
}

// redisPodsRunning reports whether all redis pods run. When not, it first
// deletes a stale pod that the scheduler cannot place. The StatefulSet uses
// OnDelete, so only the operator replaces such a pod.
//
// The step runs in each mode before the gates on the running pods. A stale
// Pending pod leaves fewer pods running than a quorum with one or two
// replicas, and bootstrapping needs all pods. Without the step, the gates end
// each reconcile before the rollout, and the only pod of a failover with one
// replica stays Pending for ever.
//
// The step needs no quorum and no wait for the last replacement. The pod is
// Pending, the scheduler did not place it, and no container of it started, so
// it holds no data in memory and serves no client. Its PVC stays. Its revision
// is not the update revision of a StatefulSet whose status is current, and
// the StatefulSet creates the replacement on the update revision, so the step
// does not delete the replacement again.
//
// An error of the step is a warning only, so that the gates and the heal steps
// after them run as without the step. A webhook can deny the delete, for
// example. With all pods running, the step makes no API call beyond the gate.
func (r *RedisFailoverHandler) redisPodsRunning(rf *redisfailoverv1.RedisFailover) bool {
	if r.rfChecker.IsRedisRunning(rf) {
		return true
	}
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	pod, err := r.unschedulableStalePod(rf)
	if err != nil {
		logger.Warningf("unable to look for a stale redis pod that the scheduler cannot place: %v", err)
		return false
	}
	if pod == nil {
		return false
	}
	logger = logger.WithField("pod", pod.Name)
	logger.Infof("deleting the stale redis pod %s, which the scheduler cannot place", pod.Name)
	// Another client can delete the pod first, and the StatefulSet can create
	// it again with the same name. The UID keeps the delete off the new pod.
	err = r.k8sservice.DeletePodWithUID(rf.Namespace, pod.Name, pod.UID)
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsConflict(err):
		logger.Debugf("the stale redis pod %s is gone already: %v", pod.Name, err)
	case err != nil:
		logger.Warningf("unable to delete the stale redis pod %s: %v", pod.Name, err)
	}
	return false
}

// unschedulableStalePod returns a pod on a stale revision that the scheduler
// cannot place and that never started a container, or nil.
func (r *RedisFailoverHandler) unschedulableStalePod(rf *redisfailoverv1.RedisFailover) (*corev1.Pod, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	var candidates []*corev1.Pod
	for i := range pods.Items {
		if pod := &pods.Items[i]; unplaceable(pod) {
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// Before the StatefulSet controller sees a change of the spec, the update
	// revision in the status is the one before. A pod that the controller
	// created on the new revision would then look stale.
	ss, err := r.k8sservice.GetStatefulSet(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	if ss == nil || ss.Status.UpdateRevision == "" || ss.Status.ObservedGeneration < ss.Generation {
		return nil, nil
	}
	for _, pod := range candidates {
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] != ss.Status.UpdateRevision {
			return pod, nil
		}
	}
	return nil, nil
}

// unplaceable reports whether pod is Pending because the scheduler cannot
// place it, and never started a container. Such a pod holds no data.
func unplaceable(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodPending || pod.DeletionTimestamp != nil {
		return false
	}
	unschedulable := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled {
			unschedulable = c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable
		}
	}
	started := slices.ContainsFunc(slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses), func(cs corev1.ContainerStatus) bool {
		return cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil || cs.RestartCount > 0
	})
	return unschedulable && !started
}

// resizeInPlace tries to move a stale pod to the update revision without
// recreating it. It reports whether the pod has to be recreated instead.
func (r *RedisFailoverHandler) resizeInPlace(rf *redisfailoverv1.RedisFailover, pod, updateRevision string) (bool, error) {
	result, err := r.rfHealer.ResizePodInPlace(rf, pod, updateRevision, r.autoscalerLimits(rf))
	if err != nil {
		return false, err
	}
	if result.Action == rfservice.ResizeWaiting && result.Message != "" {
		rf.Status.Message = result.Message
	}
	// A recreate restarts Redis for a change that no user made, so the pod
	// stays on its revision, and the refusal is kept for the autoscaler.
	if result.AutoscalerRevision && result.Class == rfservice.ResizePermanent {
		refusal := &resizeRefusal{pod: pod, class: result.Class, message: result.Message}
		r.autoscaleRollout.Store(failoverKey(rf), autoscaleRollout{refusal: refusal})
		r.logResizeRefusal(rf, refusal)
		// The rollout holds also a change of the spec, so the message shows
		// it. The state stays, because the master serves the clients.
		rf.Status.Message = "rollout holds " + refusal.String()
		return false, nil
	}
	return result.Action == rfservice.ResizeRecreate, nil
}

// redisPodsSettled returns what the rollout waits on until the last redis pod
// replacement has finished: the StatefulSet has all its pods, none is being
// deleted, and every pod already on the update revision is ready. Pod events
// start the next reconcile right after a delete, so without this check a
// rollout would delete several pods at once.
func (r *RedisFailoverHandler) redisPodsSettled(rf *redisfailoverv1.RedisFailover, updateRevision string) (*rolloutWait, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	wait := func(w *rolloutWait) (*rolloutWait, error) {
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Infof("redis rollout waits: %s", w)
		return w, nil
	}
	if len(pods.Items) < int(rf.Spec.Redis.Replicas) {
		return wait(&rolloutWait{reason: fmt.Sprintf("%d of %d pods exist", len(pods.Items), rf.Spec.Redis.Replicas)})
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			return wait(&rolloutWait{uid: pod.UID, pod: pod.Name, reason: "terminating"})
		}
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] == updateRevision && !util.PodIsReady(pod) {
			return wait(&rolloutWait{uid: pod.UID, pod: pod.Name, reason: "not ready"})
		}
	}
	return nil, nil
}

// replicaRolloutWait returns what a pending rollout waits on while the replica
// at ip is not synced with its master, or nil without a pending rollout.
func (r *RedisFailoverHandler) replicaRolloutWait(rf *redisfailoverv1.RedisFailover, ip string) (*rolloutWait, error) {
	updateRevision, unobserved, err := r.updateRevision(rf)
	if err != nil || unobserved != nil {
		return unobserved, err
	}
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	var wait *rolloutWait
	pending := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] != updateRevision {
			pending = true
		}
		if pod.Status.PodIP == ip && pod.DeletionTimestamp == nil {
			wait = &rolloutWait{uid: pod.UID, pod: pod.Name, reason: "not synced with the master"}
		}
	}
	if !pending {
		return nil, nil
	}
	return wait, nil
}

// rolloutStallTimeout is how long the rollout waits on the same pod before the
// status message says so. A full sync, even of a dataset of tens of GB, takes
// less. It also ends the wait of the master replacement for the Sentinel
// rollout.
var rolloutStallTimeout = 10 * time.Minute

// rolloutWait is what the redis pod rollout waits on: a pod, or, without pod,
// the Sentinel Deployment (with uid) or a missing pod (without uid). passed is
// true after the master replacement stopped its wait for the Sentinel
// rollout.
type rolloutWait struct {
	uid    types.UID
	pod    string
	reason string
	since  time.Time
	passed bool
}

func (w *rolloutWait) String() string {
	if w.pod == "" {
		return w.reason
	}
	return "pod " + w.pod + " is " + w.reason
}

// reportRolloutWait tracks how long the rollout has waited on the same pod,
// and sets the status message once that is longer than rolloutStallTimeout.
// A nil wait means the rollout is not waiting.
func (r *RedisFailoverHandler) reportRolloutWait(rf *redisfailoverv1.RedisFailover, wait *rolloutWait) {
	key := passwordKey(rf)
	if wait == nil {
		r.rolloutWaits.Delete(key)
		return
	}
	v, _ := r.rolloutWaits.Load(key)
	if last, ok := v.(rolloutWait); ok && last.uid == wait.uid {
		wait.since = last.since
	} else {
		wait.since = r.now()
		r.rolloutWaits.Store(key, *wait)
	}
	// The Sentinel rollout wait reports its own end at the bound.
	if wait.passed || r.now().Sub(wait.since) < rolloutStallTimeout {
		return
	}
	msg := fmt.Sprintf("rollout waiting for more than %dm: %s", int(rolloutStallTimeout.Minutes()), wait.reason)
	if wait.pod != "" {
		msg = fmt.Sprintf("rollout waiting on pod %s for more than %dm: %s", wait.pod, int(rolloutStallTimeout.Minutes()), wait.reason)
	}
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Warningf("%s", msg)
	if rf.Status.Message != "" {
		msg = rf.Status.Message + "; " + msg
	}
	rf.Status.Message = msg
}
