package redisfailover

import (
	"slices"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// sentinelMasterMsg is the status message while the Sentinels give another
// pod as the master.
const sentinelMasterMsg = "waiting for the Sentinels to agree on the master"

// sentinelMasterRecheck is the interval of the checks in the wait. A Sentinel
// that switches to a new master gives no pod event.
const sentinelMasterRecheck = time.Second

// sentinelMasterWait is a wait of the master and replica heal: the Sentinels
// give the pod at named as the master, and master is the Redis master.
type sentinelMasterWait struct {
	named  string
	master string
	since  time.Time
	// ended is true after the log of the end of the wait.
	ended bool
}

// waitForSentinelMaster reports whether the master and replica heal waits,
// because a quorum of the Sentinels gives another pod as the master. A
// Sentinel leader can promote a replica and stop before it tells the other
// Sentinels. These Sentinels then fail over the old master and select another
// replica. A master label or replicas on the first promoted replica then give
// it writes that the second failover loses. The wait ends after the
// down-after-milliseconds and the failover-timeout of the Sentinels.
func (r *RedisFailoverHandler) waitForSentinelMaster(rf *redisfailoverv1.RedisFailover, master string) bool {
	key := failoverKey(rf)
	named, pod := r.sentinelsNameOtherPod(rf, master)
	if pod == "" {
		r.sentinelMasterWaits.Delete(key)
		return false
	}
	now := r.now()
	v, _ := r.sentinelMasterWaits.Load(key)
	wait, ok := v.(sentinelMasterWait)
	started := !ok || wait.named != named || wait.master != master
	if started {
		wait = sentinelMasterWait{named: named, master: master, since: now}
	}
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).WithField("pod", pod)
	limit := rfservice.SentinelFailoverWindow(rf)
	elapsed := now.Sub(wait.since)
	if elapsed >= limit {
		if !wait.ended {
			logger.Warningf("The Sentinels give pod %s (%s) as the master for %s, but the Redis master is %s, so the operator heals the roles and the Sentinels", pod, named, elapsed.Round(time.Second), master)
			wait.ended = true
			r.sentinelMasterWaits.Store(key, wait)
		}
		return false
	}
	r.sentinelMasterWaits.Store(key, wait)
	logf := logger.Debugf
	if started {
		logf = logger.Infof
	}
	logf("The Sentinels give pod %s (%s) as the master, but the Redis master is %s, so the operator does not change the roles for up to %s", pod, named, master, limit)
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: sentinelMasterMsg,
	}
	if r.requeue != nil {
		r.requeue(key, min(sentinelMasterRecheck, limit-elapsed))
	}
	return true
}

// sentinelsNameOtherPod returns the IP and the name of the Redis pod that the
// Sentinels give as the master, when that IP is not master. All the Sentinels
// that answer must give the same IP, and they must be a quorum. Otherwise the
// Sentinels cannot end a failover together, and a wait only delays the heal.
func (r *RedisFailoverHandler) sentinelsNameOtherPod(rf *redisfailoverv1.RedisFailover, master string) (string, string) {
	ips, err := r.rfChecker.GetSentinelMasterIPs(rf)
	// The quorum of the Sentinel monitor.
	if err != nil || len(ips) < int(rf.Spec.Sentinel.Replicas/2+1) {
		return "", ""
	}
	named := ips[0]
	if named == "" || named == master || slices.ContainsFunc(ips, func(ip string) bool { return ip != named }) {
		return "", ""
	}
	// A pod in deletion still counts: its Redis can still be the master of
	// the Sentinels.
	pods, err := r.k8sservice.GetStatefulSetPods(rf.Namespace, rfservice.GetRedisName(rf))
	if err != nil {
		return "", ""
	}
	for i := range pods.Items {
		if pods.Items[i].Status.PodIP == named {
			return named, pods.Items[i].Name
		}
	}
	return "", ""
}
