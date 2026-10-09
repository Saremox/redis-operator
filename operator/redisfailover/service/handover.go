package service

import (
	"fmt"
	"strings"
	"time"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/k8s"
)

// HandoverResult is the outcome of HandOverMaster.
type HandoverResult int

const (
	// HandoverDone: the target is the master, and the old master is its replica.
	HandoverDone HandoverResult = iota
	// HandoverAborted: the old master stays the master. Redis ended the
	// failover, because the target did not catch up in time, or the operator
	// ended it, because the role change took too long or the target stopped.
	HandoverAborted
	// HandoverRefused: the old master did not start the failover. The target
	// is not an online replica at the address and port of the pod, or an ACL
	// denies FAILOVER.
	HandoverRefused
	// HandoverUnsupported: the old master has no FAILOVER command. It is
	// before Redis 6.2, or a rename-command disables the command.
	HandoverUnsupported
)

const noFailover = "no-failover"

// HandoverCatchUpLimit is the TIMEOUT of FAILOVER: the longest catch-up, in
// which the master pauses the writes. A synced replica acknowledges its offset
// each second, so the catch-up usually ends in 1s.
const HandoverCatchUpLimit = 2 * time.Second

var (
	// handoverWritePause is HandoverCatchUpLimit. The tests make it shorter.
	handoverWritePause = HandoverCatchUpLimit
	// handoverWaitMargin is the time after the catch-up for the role change,
	// the PSYNC FAILOVER handshake. Redis has no time limit for that step. It
	// usually takes milliseconds, and the margin keeps the reconcile short.
	handoverWaitMargin = time.Second
	// handoverPollInterval is the interval of the INFO replication reads.
	handoverPollInterval = 100 * time.Millisecond
)

// HandOverMaster moves the master role from masterIP to targetIP with FAILOVER.
// The master pauses the writes until the target has its whole replication
// stream, so in the normal case no acknowledged write is lost. Without FORCE,
// a timeout aborts the failover and the master continues. A failover that
// runs already, for example after an operator restart, is not started again.
// A role change that does not end in the wait is aborted, and the old master
// continues.
func (r *RedisFailoverHealer) HandOverMaster(masterIP, targetIP string, rf *redisfailoverv1.RedisFailover) (HandoverResult, error) {
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return 0, err
	}
	port := getRedisPort(rf.Spec.Redis.Port)

	info, err := r.redisClient.GetReplicationInfo(masterIP, port, password)
	if err != nil {
		return 0, err
	}
	if info.FailoverState == "" {
		return HandoverUnsupported, nil
	}
	// A target that restarts in place loads its RDB file with the replication
	// ID of the master. It can then take the role with older data, so a target
	// that stops or restarts ends the failover at once.
	targetRunID := ""
	if info.FailoverState == noFailover {
		if targetRunID, err = r.redisClient.GetRunID(targetIP, port, password); err != nil {
			return 0, err
		}
		err = r.redisClient.FailoverTo(masterIP, port, password, targetIP, handoverWritePause)
		switch {
		case err == nil:
		case strings.HasPrefix(err.Error(), "ERR unknown command"):
			return HandoverUnsupported, nil
		case strings.HasPrefix(err.Error(), "ERR FAILOVER"), strings.HasPrefix(err.Error(), "NOPERM"):
			logger.Warningf("FAILOVER to %s refused: %v", targetIP, err)
			return HandoverRefused, nil
		default:
			return 0, err
		}
	}

	deadline := time.Now().Add(handoverWritePause + handoverWaitMargin)
	for {
		info, err = r.redisClient.GetReplicationInfo(masterIP, port, password)
		if err != nil {
			return 0, err
		}
		if info.FailoverState == noFailover || !time.Now().Before(deadline) || r.targetRestarted(targetIP, port, password, &targetRunID) {
			break
		}
		time.Sleep(handoverPollInterval)
	}
	if info.FailoverState != noFailover {
		// The abort keeps the old master with all its data. A target that took
		// the role at the same time has no client writes, and the check of
		// multiple masters makes it a replica again.
		err = r.redisClient.FailoverAbort(masterIP, port, password)
		switch {
		case err == nil:
			logger.Warningf("FAILOVER to %s aborted: it did not end in the wait, or the target stopped", targetIP)
		// The role change ended just before the abort.
		case !strings.HasPrefix(err.Error(), "ERR No failover in progress"):
			return 0, err
		}
		if info, err = r.redisClient.GetReplicationInfo(masterIP, port, password); err != nil {
			return 0, err
		}
		if info.FailoverState != noFailover {
			return 0, fmt.Errorf("FAILOVER on %s still runs after FAILOVER ABORT", masterIP)
		}
	}
	switch {
	case info.Role == "master":
		return HandoverAborted, nil
	case info.MasterHost != targetIP:
		return 0, fmt.Errorf("after FAILOVER, %s is a replica of %s and not of %s", masterIP, info.MasterHost, targetIP)
	}
	isMaster, err := r.redisClient.IsMaster(targetIP, port, password)
	if err != nil {
		return 0, err
	}
	if !isMaster {
		return 0, fmt.Errorf("after FAILOVER, %s is not a master", targetIP)
	}
	return HandoverDone, nil
}

// targetRestarted reports whether the target does not answer, or answers with
// a run ID other than runID. An empty runID takes the first answer. A short
// network error also ends the failover, and the next attempt comes after the
// retry wait.
func (r *RedisFailoverHealer) targetRestarted(targetIP, port, password string, runID *string) bool {
	current, err := r.redisClient.GetRunID(targetIP, port, password)
	if err != nil {
		return true
	}
	if *runID == "" {
		*runID = current
	}
	return current != *runID
}

// AbortOrphanedFailover ends a FAILOVER on the master at masterIP. It reports
// whether a failover ran. In operator-managed mode, only the rollout sends
// FAILOVER, and it waits for the end in the same reconcile. Thus a failover at
// the start of a reconcile has no watcher, for example after an operator
// restart. Redis has no time limit for its role change. A master that does not
// answer is not an error here, because the checks of the master handle it.
func (r *RedisFailoverHealer) AbortOrphanedFailover(masterIP string, rf *redisfailoverv1.RedisFailover) (bool, error) {
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return false, err
	}
	port := getRedisPort(rf.Spec.Redis.Port)
	info, err := r.redisClient.GetReplicationInfo(masterIP, port, password)
	if err != nil || info.FailoverState == "" || info.FailoverState == noFailover {
		return false, nil
	}
	err = r.redisClient.FailoverAbort(masterIP, port, password)
	switch {
	case err == nil:
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Warningf("aborted a FAILOVER on %s that no reconcile watched", masterIP)
	// The failover ended just before the abort. The roles changed, so the
	// caller also checks them again.
	case !strings.HasPrefix(err.Error(), "ERR No failover in progress"):
		return false, err
	}
	return true, nil
}
