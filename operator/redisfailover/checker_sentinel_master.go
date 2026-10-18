package redisfailover

import (
	"slices"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
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
	// limit is the bound, fixed at the start of the wait.
	limit time.Duration
	// ended is true after the log of the end of the wait.
	ended bool
	// seen is true after the check of the wait in the current reconcile.
	seen bool
}

// startSentinelMasterRound marks the wait of key as not seen, and returns the
// function that drops it at the end of a reconcile that did not check it. A
// wait counts only reconciles in a row: after a reconcile that returned
// early, for example on an error, the same pair of IPs gets a new bound. The
// controller runs one reconcile of a key at a time.
func (r *RedisFailoverHandler) startSentinelMasterRound(key string) func() {
	if v, ok := r.sentinelMasterWaits.Load(key); ok {
		wait, _ := v.(sentinelMasterWait)
		wait.seen = false
		r.sentinelMasterWaits.Store(key, wait)
	}
	return func() {
		if v, ok := r.sentinelMasterWaits.Load(key); ok {
			if wait, _ := v.(sentinelMasterWait); !wait.seen {
				r.sentinelMasterWaits.Delete(key)
			}
		}
	}
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
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).WithField("pod", pod)
	if started {
		wait = sentinelMasterWait{named: named, master: master, since: now, limit: r.sentinelMasterLimit(rf, logger)}
	}
	wait.seen = true
	limit := wait.limit
	elapsed := now.Sub(wait.since)
	if elapsed >= limit {
		if !wait.ended {
			logger.Warningf("The Sentinels give pod %s (%s) as the master for %s, but the Redis master is %s, so the operator heals the roles and the Sentinels", pod, named, elapsed.Round(time.Second), master)
			wait.ended = true
		}
		r.sentinelMasterWaits.Store(key, wait)
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

// sentinelMasterLimit returns the bound of a wait that starts. The running
// Sentinels keep their old timeouts until the heal applies the customConfig,
// and the wait stops that heal. Thus the bound is the larger of the
// customConfig window and the longest window of the Sentinels that answer.
// It stays the same for the whole wait.
func (r *RedisFailoverHandler) sentinelMasterLimit(rf *redisfailoverv1.RedisFailover, logger log.Logger) time.Duration {
	limit := rfservice.SentinelFailoverWindow(rf)
	sentinels, err := r.rfChecker.GetSentinelsIPs(rf)
	if err != nil {
		logger.Debugf("The operator did not read the Sentinels, so the wait uses the customConfig timeouts: %v", err)
		return limit
	}
	for _, sip := range sentinels {
		f, err := r.rfChecker.GetSentinelFailover(sip)
		if err != nil || f.DownAfter == 0 || f.Timeout == 0 {
			logger.Debugf("Sentinel %s did not give its timeouts, so the wait does not use them: %v", sip, err)
			continue
		}
		limit = max(limit, f.DownAfter+f.Timeout)
	}
	return limit
}

// sentinelsNameOtherPod returns the IP and the name of the Redis pod that the
// Sentinels give as the master, when that IP is not master. All the Sentinels
// that answer must give the same IP, and they must be a quorum. Otherwise no
// Sentinel quorum supports that pod, and a wait only delays the heal.
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
