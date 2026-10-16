package redisfailover

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/redis"
)

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
	// failoverRead is true after the first read of the failover state of
	// the Sentinels in this reconcile.
	failoverRead bool
	// failoverHold is true while a Sentinel fails over.
	failoverHold bool
	// readErrors holds the time of the first read error of the failover
	// state of each Sentinel.
	readErrors map[string]time.Time
	// held holds the Sentinels that get no change in this reconcile: after a
	// read error, or in a failover that the stored wait does not cover.
	held map[string]bool
}

// startSentinelHeal loads the reset times that are still in the hold-off.
func (r *RedisFailoverHandler) startSentinelHeal(rf *redisfailoverv1.RedisFailover) *sentinelHeal {
	heal := &sentinelHeal{
		key:         failoverKey(rf),
		logger:      r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace),
		resets:      map[string]time.Time{},
		unreachable: map[string]bool{},
		readErrors:  map[string]time.Time{},
		held:        map[string]bool{},
	}
	if v, ok := r.sentinelReadErrors.Load(heal.key); ok {
		if last, ok := v.(map[string]time.Time); ok {
			maps.Copy(heal.readErrors, last)
		}
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
	// Without a read, no Sentinel needed a change, so a failover that a
	// later reconcile finds is a new one.
	if !heal.failoverRead {
		r.sentinelFailovers.Delete(heal.key)
		clear(heal.readErrors)
	}
	maps.DeleteFunc(heal.readErrors, func(sip string, _ time.Time) bool {
		return !slices.Contains(sentinels, sip)
	})
	if len(heal.readErrors) == 0 {
		r.sentinelReadErrors.Delete(heal.key)
	} else {
		r.sentinelReadErrors.Store(heal.key, heal.readErrors)
	}
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
	if err := r.restoreSentinelMismatches(rf, sentinels, heal, metrics.SENTINEL_NUMBER_IN_MEMORY_MISMATCH, "sentinels", r.rfChecker.CheckSentinelNumberInMemory); err != nil {
		return err
	}
	if err := r.restoreSentinelMismatches(rf, sentinels, heal, metrics.REDIS_SLAVES_NUMBER_IN_MEMORY_MISMATCH, "expected slaves", r.rfChecker.CheckSentinelSlavesNumberInMemory); err != nil {
		return err
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

// restoreSentinelMismatches runs check on each Sentinel that answers, and
// resets each Sentinel with a mismatch. property is the metric of the check,
// and mismatch names the count in the log.
func (r *RedisFailoverHandler) restoreSentinelMismatches(rf *redisfailoverv1.RedisFailover, sentinels []string, heal *sentinelHeal, property, mismatch string, check func(string, *redisfailoverv1.RedisFailover) error) error {
	for _, sip := range sentinels {
		if r.skipped(rf, heal, sip, property) {
			continue
		}
		err := check(sip, rf)
		setRedisCheckerMetrics(r.mClient, "sentinel", rf.Namespace, rf.Name, property, sip, err)
		if heal.skip(sip, err) {
			continue
		}
		if err != nil {
			if err := r.restoreSentinel(rf, sentinels, sip, mismatch, heal); err != nil {
				rf.Status = redisfailoverv1.RedisFailoverStatus{
					State:   redisfailoverv1.NotHealthyState,
					Message: "unable to restore sentinel",
				}
				return err
			}
		}
	}
	return nil
}

// sentinelFailoverMsg is the status message while a Sentinel fails over.
const sentinelFailoverMsg = "waiting for the Sentinel failover"

// sentinelFailoverStates are the values of failover-state in the order of a
// failover.
var sentinelFailoverStates = []string{"wait_start", "select_slave", "send_slaveof_noone", "wait_promotion", "reconf_slaves", "update_config"}

// sentinelFailover is the failover that the operator waits for.
type sentinelFailover struct {
	sentinel string
	master   string
	// state is the index in sentinelFailoverStates, or -1.
	state int
	since time.Time
}

// sentinelChangeAllowed reports whether the operator can send SENTINEL
// MONITOR or RESET to the Sentinel sip. These commands end a failover in that
// Sentinel. The Sentinels can then return to the old master and fail over a
// second time, and the promoted replica loses its writes. The first call in a
// reconcile reads all the Sentinels, because only a Sentinel that fails over
// has the flag. Each call then reads sip again just before the change, because
// a failover can start after an earlier read.
func (r *RedisFailoverHandler) sentinelChangeAllowed(rf *redisfailoverv1.RedisFailover, sentinels []string, heal *sentinelHeal, sip string) bool {
	if !heal.failoverRead {
		heal.failoverRead = true
		r.readSentinelFailovers(rf, sentinels, true, heal)
	}
	if !heal.failoverHold && !heal.unreachable[sip] && !heal.held[sip] {
		r.readSentinelFailovers(rf, []string{sip}, false, heal)
	}
	return !heal.failoverHold && !heal.unreachable[sip] && !heal.held[sip]
}

// readSentinelFailovers reads the failover state of the Sentinels and sets
// failoverHold. all is true when sentinels holds each Sentinel. The hold ends
// after the failover-timeout of the Sentinel.
func (r *RedisFailoverHandler) readSentinelFailovers(rf *redisfailoverv1.RedisFailover, sentinels []string, all bool, heal *sentinelHeal) {
	found, timeout := r.findSentinelFailover(rf, sentinels, heal)
	if found == nil {
		if all {
			r.sentinelFailovers.Delete(heal.key)
		}
		return
	}
	if timeout == 0 {
		timeout = redisfailoverv1.DefaultSentinelFailoverTimeoutMilliseconds * time.Millisecond
	}
	now := r.now()
	// A failover of another Sentinel or of another master, or an earlier
	// failover-state, is a new failover with its own wait. The master of a
	// Sentinel changes only at the end of a failover.
	v, ok := r.sentinelFailovers.Load(heal.key)
	last, _ := v.(sentinelFailover)
	newFailover := !ok || !found.continues(last)
	if newFailover && !all {
		// Only a read of all the Sentinels finds the leader and starts a
		// wait. Until then, the target alone gets no change, so a candidate
		// cannot make the wait of a leader longer.
		heal.logger.Infof("Sentinel %s fails over, so the operator does not change it in this reconcile", found.sentinel)
		heal.held[found.sentinel] = true
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: sentinelFailoverMsg,
		}
		if r.requeue != nil {
			r.requeue(heal.key, timeout)
		}
		return
	}
	switch {
	case newFailover:
		found.since = now
	case all:
		found.since = last.since
	default:
		*found = last
	}
	r.sentinelFailovers.Store(heal.key, *found)
	elapsed := now.Sub(found.since)
	if elapsed >= timeout {
		heal.logger.Warningf("Sentinel %s fails over for %s, longer than its failover-timeout %s, so the operator heals the Sentinels", found.sentinel, elapsed.Round(time.Second), timeout)
		return
	}
	logf := heal.logger.Debugf
	if newFailover {
		logf = heal.logger.Infof
	}
	logf("Sentinel %s fails over, so the operator sends no SENTINEL MONITOR or RESET until the failover ends", found.sentinel)
	heal.failoverHold = true
	rf.Status = redisfailoverv1.RedisFailoverStatus{
		State:   redisfailoverv1.NotHealthyState,
		Message: sentinelFailoverMsg,
	}
	if r.requeue != nil {
		r.requeue(heal.key, timeout-elapsed)
	}
}

// continues reports whether f is the failover last at the same or a later
// failover-state.
func (f sentinelFailover) continues(last sentinelFailover) bool {
	return last.sentinel == f.sentinel && last.master == f.master && f.state >= last.state
}

// findSentinelFailover returns the failover of the leader among the
// Sentinels, or nil, and the longest failover-timeout of the Sentinels that
// fail over.
func (r *RedisFailoverHandler) findSentinelFailover(rf *redisfailoverv1.RedisFailover, sentinels []string, heal *sentinelHeal) (*sentinelFailover, time.Duration) {
	var found *sentinelFailover
	var timeout time.Duration
	for _, sip := range sentinels {
		if heal.unreachable[sip] {
			continue
		}
		// skip records a Sentinel that does not answer, so it gets no change.
		f, err := r.rfChecker.GetSentinelFailover(sip)
		if heal.skip(sip, err) {
			continue
		}
		if err != nil && !redis.IsNoSuchMasterError(err) {
			r.holdOnReadError(rf, heal, sip, err)
			continue
		}
		delete(heal.readErrors, sip)
		// A Sentinel without the master has no failover.
		if err != nil || !f.InProgress {
			continue
		}
		timeout = max(timeout, f.Timeout)
		// Each candidate of the election has the flag. The leader has the
		// highest state.
		state := slices.Index(sentinelFailoverStates, f.State)
		if found == nil || state > found.state {
			found = &sentinelFailover{sentinel: sip, master: f.Master, state: state}
		}
	}
	return found, timeout
}

// holdOnReadError holds the changes of a Sentinel that gives an error other
// than a missing master, for example an ACL error. That Sentinel can fail
// over. The hold ends after the default failover-timeout from the first error,
// so it is never permanent.
func (r *RedisFailoverHandler) holdOnReadError(rf *redisfailoverv1.RedisFailover, heal *sentinelHeal, sip string, err error) {
	now := r.now()
	first, ok := heal.readErrors[sip]
	if !ok {
		first = now
		heal.readErrors[sip] = now
	}
	timeout := redisfailoverv1.DefaultSentinelFailoverTimeoutMilliseconds * time.Millisecond
	if elapsed := now.Sub(first); elapsed < timeout {
		if !ok {
			heal.logger.Warningf("Sentinel %s did not give its failover state, so the operator does not change it for %s: %s", sip, timeout, err.Error())
		}
		heal.held[sip] = true
		rf.Status = redisfailoverv1.RedisFailoverStatus{
			State:   redisfailoverv1.NotHealthyState,
			Message: fmt.Sprintf("waiting to read the failover state of sentinel %s", sip),
		}
		if r.requeue != nil {
			r.requeue(heal.key, timeout-elapsed)
		}
		return
	}
	heal.logger.Warningf("Sentinel %s did not give its failover state for %s, so the operator heals it: %s", sip, timeout, err.Error())
}

// sentinelResetHoldOff is shorter than the default resync interval of 30
// seconds, so a mismatch that stays gets a reset at the next resync.
const sentinelResetHoldOff = 25 * time.Second

// restoreSentinel resets a Sentinel with a mismatch, unless it had a reset or
// a new monitor in the hold-off. After a reset, the Sentinel needs some seconds
// to find the other Sentinels and the replicas, and a new reset makes this
// time longer.
func (r *RedisFailoverHandler) restoreSentinel(rf *redisfailoverv1.RedisFailover, sentinels []string, sip, mismatch string, heal *sentinelHeal) error {
	now := r.now()
	if at, ok := heal.resets[sip]; ok {
		heal.logger.Debugf("Sentinel %s mismatch number of %s in memory. last reset %s ago, waiting", sip, mismatch, now.Sub(at).Round(time.Second))
		// Without a pod event, the next resync can come long after the hold-off.
		if r.requeue != nil {
			r.requeue(heal.key, sentinelResetHoldOff-now.Sub(at))
		}
		return nil
	}
	if !r.sentinelChangeAllowed(rf, sentinels, heal, sip) {
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
