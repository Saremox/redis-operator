package redisfailover

import (
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

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

// stoppingMasterStatus reports whether the master pod stops, and then sets
// the status message msg.
func (r *RedisFailoverHandler) stoppingMasterStatus(rf *redisfailoverv1.RedisFailover, msg string) (bool, error) {
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
			Message: msg,
		}
	}
	return stopping, nil
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

// waitForMasterPod reports whether the caller must return: the lookup of the
// master pod failed, or the master pod still has time to answer.
func (r *RedisFailoverHandler) waitForMasterPod(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod, lookupErr error) (bool, error) {
	if lookupErr != nil {
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: masterPodLookupFailed,
		}
		return true, lookupErr
	}
	if pod == nil {
		return false, nil
	}
	return r.waitForFailover(rf, pod)
}

// setPromotionFailedStatus sets the status after a failed promotion. A
// partial reconciliation has its own message.
func setPromotionFailedStatus(rf *redisfailoverv1.RedisFailover, err error, msg string) {
	if errors.Is(err, rfservice.ErrPartialReconciliation) {
		msg = "failover incomplete: replica reconfiguration failed"
	}
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: msg,
	}
}
