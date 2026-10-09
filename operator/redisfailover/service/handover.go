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
	// HandoverAborted: the old master stays the master, because Redis ended the
	// failover, for example when the target did not reach the offset in time.
	HandoverAborted
	// HandoverRefused: the old master did not start the failover. The target
	// is not an online replica at the address and port of the pod, or an ACL
	// denies FAILOVER.
	HandoverRefused
	// HandoverInProgress: the catch-up ended, but the role change did not end
	// in the wait. Redis has no time limit for that step. The old master is a
	// replica already and accepts no writes. It leaves that state only when the
	// target accepts or rejects the role. The next reconcile sees no master or
	// the new master, and the checks of those cases continue.
	HandoverInProgress
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
	// the PSYNC FAILOVER handshake. Redis has no time limit for that step.
	handoverWaitMargin = 3 * time.Second
	// handoverPollInterval is the interval of the INFO replication reads.
	handoverPollInterval = 100 * time.Millisecond
)

// HandOverMaster moves the master role from masterIP to targetIP with FAILOVER.
// The master pauses the writes until the target has its whole replication
// stream, so no acknowledged write is lost. Without FORCE, a timeout aborts
// the failover and the master continues. A failover that runs already, for
// example after an operator restart, is not started again.
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
	if info.FailoverState == noFailover {
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
		if info.FailoverState == noFailover || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(handoverPollInterval)
	}
	switch {
	case info.FailoverState != noFailover:
		return HandoverInProgress, nil
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
