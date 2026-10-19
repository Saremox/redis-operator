package redisfailover

import (
	"net"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// sentinelMasterMsg is the status message while the Sentinels give another
// address as the master.
const sentinelMasterMsg = "waiting for the Sentinels to agree on the master"

// sentinelMasterRecheck is the interval of the checks in the wait. A Sentinel
// that switches to a new master gives no pod event.
const sentinelMasterRecheck = time.Second

// sentinelMasterWait is a wait of the master and replica heal: the Sentinels
// give named as the master, and master is the Redis master.
type sentinelMasterWait struct {
	named  string
	master string
	since  time.Time
	// downAfter and failoverTimeout are the largest values of the
	// customConfig at the start of the wait and of the Sentinels in known.
	downAfter, failoverTimeout time.Duration
	// known holds the UIDs of the Sentinel pods that gave their timeouts in
	// this wait. A new pod can have the IP of an old one.
	known map[types.UID]bool
	// ended is true after the log of the end of the wait.
	ended bool
	// seen is true after the check of the wait in the current reconcile.
	seen bool
	// elected is true when the operator promoted master at since. The
	// election needs Sentinels that know no replica pod and monitor no pod,
	// so they cannot fail over the address that they give.
	elected bool
}

// newSentinelMasterWait fixes the customConfig part of the bound at the
// start. A later customConfig reaches the running Sentinels only after the
// wait.
func newSentinelMasterWait(rf *redisfailoverv1.RedisFailover, named, master string, now time.Time) sentinelMasterWait {
	downAfter, failoverTimeout := rfservice.SentinelTimeouts(rf)
	return sentinelMasterWait{named: named, master: master, since: now, downAfter: downAfter, failoverTimeout: failoverTimeout, known: map[types.UID]bool{}, seen: true}
}

// recordElection keeps the promoted master, so that the checks until the
// bound do not wait for the Sentinels that give the old address.
func (r *RedisFailoverHandler) recordElection(rf *redisfailoverv1.RedisFailover, master string) {
	wait := newSentinelMasterWait(rf, "", master, r.now())
	wait.elected = true
	r.sentinelMasterWaits.Store(failoverKey(rf), wait)
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
// because a quorum of the Sentinels gives another address as the master. A
// Sentinel leader can promote a replica and stop before it tells the other
// Sentinels. These Sentinels then fail over the old master and select another
// replica. A master label or replicas on the first promoted replica then give
// it writes that the second failover loses. The wait ends after the
// down-after-milliseconds and the failover-timeout of the Sentinels. Until
// that bound after an election by the operator, the elected master gets no
// wait.
func (r *RedisFailoverHandler) waitForSentinelMaster(rf *redisfailoverv1.RedisFailover, master string) (bool, error) {
	key := failoverKey(rf)
	named, err := r.sentinelsNameOtherMaster(rf, master)
	if err != nil {
		return false, err
	}
	if named == "" {
		r.sentinelMasterWaits.Delete(key)
		return false, nil
	}
	now := r.now()
	v, _ := r.sentinelMasterWaits.Load(key)
	wait, ok := v.(sentinelMasterWait)
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	if ok && wait.elected && wait.master == master {
		wait.seen = true
		r.readSentinelTimeouts(rf, &wait, logger)
		if now.Sub(wait.since) < wait.downAfter+wait.failoverTimeout {
			r.sentinelMasterWaits.Store(key, wait)
			logger.Debugf("The operator elected the master %s, so it does not wait for the Sentinels that give %s", master, named)
			return false, nil
		}
		// The bound counts from the election, so the wait ends at once.
		wait.named, wait.elected = named, false
	}
	started := !ok || wait.named != named || wait.master != master
	if started {
		wait = newSentinelMasterWait(rf, named, master, now)
	}
	wait.seen = true
	if !wait.ended {
		r.readSentinelTimeouts(rf, &wait, logger)
	}
	limit := wait.downAfter + wait.failoverTimeout
	elapsed := now.Sub(wait.since)
	if elapsed >= limit {
		if !wait.ended {
			logger.Warningf("The Sentinels give %s as the master for %s, but the Redis master is %s, so the operator heals the roles and the Sentinels", named, elapsed.Round(time.Second), master)
			wait.ended = true
		}
		r.sentinelMasterWaits.Store(key, wait)
		return false, nil
	}
	r.sentinelMasterWaits.Store(key, wait)
	logf := logger.Debugf
	if started {
		logf = logger.Infof
	}
	logf("The Sentinels give %s as the master, but the Redis master is %s, so the operator does not change the roles for up to %s", named, master, limit)
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: sentinelMasterMsg,
	}
	if r.requeue != nil {
		r.requeue(key, min(sentinelMasterRecheck, limit-elapsed))
	}
	return true, nil
}

// readSentinelTimeouts adds the timeouts of each running Sentinel that did
// not give them yet in this wait. The running Sentinels keep their old
// timeouts until the heal applies the customConfig, and the wait stops that
// heal. One Sentinel can flag the master down late and another can lead the
// failover, so the bound is the largest down-after-milliseconds plus the
// largest failover-timeout. A new value can only make the bound longer. A
// Sentinel that never answers stays unknown, and the wait ends at the bound
// of the known values. The values of a pod that is gone stay in the bound.
func (r *RedisFailoverHandler) readSentinelTimeouts(rf *redisfailoverv1.RedisFailover, wait *sentinelMasterWait, logger log.Logger) {
	pods, err := r.k8sservice.GetDeploymentPods(rf.Namespace, rfservice.GetSentinelName(rf))
	if err != nil {
		logger.Debugf("The operator did not read the Sentinels, so the wait uses the known timeouts: %v", err)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		sip := pod.Status.PodIP
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || wait.known[pod.UID] {
			continue
		}
		f, err := r.rfChecker.GetSentinelFailover(sip)
		if err != nil {
			logger.Debugf("Sentinel %s did not give its timeouts, so the operator reads them again in the next check: %v", sip, err)
			continue
		}
		if f.DownAfter == 0 || f.Timeout == 0 {
			logger.Debugf("Sentinel %s did not give its timeouts, so the operator reads them again in the next check", sip)
			continue
		}
		wait.known[pod.UID] = true
		wait.downAfter = max(wait.downAfter, f.DownAfter)
		wait.failoverTimeout = max(wait.failoverTimeout, f.Timeout)
	}
}

// sentinelsNameOtherMaster returns an address that a quorum of the Sentinels
// can give as the master, when it is not master. Such a quorum can fail over
// the old master later, also when other Sentinels give the promoted replica,
// and also when the old master pod is gone. A Sentinel without an answer can
// be one more vote for an address that another Sentinel gives. Without a
// possible quorum for another address, a wait only delays the heal.
func (r *RedisFailoverHandler) sentinelsNameOtherMaster(rf *redisfailoverv1.RedisFailover, master string) (string, error) {
	ips, err := r.rfChecker.GetSentinelMasterIPs(rf)
	if err != nil {
		return "", err
	}
	votes := map[string]int{}
	unknown := 0
	for _, ip := range ips {
		switch {
		case ip == "":
			unknown++
		case ip == master:
		case credibleMaster(ip):
			votes[ip]++
		}
	}
	// The quorum of the Sentinel monitor.
	quorum := int(rf.Spec.Sentinel.Replicas/2 + 1)
	var named []string
	for ip, n := range votes {
		if n+unknown >= quorum {
			named = append(named, ip)
		}
	}
	if len(named) == 0 {
		return "", nil
	}
	// More Sentinels than the spec run during a rollout, so two addresses can
	// have a quorum.
	return slices.Min(named), nil
}

// credibleMaster reports whether a Sentinel can fail over the master at ip. A
// new Sentinel gives 127.0.0.1, and no Redis listens on a loopback or an
// unspecified address.
func credibleMaster(ip string) bool {
	addr := net.ParseIP(ip)
	return addr != nil && !addr.IsLoopback() && !addr.IsUnspecified()
}
