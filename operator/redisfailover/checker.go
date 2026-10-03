package redisfailover

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/saremox/redis-operator/service/k8s"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/redis"
)

// UpdateRedisesPods if the running version of pods is equal to the statefulset one
func (r *RedisFailoverHandler) UpdateRedisesPods(rf *redisfailoverv1.RedisFailover) (err error) {
	var wait *rolloutWait
	defer func() {
		if err == nil {
			r.reportRolloutWait(rf, wait)
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

	// Update stale pods with a slave role
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
			if wait, err = r.redisPodsSettled(rf, ssUR); err != nil || wait != nil {
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
			// failover. In operator-managed mode (sentinel.enabled: false), no
			// Sentinel Deployment exists, so GetSentinelsIPs would fail with a
			// 404. After this delete, the next reconcile finds no master, and
			// the "no master" branch of checkAndHealOperatorManagedMode elects
			// a replica.
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
	if v, ok := r.rolloutWaits.Load(key); ok && v.(rolloutWait).uid == wait.uid {
		wait.since = v.(rolloutWait).since
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
		if pod.Annotations[masterUnreachableAnnotation] == "" {
			continue
		}
		if err := r.k8sservice.UpdatePodAnnotations(rf.Namespace, pod.Name, map[string]string{masterUnreachableAnnotation: ""}); err != nil {
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
	msg := fmt.Sprintf("master unreachable for %s, failing over after %s", unreachable.Truncate(time.Second), timeout)
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
	redis    string
	sentinel string
}

func passwordKey(rf *redisfailoverv1.RedisFailover) string {
	return rf.Namespace + "/" + rf.Name
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
	if known && state.redis == password && state.sentinel == password {
		return nil
	}

	if !known || state.redis != password {
		previous := password
		if known {
			previous = state.redis
		}
		complete, err := r.rfHealer.ApplyPassword(rf, password, previous)
		if err != nil {
			return err
		}
		// A pod yet to start keeps the old password in play. With none known,
		// the one every running pod accepts is the best there is.
		if complete || !known {
			state.redis = password
		}
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
	r.passwords.Store(key, state)
	return nil
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

	// Every check below authenticates, so a changed password goes first.
	if err := r.applyPassword(rf); err != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: "unable to apply the configured password",
		}
		return err
	}

	if rf.Bootstrapping() {
		return r.checkAndHealBootstrapMode(rf)
	}

	// Route to operator-managed mode when Sentinel is disabled
	if rf.OperatorManagedFailover() {
		return r.checkAndHealOperatorManagedMode(rf)
	}

	// From here on, sentinel-managed mode checks and heals, in this order:
	//   - a quorum of Redis pods and of Sentinel pods,
	//   - exactly one Redis master, with every slave replicating from it,
	//   - the custom Redis config and maxmemory,
	//   - the Redis pod rollout,
	//   - the master that each Sentinel monitors,
	//   - the Sentinel and slave counts in each Sentinel, and the custom
	//     Sentinel config.
	// A quorum is a majority, not all the pods in the RF spec. The comment
	// below on IsRedisRunningQuorum gives the reason.

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
		// A stopping master is not counted but can still accept writes, and its
		// shutdown script asks Sentinel to fail over. An election now can lose writes.
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
		// The master can be a ready pod that does not answer. Its wait starts
		// at this first missed check. Errors are ignored, because this
		// reconcile fails already.
		if errors.Is(err, rfservice.ErrRedisNotAnswering) {
			if pod, _ := r.findRedisPod(rf, rfservice.IsMasterPod); pod != nil {
				_, _ = r.masterUnreachableSince(rf, pod)
			}
		}
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
		r.clearMasterUnreachable(rf)
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
			// The master counted above may have started stopping since; then
			// it is no longer found but may still take writes, as in case 0.
			if masterIP == "" {
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
			}
			// Only a master that was found gets the wait. A master that was
			// not found can be gone, and a wait only makes the outage longer.
			if masterIP != "" {
				pod, err := r.findRedisPod(rf, func(pod *corev1.Pod) bool { return pod.Status.PodIP == masterIP })
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
			}
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
			r.clearMasterUnreachable(rf)
			return nil
		}
		r.clearMasterUnreachable(rf)

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
			return nil
		}
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
