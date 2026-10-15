package redisfailover

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/saremox/redis-operator/service/k8s"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/redis"
)

// UpdateRedisesPods replaces one stale Redis pod in each call: the replicas
// first, and the master last. A pod is stale when its revision is not the
// update revision of the StatefulSet.
func (r *RedisFailoverHandler) UpdateRedisesPods(rf *redisfailoverv1.RedisFailover) (err error) {
	var wait *rolloutWait
	defer func() {
		if err == nil {
			r.reportRolloutWait(rf, wait)
			err = r.recordAutoscaleRollout(rf, wait != nil)
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
	// No performed updates when nodes are syncing, still not connected, etc.
	// The exception is an unsynced replica on a stale revision, for example on
	// an image that cannot load the RDB of the master. It has no data to lose,
	// and a wait for it can be infinite. Its replacement is on the update
	// revision, so the rollout waits for it and does not replace it again.
	ssUR := ""
	var podNames map[string]string
	var unsyncedStale []string
	for _, rip := range redises {
		if rip != masterIP {
			ready, err := r.rfChecker.CheckRedisSlavesReady(rip, rf)
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("ready", ready).Debug("got secondary state")
			if err != nil {
				return err
			}
			if ready {
				continue
			}
			// Without a known master, rip can be the master. While bootstrapping,
			// rip can hold the only copy of the data. Do not replace it.
			if masterIP == "" {
				wait, err = r.replicaRolloutWait(rf, rip)
				return err
			}
			if podNames == nil {
				if ssUR, err = r.rfChecker.GetStatefulSetUpdateRevision(rf); err != nil {
					return err
				}
				if podNames, err = r.redisPodNamesByIP(rf); err != nil {
					return err
				}
			}
			pod, ok := podNames[rip]
			if !ok {
				wait, err = r.replicaRolloutWait(rf, rip)
				return err
			}
			revision, err := r.rfChecker.GetRedisRevisionHash(pod, rf)
			if err != nil {
				return err
			}
			if revision == ssUR {
				wait, err = r.replicaRolloutWait(rf, rip)
				return err
			}
			unsyncedStale = append(unsyncedStale, pod)
		}
	}

	if podNames == nil {
		if ssUR, err = r.rfChecker.GetStatefulSetUpdateRevision(rf); err != nil {
			return err
		}
	}
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("ssUR", ssUR).Debug("got StatefulSet update revision")

	redisesPods := unsyncedStale
	if len(redisesPods) == 0 {
		redisesPods, err = r.rfChecker.GetRedisesSlavesPods(rf)
		if err != nil {
			return err
		}
	}

	for _, pod := range redisesPods {
		revision, err := r.rfChecker.GetRedisRevisionHash(pod, rf)
		if err != nil {
			return err
		}
		if revision != ssUR {
			if wait, err = r.redisPodsSettled(rf, ssUR); err != nil || wait != nil {
				return err
			}
			if recreate, err := r.resizeInPlace(rf, pod, ssUR); err != nil || !recreate {
				return err
			}
			// A master also fails the sync check, and sentinel can promote the
			// candidate after GetMasterIP. Read the role again before the delete.
			if len(unsyncedStale) > 0 {
				replicas, err := r.rfChecker.GetRedisesSlavesPods(rf)
				if err != nil || !slices.Contains(replicas, pod) {
					return err
				}
			}
			err = r.rfHealer.DeletePod(pod, rf)
			if err != nil {
				return err
			}
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("revision", revision).WithField("pod", pod).Debug("deleted secondary pod")
			return nil
		}
	}

	if !rf.Bootstrapping() {
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
			if wait, err = r.redisPodsSettled(rf, ssUR); err != nil || wait != nil {
				return err
			}
			if recreate, err := r.resizeInPlace(rf, master, ssUR); err != nil || !recreate {
				return err
			}

			// The deletion of the master makes Sentinel fail over. Sentinel uses
			// its own list of replicas, and that list is slower than the Redis
			// readiness checked above. Thus each Sentinel must know a quorum
			// (majority) of the replicas first. Without a known replica, the
			// failover fails with NOGOODSLAVE, and the outage is longer until the
			// operator promotes a replica. One replica that stays unavailable
			// must not block the master replacement, so a quorum is sufficient.
			//
			// In operator-managed mode, no Sentinel exists. With more than one
			// pod, the master hands its role over to a replica first, so that a
			// master exists at all times. Without FAILOVER, or with one pod,
			// case 0 of checkAndHealOperatorManagedMode waits until the master pod
			// stops, and then elects a master.
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
			} else if rf.Spec.Redis.Replicas > 1 {
				if handled, err := r.handOverMaster(rf, masterIP, master); err != nil || handled {
					return err
				}
			}

			err = r.rfHealer.DeletePod(master, rf)
			if err != nil {
				return err
			}
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("revision", masterRevision).WithField("pod", master).Debug("deleted primary pod")
			return nil
		}
		// The next rollout counts the refusals from zero.
		r.handoverRetries.Delete(failoverKey(rf))
	}
	return nil
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
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodPending || pod.DeletionTimestamp != nil {
			continue
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
		if unschedulable && !started {
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
		r.autoscaleRollout.Store(failoverKey(rf), autoscaleRollout{refusal: &resizeRefusal{pod: pod, class: result.Class, message: result.Message}})
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
	updateRevision, err := r.rfChecker.GetStatefulSetUpdateRevision(rf)
	if err != nil {
		return nil, err
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
// less.
var rolloutStallTimeout = 10 * time.Minute

// rolloutWait is what the redis pod rollout waits on: a pod, or, without uid,
// a missing one.
type rolloutWait struct {
	uid    types.UID
	pod    string
	reason string
	since  time.Time
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
		wait.since = time.Now()
		r.rolloutWaits.Store(key, *wait)
	}
	if time.Since(wait.since) < rolloutStallTimeout {
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

// masterPodStopping reports whether the master pod is in deletion but still
// ready, so it can still accept writes. A pod on a lost node is not ready, so
// it does not block an election.
func (r *RedisFailoverHandler) masterPodStopping(rf *redisfailoverv1.RedisFailover) (bool, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil && rfservice.IsMasterPod(pod) && util.PodIsReady(pod) {
			r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).WithField("pod", pod.Name).Info("waiting for the stopping master pod to exit")
			return true, nil
		}
	}
	return false, nil
}

// findRedisPod ignores a pod in deletion, because that pod does not come back.
func (r *RedisFailoverHandler) findRedisPod(rf *redisfailoverv1.RedisFailover, match func(*corev1.Pod) bool) (*corev1.Pod, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil && match(pod) {
			return pod, nil
		}
	}
	return nil, nil
}

// unreachableMasterPod returns the pod labelled master only when it is not
// ready. A ready master pod answered as a replica, or GetNumberMasters
// returned ErrRedisNotAnswering.
func (r *RedisFailoverHandler) unreachableMasterPod(rf *redisfailoverv1.RedisFailover) (*corev1.Pod, error) {
	return r.findRedisPod(rf, func(pod *corev1.Pod) bool {
		return rfservice.IsMasterPod(pod) && !util.PodIsReady(pod)
	})
}

// recordMasterNotAnswering starts the wait of a ready master pod that does not
// answer at its first missed check. Errors are ignored, because the reconcile
// fails already.
func (r *RedisFailoverHandler) recordMasterNotAnswering(rf *redisfailoverv1.RedisFailover, err error) {
	if !errors.Is(err, rfservice.ErrRedisNotAnswering) {
		return
	}
	if pod, _ := r.findRedisPod(rf, rfservice.IsMasterPod); pod != nil {
		_, _ = r.masterUnreachableSince(rf, pod)
	}
}

// labelledMasterPod returns the pod labelled master only when exactly one
// pod has the label. With more than one, the label does not show which master
// the operator elected.
func (r *RedisFailoverHandler) labelledMasterPod(rf *redisfailoverv1.RedisFailover) (*corev1.Pod, error) {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	var master *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || !rfservice.IsMasterPod(pod) {
			continue
		}
		if master != nil {
			return nil, nil
		}
		master = pod
	}
	return master, nil
}

const (
	masterPodLookupFailed = "unable to look up the master pod"
	masterStoppingMsg     = "no master, waiting for the stopping master pod to exit"
	masterPodStoppingMsg  = "waiting for the stopping master pod to exit"
)

func failoverKey(rf *redisfailoverv1.RedisFailover) string {
	return rf.Namespace + "/" + rf.Name
}

// masterUnreachableSince returns the time of the first check that the master
// pod missed. It stores that time on the pod, so an operator restart or a new
// leader keeps the deadline.
func (r *RedisFailoverHandler) masterUnreachableSince(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod) (time.Time, error) {
	if since, err := time.Parse(time.RFC3339, pod.Annotations[masterUnreachableAnnotation]); err == nil {
		return since, nil
	}
	now := r.now().Truncate(time.Second)
	r.unreachableCleared.Delete(failoverKey(rf))
	return now, r.k8sservice.UpdatePodAnnotations(rf.Namespace, pod.Name, map[string]string{masterUnreachableAnnotation: now.UTC().Format(time.RFC3339)})
}

// clearMasterUnreachable removes the unreachable-since annotation from all
// redis pods, because an old annotation shortens the wait of the next stall.
// A failure is only logged, because the master answers again or was replaced.
func (r *RedisFailoverHandler) clearMasterUnreachable(rf *redisfailoverv1.RedisFailover) {
	key := failoverKey(rf)
	if _, cleared := r.unreachableCleared.Load(key); cleared {
		return
	}
	if err := r.clearUnreachableAnnotations(rf); err != nil {
		r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Warningf("unable to clear the unreachable-since annotation, retrying on the next reconcile: %v", err)
		return
	}
	r.unreachableCleared.Store(key, true)
}

func (r *RedisFailoverHandler) clearUnreachableAnnotations(rf *redisfailoverv1.RedisFailover) error {
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if _, ok := pod.Annotations[masterUnreachableAnnotation]; !ok {
			continue
		}
		if err := r.k8sservice.RemovePodAnnotation(rf.Namespace, pod.Name, masterUnreachableAnnotation); err != nil {
			return err
		}
	}
	return nil
}

// waitForFailover reports whether the master pod still has time to answer
// before failoverTimeout ends.
func (r *RedisFailoverHandler) waitForFailover(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod) (bool, error) {
	timeout := rf.GetFailoverTimeoutDuration()
	// Without a wait there is no deadline to keep, so a failed annotation
	// write must not stop the failover.
	if timeout <= 0 {
		return false, nil
	}
	since, err := r.masterUnreachableSince(rf, pod)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to record when the master became unreachable",
		}
		return false, err
	}
	unreachable := r.now().Sub(since)
	if unreachable >= timeout {
		return false, nil
	}
	// The message holds no elapsed time. Each status change queues a
	// reconcile at once, and the requeue below must set the next check.
	msg := fmt.Sprintf("master unreachable since %s, failing over after %s", since.UTC().Format(time.RFC3339), timeout)
	r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name).Info(msg)
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: msg,
	}
	// The deadline can pass without a pod event, so queue a reconcile for it.
	if r.requeue != nil {
		r.requeue(failoverKey(rf), timeout-unreachable)
	}
	return true, nil
}

// passwordState is the password the Redis pods and the Sentinels were last
// brought onto.
type passwordState struct {
	redis string
	// applied is the password that the running Redis pods were last changed to.
	// It is not redis while a pod yet to start keeps redis in use.
	applied  string
	sentinel string
	// stale makes the next reconcile check the Redis pods again, also when
	// redis is the password: a container restart of a pod on a stale template
	// gives Redis another password.
	stale bool
	// retrySecret makes the next reconcile read and write the Secret again
	// after a temporary error.
	retrySecret bool
}

func passwordKey(rf *redisfailoverv1.RedisFailover) string {
	return rf.Namespace + "/" + rf.Name
}

// Keys of the Secret that keeps redis and applied of passwordState. The
// Sentinel password needs no key: it is set without the one it replaces.
const (
	appliedSecretRedisKey   = "redis"
	appliedSecretAppliedKey = "applied"
)

// loadAppliedPasswords reads the passwords that saveAppliedPasswords kept
// before an operator restart. ok is false when the Secret must not be written
// in this reconcile: an unknown content must not be replaced, and another
// owner keeps its Secret. retry is true for a temporary error.
func (r *RedisFailoverHandler) loadAppliedPasswords(rf *redisfailoverv1.RedisFailover) (saved passwordState, ok, retry bool) {
	name := rfservice.GetRedisAuthAppliedName(rf)
	secret, err := r.k8sservice.GetSecret(rf.Namespace, name)
	if apierrors.IsNotFound(err) {
		r.forgetAppliedSecretLog(rf, true)
		return passwordState{}, true, false
	}
	if err == nil {
		err = k8s.CheckSameController("Secret", secret, &metav1.ObjectMeta{OwnerReferences: r.createOwnerReferences(rf)})
	}
	if err != nil {
		r.logAppliedSecretError(rf, true, "unable to read the applied passwords from Secret "+name, err)
		return passwordState{}, false, retryableSecretError(err)
	}
	r.forgetAppliedSecretLog(rf, true)
	return passwordState{
		redis:   string(secret.Data[appliedSecretRedisKey]),
		applied: string(secret.Data[appliedSecretAppliedKey]),
	}, true, false
}

// retryableSecretError is false for a missing RBAC verb and for another
// owner: a retry at each reconcile cannot change them.
func retryableSecretError(err error) bool {
	var other *k8s.ControlledByOtherError
	return !apierrors.IsForbidden(err) && !errors.As(err, &other)
}

// saveAppliedPasswords keeps the passwords of state in a Secret, so that an
// operator restart does not forget them. A failure costs only that memory, so
// it does not stop the reconcile.
func (r *RedisFailoverHandler) saveAppliedPasswords(rf *redisfailoverv1.RedisFailover, state *passwordState) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            rfservice.GetRedisAuthAppliedName(rf),
			Namespace:       rf.Namespace,
			Labels:          r.getLabels(rf),
			OwnerReferences: r.createOwnerReferences(rf),
		},
		Data: map[string][]byte{
			appliedSecretRedisKey:   []byte(state.redis),
			appliedSecretAppliedKey: []byte(state.applied),
		},
	}
	err := r.k8sservice.CreateOrUpdateSecret(rf.Namespace, secret)
	if err == nil {
		r.forgetAppliedSecretLog(rf, false)
		return
	}
	state.retrySecret = retryableSecretError(err)
	r.logAppliedSecretError(rf, false, "unable to save the applied passwords in Secret "+secret.Name+", an operator restart forgets them", err)
}

// appliedSecretLog is the last error with the applied passwords Secret. read
// is true for an error of a read.
type appliedSecretLog struct {
	read bool
	err  string
}

func (r *RedisFailoverHandler) logAppliedSecretError(rf *redisfailoverv1.RedisFailover, read bool, msg string, err error) {
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	logged := appliedSecretLog{read: read, err: err.Error()}
	prev, _ := r.appliedSecretLogged.Swap(passwordKey(rf), logged)
	if last, ok := prev.(appliedSecretLog); ok && last.err == logged.err {
		logger.Debugf("%s: %v", msg, err)
		return
	}
	logger.Warningf("%s: %v", msg, err)
}

// forgetAppliedSecretLog drops the logged error after a success, so that the
// same error logs a Warning again. A read does not show that a write works.
func (r *RedisFailoverHandler) forgetAppliedSecretLog(rf *redisfailoverv1.RedisFailover, read bool) {
	key := passwordKey(rf)
	v, _ := r.appliedSecretLogged.Load(key)
	if last, ok := v.(appliedSecretLog); ok && (!read || last.read) {
		r.appliedSecretLogged.CompareAndDelete(key, v)
	}
}

// applyPassword applies a changed auth secret to the running Redis and the
// Sentinels. Nothing is checked while both are on the secret.
func (r *RedisFailoverHandler) applyPassword(rf *redisfailoverv1.RedisFailover) error {
	password, err := k8s.GetRedisPassword(r.k8sservice, rf)
	if err != nil {
		return err
	}
	key := passwordKey(rf)
	v, known := r.passwords.Load(key)
	state, _ := v.(passwordState)
	if known && state.redis == password && state.applied == password && state.sentinel == password && !state.stale && !state.retrySecret {
		return nil
	}

	saved, save := state, true
	if !known || state.retrySecret {
		saved, save, state.retrySecret = r.loadAppliedPasswords(rf)
	}
	if !known || state.stale || state.redis != password || state.applied != password {
		previous := []string{password}
		if known {
			previous = []string{state.redis}
			if state.applied != state.redis {
				previous = append(previous, state.applied)
			}
		} else {
			for _, p := range []string{saved.redis, saved.applied} {
				if p != "" && !slices.Contains(previous, p) {
					previous = append(previous, p)
				}
			}
		}
		complete, stale, err := r.rfHealer.ApplyPassword(rf, password, previous)
		if err != nil {
			return err
		}
		state.applied = password
		state.stale = stale
		// A pod yet to start keeps the old password in play. With none known,
		// not even in the Secret, the one every running pod accepts is the
		// best there is.
		if complete {
			state.redis = password
		} else if !known {
			state.redis = cmp.Or(saved.redis, password)
		}
	}
	if save && (state.redis != saved.redis || state.applied != saved.applied) {
		r.saveAppliedPasswords(rf, &state)
	}

	// Every running Redis now accepts the password, so the Sentinels need it
	// to reach them.
	if state.sentinel != password {
		complete, err := r.rfHealer.ApplySentinelPassword(rf, password)
		if err != nil {
			return err
		}
		if complete {
			state.sentinel = password
		}
	}
	// Without the Secret, state.redis can miss the password of a pod yet to
	// start, so the next reconcile starts again without a state.
	if !known && !save && state.retrySecret {
		return nil
	}
	r.passwords.Store(key, state)
	return nil
}

// CheckAndHeal checks the state of the RedisFailover and heals each check
// that fails. It writes the status one time, at the end.
func (r *RedisFailoverHandler) CheckAndHeal(rf *redisfailoverv1.RedisFailover) error {

	observed := rf.Status
	r.autoscaleRollout.Delete(failoverKey(rf))

	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State: redisfailoverv1.HealthyState,
	}

	// A panic ends the reconcile with an error, so the status and cluster_ok must not stay healthy.
	defer func() {
		if p := recover(); p != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:       redisfailoverv1.NotHealthyState,
				Message:     "internal error, see the operator log",
				Autoscaling: observed.Autoscaling,
			}
			updateStatus(r.k8sservice, rf, observed)
			r.mClient.SetClusterError(rf.Namespace, rf.Name)
			panic(p)
		}
		// The checks replace rf.Status as a whole, so the autoscaler sets
		// its part last.
		r.autoscale(rf, observed.Autoscaling)
		updateStatus(r.k8sservice, rf, observed)
	}()

	// Every check below authenticates, so a changed password goes first.
	if err := r.applyPassword(rf); err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to apply the configured password",
		}
		return err
	}

	// The password skips the pods that do not run, so a stale Pending pod
	// does not hold it.
	allRunning := r.redisPodsRunning(rf)

	if rf.Bootstrapping() {
		return r.checkAndHealBootstrapMode(rf, allRunning)
	}

	if rf.OperatorManagedFailover() {
		return r.checkAndHealOperatorManagedMode(rf, allRunning)
	}

	// Sentinel-managed mode checks and heals, in this order:
	//   - a quorum of Redis pods and of Sentinel pods,
	//   - exactly one Redis master, and each replica on that master,
	//   - the custom Redis config and maxmemory,
	//   - the Redis pod rollout,
	//   - a wait while the master pod stops,
	//   - the master that each Sentinel monitors,
	//   - the Sentinel and replica counts in each Sentinel, and the custom
	//     Sentinel config.

	// A quorum (majority) of Running pods is sufficient to heal. One Pending
	// pod, for example from an unschedulable affinity or a lost zone, must not
	// block the election and the Sentinel configuration. The heal steps below
	// use only the Running pods. With all pods running, a quorum runs.
	if !allRunning && !r.rfChecker.IsRedisRunningQuorum(rf) {
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
		// A master pod in deletion is not counted, but it can still accept
		// writes. When CLIENT PAUSE WRITE returns OK, its shutdown script waits for
		// the replicas and asks Sentinel to fail over. An election at this time
		// can lose writes.
		stopping, err := r.masterPodStopping(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to check whether the master is stopping",
			}
			return err
		}
		if stopping {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: masterStoppingMsg,
			}
			return nil
		}
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no masters detected"))
		// With one Redis pod, Sentinel has no replica to promote, so the
		// operator makes that pod the master.
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
		// The operator elects a master when Sentinel cannot fail over:
		//   - the Sentinels have no quorum (CheckSentinelQuorum),
		//   - each Redis pod has 127.0.0.1 as master, after the first start or
		//     after all the pods restarted (CheckIfMasterLocalhost),
		//   - no Sentinel monitors a pod or knows a replica pod
		//     (CheckSentinelsCannotFailover).
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Number of Masters running is 0")
		maxUptime, err := r.rfChecker.GetMaxRedisPodTime(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to get Redis POD time",
			}
			return err
		}

		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("No master available, the max pod uptime is %s", maxUptime.Round(time.Second))
		noqrmCnt, err := r.rfChecker.CheckSentinelQuorum(rf)
		if err != nil {
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Quorum not available for sentinel to choose master, estimated unhealthy sentinels: %d, operator to step in: %v", noqrmCnt, err)
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
			status, err2 := r.rfChecker.CheckIfMasterLocalhost(rf)
			if err2 != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to check if master localhost",
				}
				return err2
			} else if status {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("each available redis has 127.0.0.1 as master, the operator elects a master")
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
				cannot, err3 := r.rfChecker.CheckSentinelsCannotFailover(rf)
				if err3 != nil {
					rf.Status = redisfailoverv1.RedisFailoverStatus{
						State:   redisfailoverv1.NotHealthyState,
						Message: "unable to check whether Sentinel can fail over",
					}
					return err3
				}
				if cannot {
					elected, err3 := r.electMasterForSentinel(rf)
					if err3 != nil || !elected {
						return err3
					}
					break
				}

				// Sentinel can fail over, so the operator waits for it.
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Infof("no master found, wait until failover or fix manually")
				setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no master not fixed, wait until failover or fix manually"))
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "no master, waiting for the Sentinel failover",
				}
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
			return ferr
		}
		if err = r.rfHealer.SetMasterOnAll(freshMaster, rf); err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State: redisfailoverv1.NotHealthyState,
			}
			return err
		}
	}

	if rf.AutoscalingEnabled() {
		r.autoscaleMasters.Store(failoverKey(rf), master)
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

	// A SENTINEL RESET makes Sentinel forget the replicas until the master
	// tells it again. A stopping master may not do that before it exits, and
	// then the failover finds no replica to promote (NOGOODSLAVE).
	stopping, err := r.masterPodStopping(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to check whether the master is stopping",
		}
		return err
	}
	if stopping {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: masterPodStoppingMsg,
		}
		return nil
	}

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

	port := getRedisPort(rf.Spec.Redis.Port)
	// master can be stale after applyRedisCustomConfig and UpdateRedisesPods.
	// Read it again only when a Sentinel needs a fix, and only one time.
	sentinelMonitorMaster := master
	masterRefreshed := false
	for _, sip := range sentinels {
		err = r.rfChecker.CheckSentinelMonitor(sip, sentinelMonitorMaster, port)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
		if heal.skip(sip, err) {
			continue
		}
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
				masterRefreshed = true
				if freshMaster != sentinelMonitorMaster {
					sentinelMonitorMaster = freshMaster
					// The Sentinel can already monitor the new master. A new
					// monitor then only resets its state.
					err = r.rfChecker.CheckSentinelMonitor(sip, sentinelMonitorMaster, port)
					setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
					if err == nil || heal.skip(sip, err) {
						continue
					}
				}
			}
			if err := r.rfHealer.NewSentinelMonitor(sip, sentinelMonitorMaster, rf); err != nil {
				if heal.skip(sip, err) {
					continue
				}
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State: redisfailoverv1.NotHealthyState,
				}
				return err
			}
			heal.resets[sip] = r.now()
		}
	}
	return r.checkAndHealSentinels(rf, sentinels, heal)
}

// logWrongMaster logs the result of CheckAllSlavesFromMaster with the pod of
// the replica as a field.
func (r *RedisFailoverHandler) logWrongMaster(rf *redisfailoverv1.RedisFailover, err error) {
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	var wrong *rfservice.WrongMasterError
	if errors.As(err, &wrong) {
		logger = logger.WithField("pod", wrong.Pod)
	}
	logger.Warningf("Slave not associated to master: %s", err.Error())
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
	stopping, err := r.masterPodStopping(rf)
	if err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to check whether the master is stopping",
		}
		return false, err
	}
	if stopping {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: masterStoppingMsg,
		}
		return false, nil
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
		msg := "Sentinel cannot fail over and the promotion of a replica failed"
		if errors.Is(err, rfservice.ErrPartialReconciliation) {
			msg = "failover incomplete: replica reconfiguration failed"
		}
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: msg,
		}
		return false, err
	}
	// The checks below make the Sentinels monitor the new master. The status
	// stays NotHealthy until the next check finds them healthy.
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: fmt.Sprintf("Sentinel knew no replica to fail over to, the operator promoted %s", best.PodName),
	}
	return true, nil
}

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
	if !allRunning && !r.rfChecker.IsRedisRunningQuorum(rf) {
		errorMsg := "redis quorum not running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.REDIS_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Redis quorum not running, waiting for redis statefulset reconcile")
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
		// A master pod in deletion is not counted, but it can still accept
		// writes. Wait until it stops, so that a promoted replica does not
		// lose them.
		stopping, err := r.masterPodStopping(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to check whether the master is stopping",
			}
			return err
		}
		if stopping {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: masterStoppingMsg,
			}
			return nil
		}
		// A master pod that does not answer gets failoverTimeout to recover.
		// Without a master pod, a wait only makes the outage longer.
		pod, err := r.unreachableMasterPod(rf)
		if err != nil {
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: masterPodLookupFailed,
			}
			return err
		}
		if pod != nil {
			if wait, err := r.waitForFailover(rf, pod); err != nil || wait {
				return err
			}
		}
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NO_MASTER, metrics.NOT_APPLICABLE, errors.New("no masters detected"))
		r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("No master available, operator will elect one")

		bestReplica, err := r.rfChecker.GetBestReplicaForPromotion(rf)
		if err != nil {
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
		r.clearMasterUnreachable(rf)
		return nil

	case 1:
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.NUMBER_OF_MASTERS, metrics.NOT_APPLICABLE, nil)

		healthy, masterIP, err := r.rfChecker.CheckMasterHealth(rf)
		if err != nil {
			r.recordMasterNotAnswering(rf, err)
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to check master health",
			}
			return err
		}

		if !healthy {
			var pod *corev1.Pod
			if masterIP == "" {
				// The master counted above can be in deletion at this time.
				// Then it is not found, but it can still accept writes, as in
				// case 0.
				var stopping bool
				stopping, err = r.masterPodStopping(rf)
				if err != nil {
					rf.Status = redisfailoverv1.RedisFailoverStatus{
						State:   redisfailoverv1.NotHealthyState,
						Message: "unable to check whether the master is stopping",
					}
					return err
				}
				if stopping {
					rf.Status = redisfailoverv1.RedisFailoverStatus{
						State:   redisfailoverv1.NotHealthyState,
						Message: masterStoppingMsg,
					}
					return nil
				}
				// The second count does not count a master pod that is not
				// ready. As in case 0, that pod gets failoverTimeout to recover.
				pod, err = r.unreachableMasterPod(rf)
			} else {
				pod, err = r.findRedisPod(rf, func(pod *corev1.Pod) bool { return pod.Status.PodIP == masterIP })
			}
			if err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: masterPodLookupFailed,
				}
				return err
			}
			if pod != nil {
				if wait, err := r.waitForFailover(rf, pod); err != nil || wait {
					return err
				}
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
			r.clearMasterUnreachable(rf)
			return nil
		}
		r.clearMasterUnreachable(rf)

		master = masterIP

		err = r.rfChecker.CheckAllSlavesFromMaster(masterIP, rf)
		setRedisCheckerMetrics(r.mClient, "redis", rf.Namespace, rf.Name, metrics.SLAVE_WRONG_MASTER, metrics.NOT_APPLICABLE, err)
		if err != nil {
			r.logWrongMaster(rf, err)
			if err = r.rfHealer.SetMasterOnAll(masterIP, rf); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "failed to configure slaves",
				}
				return err
			}
		}

	default:
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

	if rf.AutoscalingEnabled() {
		r.autoscaleMasters.Store(failoverKey(rf), master)
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
				Message: "unable to update redis pods",
			}
			return err
		}
	}

	return nil
}

func (r *RedisFailoverHandler) checkAndHealBootstrapMode(rf *redisfailoverv1.RedisFailover, allRunning bool) error {

	if !allRunning {
		errorMsg := "not all replicas running"
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: errorMsg,
		}
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
			setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_REPLICA_MISMATCH, metrics.NOT_APPLICABLE, errors.New(errorMsg))
			r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Debugf("Number of sentinel mismatch, waiting for sentinel deployment reconcile")
			return nil
		}

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
		for _, sip := range sentinels {
			err = r.rfChecker.CheckSentinelMonitor(sip, bootstrapSettings.Host, bootstrapSettings.Port)
			setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_WRONG_MASTER, sip, err)
			if heal.skip(sip, err) {
				continue
			}
			if err != nil {
				r.logger.WithField("redisfailover", rf.ObjectMeta.Name).WithField("namespace", rf.ObjectMeta.Namespace).Warningf("Fixing sentinel not monitoring expected master: %s", err.Error())
				if err := r.rfHealer.NewSentinelMonitorWithPort(sip, bootstrapSettings.Host, bootstrapSettings.Port, rf); err != nil {
					if heal.skip(sip, err) {
						continue
					}
					rf.Status = redisfailoverv1.RedisFailoverStatus{
						State:   redisfailoverv1.NotHealthyState,
						Message: "unable to check sentinel monitor",
					}
					return err
				}
				heal.resets[sip] = r.now()
			}
		}
		return r.checkAndHealSentinels(rf, sentinels, heal)
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
			// Skip a pod on a lost node, so that the reachable pods and the
			// other heal steps still run. Other errors, for example a bad config
			// value or a wrong password, stop the reconcile.
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
		r.maxMemoryLogged.Delete(failoverKey(rf))
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
		rf.Status.Message = result.Message
	}
	r.logMaxMemoryOutcome(logger, failoverKey(rf), result)
	return result.HoldRollout, nil
}

// maxMemoryOutcome is the part of a MaxMemoryResult that the handler logs.
type maxMemoryOutcome struct {
	message string
	hold    bool
}

// logMaxMemoryOutcome logs an outcome at Warning when it differs from the last
// one that was logged for the RedisFailover, and at Debug otherwise. The status
// message shows an unchanged outcome to the user.
func (r *RedisFailoverHandler) logMaxMemoryOutcome(logger log.Logger, key string, result rfservice.MaxMemoryResult) {
	if result.Message == "" && !result.HoldRollout {
		r.maxMemoryLogged.Delete(key)
		return
	}
	outcome := maxMemoryOutcome{message: result.Message, hold: result.HoldRollout}
	last, loaded := r.maxMemoryLogged.Swap(key, outcome)
	logf := logger.Warningf
	if loaded && last == outcome {
		logf = logger.Debugf
	}
	if result.Message != "" {
		logf("%s", result.Message)
	}
	if result.HoldRollout {
		logf("Holding the pod rollout until maxmemory fits the lowered memory limit")
	}
}

// errSentinelSkipped marks the later checks of a Sentinel that did not answer
// earlier in the reconcile.
var errSentinelSkipped = errors.New("sentinel unreachable, check skipped")

// sentinelHeal is the state of the Sentinel heal steps in one reconcile.
type sentinelHeal struct {
	key    string
	logger log.Logger
	// resets holds the last SENTINEL RESET or SENTINEL MONITOR of each
	// Sentinel in the hold-off.
	resets      map[string]time.Time
	unreachable map[string]bool
}

// startSentinelHeal loads the reset times that are still in the hold-off.
func (r *RedisFailoverHandler) startSentinelHeal(rf *redisfailoverv1.RedisFailover) *sentinelHeal {
	heal := &sentinelHeal{
		key:         failoverKey(rf),
		logger:      r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace),
		resets:      map[string]time.Time{},
		unreachable: map[string]bool{},
	}
	v, _ := r.sentinelResets.Load(heal.key)
	if last, ok := v.(map[string]time.Time); ok {
		now := r.now()
		for sip, at := range last {
			if now.Sub(at) < sentinelResetHoldOff {
				heal.resets[sip] = at
			}
		}
	}
	return heal
}

// saveSentinelResets keeps the reset times of the listed Sentinels only, so
// the entry of the old IP of a recreated pod does not stay.
func (r *RedisFailoverHandler) saveSentinelResets(heal *sentinelHeal, sentinels []string) {
	maps.DeleteFunc(heal.resets, func(sip string, _ time.Time) bool {
		return !slices.Contains(sentinels, sip)
	})
	if len(heal.resets) == 0 {
		r.sentinelResets.Delete(heal.key)
		return
	}
	r.sentinelResets.Store(heal.key, heal.resets)
}

// skip reports whether err shows that the Sentinel does not answer, and
// records it. A Sentinel pod can stop after GetSentinelsIPs listed it.
func (h *sentinelHeal) skip(sip string, err error) bool {
	if !redis.IsUnreachableError(err) {
		return false
	}
	h.logger.Warningf("Skipping unreachable sentinel %s: %s", sip, err.Error())
	h.unreachable[sip] = true
	return true
}

// skipped reports whether the Sentinel did not answer earlier in the
// reconcile. It records the check as unhealthy, as a failed check does.
func (r *RedisFailoverHandler) skipped(rf *redisfailoverv1.RedisFailover, heal *sentinelHeal, sip, property string) bool {
	if !heal.unreachable[sip] {
		return false
	}
	setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, property, sip, errSentinelSkipped)
	return true
}

// checkAndHealSentinels heals each Sentinel that answers. An unreachable
// Sentinel makes the RedisFailover NotHealthy after the heal of the others.
func (r *RedisFailoverHandler) checkAndHealSentinels(rf *redisfailoverv1.RedisFailover, sentinels []string, heal *sentinelHeal) error {
	for _, sip := range sentinels {
		if r.skipped(rf, heal, sip, metrics.SENTINEL_NUMBER_IN_MEMORY_MISMATCH) {
			continue
		}
		err := r.rfChecker.CheckSentinelNumberInMemory(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.SENTINEL_NUMBER_IN_MEMORY_MISMATCH, sip, err)
		if heal.skip(sip, err) {
			continue
		}
		if err != nil {
			if err := r.restoreSentinel(sip, "sentinels", heal); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to restore sentinel",
				}
				return err
			}
		}
	}
	for _, sip := range sentinels {
		if r.skipped(rf, heal, sip, metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH) {
			continue
		}
		err := r.rfChecker.CheckSentinelSlavesNumberInMemory(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH, sip, err)
		if heal.skip(sip, err) {
			continue
		}
		if err != nil {
			if err := r.restoreSentinel(sip, "expected slaves", heal); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to restore sentinel",
				}
				return err
			}
		}
	}
	for _, sip := range sentinels {
		if r.skipped(rf, heal, sip, metrics.APPLY_SENTINEL_CONFIG) {
			continue
		}
		err := r.rfHealer.SetSentinelCustomConfig(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, metrics.APPLY_SENTINEL_CONFIG, sip, err)
		if err != nil {
			if heal.skip(sip, err) {
				continue
			}
			rf.Status = redisfailoverv1.RedisFailoverStatus{
				State:   redisfailoverv1.NotHealthyState,
				Message: "unable to set sentinel custom config",
			}
			return err
		}
	}
	var unreachable []string
	for _, sip := range sentinels {
		if heal.unreachable[sip] {
			unreachable = append(unreachable, cmp.Or(sip, "(no IP)"))
		}
	}
	if len(unreachable) > 0 {
		// A Sentinel that does not answer cannot vote in a failover.
		msg := fmt.Sprintf("sentinel(s) %s unreachable", strings.Join(unreachable, ", "))
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: msg,
		}
		return errors.New(msg)
	}
	return nil
}

// sentinelResetHoldOff is shorter than the default resync interval of 30
// seconds, so a mismatch that stays gets a reset at the next resync.
const sentinelResetHoldOff = 25 * time.Second

// restoreSentinel resets a Sentinel with a mismatch, unless it had a reset or
// a new monitor in the hold-off. After a reset, the Sentinel needs some seconds
// to find the other Sentinels and the replicas, and a new reset makes this
// time longer.
func (r *RedisFailoverHandler) restoreSentinel(sip, mismatch string, heal *sentinelHeal) error {
	now := r.now()
	if at, ok := heal.resets[sip]; ok {
		heal.logger.Debugf("Sentinel %s mismatch number of %s in memory. last reset %s ago, waiting", sip, mismatch, now.Sub(at).Round(time.Second))
		// Without a pod event, the next resync can come long after the hold-off.
		if r.requeue != nil {
			r.requeue(heal.key, sentinelResetHoldOff-now.Sub(at))
		}
		return nil
	}
	heal.logger.Warningf("Sentinel %s mismatch number of %s in memory. resetting", sip, mismatch)
	if err := r.rfHealer.RestoreSentinel(sip); err != nil {
		if heal.skip(sip, err) {
			return nil
		}
		return err
	}
	heal.resets[sip] = now
	return nil
}

func getRedisPort(p int32) string {
	return strconv.Itoa(int(p))
}

func setRedisCheckerMetrics(metricsClient metrics.Recorder, mode /* redis or sentinel? */, rfNamespace, rfName, property, IP string, err error) {
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

// updateStatus is the only status write of a reconcile. The checks set only
// State and Message, so LastChanged comes from the observed status unless the
// state changed. An unchanged status is not written, which saves one API call
// on each resync and pod event.
func updateStatus(k8sservice k8s.Services, rf *redisfailoverv1.RedisFailover, observed redisfailoverv1.RedisFailoverStatus) {
	if observed.State != rf.Status.State {
		rf.Status.LastChanged = time.Now().Format(time.RFC3339)
	} else {
		rf.Status.LastChanged = observed.LastChanged
	}
	if rf.Status == observed {
		return
	}
	k8sservice.UpdateRedisFailoverStatus(context.Background(), rf.Namespace, rf, metav1.PatchOptions{})
}
