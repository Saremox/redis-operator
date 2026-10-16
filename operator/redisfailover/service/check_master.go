package service

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/k8s"
)

// ErrAmbiguousMasterCount is the error of GetMasterIP for zero masters and
// for more than one master. GetNumberMasters tells the two cases apart, so
// that a caller does not promote a replica during a split-brain.
var ErrAmbiguousMasterCount = errors.New("number of redis nodes known as master is different than 1")

// ErrRedisNotAnswering tells that no pod answered as master and a ready pod
// did not answer, so that pod can still be the master.
var ErrRedisNotAnswering = errors.New("ready redis pod did not answer")

// ReplicaInfo holds information about a Redis replica for failover decisions.
type ReplicaInfo struct {
	IP                string
	PodName           string
	ReplicationOffset int64
	// Synced is true when `master_link_status` is `up` and no sync is in progress.
	Synced   bool
	PodReady bool
}

// CheckIfMasterLocalhost reports whether each Running Redis pod has 127.0.0.1
// as master. This is the state after the first start of all the pods. It
// returns an error when it finds no pod IP, when a pod does not answer, or
// when a pod is a master.
func (r *RedisFailoverChecker) CheckIfMasterLocalhost(rFailover *redisfailoverv1.RedisFailover) (bool, error) {
	logger := r.logger.WithField("redisfailover", rFailover.Name).WithField("namespace", rFailover.Namespace)

	var lhmaster = 0
	redisIps, err := r.GetRedisesIPs(rFailover)
	if err != nil {
		return false, err
	}
	if len(redisIps) == 0 {
		return false, errors.New("unable to fetch any redis Ips Currently")
	}
	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		return false, err
	}
	rport := getRedisPort(rFailover.Spec.Redis.Port)
	for _, sip := range redisIps {
		master, err := r.redisClient.GetSlaveOf(sip, rport, password)
		if err != nil {
			return false, fmt.Errorf("redis %s: %w", sip, err)
		} else if master == "" {
			return false, fmt.Errorf("unexpected master state: redis %s is a master", sip)
		} else {
			if master == "127.0.0.1" {
				lhmaster++
			}
		}
	}
	if lhmaster == len(redisIps) {
		logger.Infof("all available redis configured localhost as master , operator must heal")
		return true, nil
	}
	logger.Infof("atleast one pod does not have localhost as master , operator should not heal")
	return false, nil
}

// GetMasterIP connects to all redis and returns the master of the redis failover.
func (r *RedisFailoverChecker) GetMasterIP(rf *redisfailoverv1.RedisFailover) (string, error) {
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return "", err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return "", err
	}

	var masters []string
	rport := getRedisPort(rf.Spec.Redis.Port)
	for i := range rps.Items {
		rp := &rps.Items[i]
		if rp.Status.Phase != corev1.PodRunning || rp.DeletionTimestamp != nil {
			continue
		}
		master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password)
		if err != nil {
			podErrorLogf(r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace), rp)("Get redis info failed, maybe this node is not ready, pod ip: %s: %v", rp.Status.PodIP, err)
			continue
		}
		if master {
			masters = append(masters, rp.Status.PodIP)
		}
	}

	if len(masters) != 1 {
		return "", ErrAmbiguousMasterCount
	}
	return masters[0], nil
}

// GetNumberMasters counts the Running, non-terminating redis pods that answer
// as master. If no pod answers as master and a ready pod does not answer, it
// returns an error and not zero, because that pod can still be the master.
func (r *RedisFailoverChecker) GetNumberMasters(rf *redisfailoverv1.RedisFailover) (int, error) {
	nMasters := 0
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return nMasters, err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return nMasters, err
	}

	var unanswered error
	rport := getRedisPort(rf.Spec.Redis.Port)
	for i := range rps.Items {
		rp := &rps.Items[i]
		if rp.Status.Phase != corev1.PodRunning || rp.DeletionTimestamp != nil {
			continue
		}
		master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password)
		if err != nil {
			podErrorLogf(r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace), rp)("Get redis info failed, maybe this node is not ready, pod ip: %s: %v", rp.Status.PodIP, err)
			if unanswered == nil && util.PodIsReady(rp) {
				unanswered = fmt.Errorf("%w: %s: %w", ErrRedisNotAnswering, rp.Name, err)
			}
			continue
		}
		if master {
			nMasters++
		}
	}
	if nMasters == 0 && unanswered != nil {
		return nMasters, unanswered
	}
	return nMasters, nil
}

// GetRedisesSlavesPods returns pods names of the Redis secondary nodes.
func (r *RedisFailoverChecker) GetRedisesSlavesPods(rf *redisfailoverv1.RedisFailover) ([]string, error) {
	redises := []string{}
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return nil, err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return redises, err
	}

	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running
			master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password)
			if err != nil {
				return []string{}, err
			}
			if !master {
				redises = append(redises, rp.Name)
			}
		}
	}
	return redises, nil
}

// GetRedisesMasterPod returns the name of the redis pod that answers as master.
func (r *RedisFailoverChecker) GetRedisesMasterPod(rFailover *redisfailoverv1.RedisFailover) (string, error) {
	rps, err := r.k8sService.GetStatefulSetPods(rFailover.Namespace, GetRedisName(rFailover))
	if err != nil {
		return "", err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		return "", err
	}

	rport := getRedisPort(rFailover.Spec.Redis.Port)
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running
			master, err := r.redisClient.IsMaster(rp.Status.PodIP, rport, password)
			if err != nil {
				return "", err
			}
			if master {
				return rp.Name, nil
			}
		}
	}
	return "", errors.New("redis nodes known as master not found")
}

// CheckRedisSlavesReady reports whether the replica is connected to its
// master and has no sync in progress.
func (r *RedisFailoverChecker) CheckRedisSlavesReady(ip string, rFailover *redisfailoverv1.RedisFailover) (bool, error) {
	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		return false, err
	}

	port := getRedisPort(rFailover.Spec.Redis.Port)
	return r.redisClient.SlaveIsReady(ip, port, password)
}

// CheckMasterHealth checks if the current master is healthy and reachable.
// Returns (healthy, masterIP, error).
func (r *RedisFailoverChecker) CheckMasterHealth(rf *redisfailoverv1.RedisFailover) (bool, string, error) {
	masterIP, err := r.GetMasterIP(rf)
	if err != nil {
		if !errors.Is(err, ErrAmbiguousMasterCount) {
			return false, "", err
		}
		// The caller promotes a replica at once when no master is found. A
		// master that stalls for a moment is also not found, so a second
		// count must confirm that there is no master.
		n, nErr := r.GetNumberMasters(rf)
		if nErr != nil {
			return false, "", nErr
		}
		if n > 1 {
			return false, "", fmt.Errorf("split-brain detected: %d redis nodes claim to be master, refusing to promote another replica", n)
		}
		if n == 1 {
			return false, "", errors.New("the master did not answer every check, checking again on the next reconcile")
		}
		return false, "", nil
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return false, masterIP, err
	}

	port := getRedisPort(rf.Spec.Redis.Port)

	// IsMaster reads the role from INFO replication.
	isMaster, err := r.redisClient.IsMaster(masterIP, port, password)
	if err != nil {
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).WithField("ip", masterIP).Warnf("Master health check failed: %v", err)
		return false, masterIP, nil
	}

	return isMaster, masterIP, nil
}

// GetBestReplicaForPromotion prefers a synced replica, then the highest
// replication offset, to lose the fewest writes. Pod readiness decides only
// between equal offsets, because the master Service sends traffic only to
// Ready pods.
func (r *RedisFailoverChecker) GetBestReplicaForPromotion(rf *redisfailoverv1.RedisFailover) (*ReplicaInfo, error) {
	replicas, err := r.GetReplicaReplicationOffsets(rf)
	if err != nil {
		return nil, err
	}

	if len(replicas) == 0 {
		return nil, errors.New("no replicas available for promotion")
	}

	candidates := make([]string, 0, len(replicas))
	best := &replicas[0]
	for i := range replicas {
		replica := &replicas[i]
		candidates = append(candidates, fmt.Sprintf("%s (pod: %s, pod ready: %t, synced: %t, offset: %d)",
			replica.IP, replica.PodName, replica.PodReady, replica.Synced, replica.ReplicationOffset))
		if betterPromotionCandidate(replica, best) {
			best = replica
		}
	}
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	logger.Infof("Promotion candidates: %s", strings.Join(candidates, ", "))

	logger.WithField("pod", best.PodName).Infof("Selected replica %s (pod: %s, pod ready: %t, offset: %d) for promotion", best.IP, best.PodName, best.PodReady, best.ReplicationOffset)
	return best, nil
}

// betterPromotionCandidate reports whether a ranks strictly above b.
func betterPromotionCandidate(a, b *ReplicaInfo) bool {
	if a.Synced != b.Synced {
		return a.Synced
	}
	if a.ReplicationOffset != b.ReplicationOffset {
		return a.ReplicationOffset > b.ReplicationOffset
	}
	return a.PodReady && !b.PodReady
}

// GetReplicaReplicationOffsets returns replication offset information for all replicas.
func (r *RedisFailoverChecker) GetReplicaReplicationOffsets(rf *redisfailoverv1.RedisFailover) ([]ReplicaInfo, error) {
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return nil, err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return nil, err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	var replicas []ReplicaInfo

	for _, rp := range rps.Items {
		if rp.Status.Phase != corev1.PodRunning || rp.DeletionTimestamp != nil {
			continue
		}

		replInfo, err := r.redisClient.GetReplicationInfo(rp.Status.PodIP, port, password)
		if err != nil {
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).WithField("pod", rp.Name).WithField("ip", rp.Status.PodIP).Warnf("Failed to get replication info: %v", err)
			continue
		}

		// Skip masters
		if replInfo.Role == "master" {
			continue
		}
		// A master in a FAILOVER is a replica already, but it refuses
		// REPLICAOF NO ONE until the failover ends.
		if replInfo.FailoverState != "" && replInfo.FailoverState != "no-failover" {
			continue
		}

		replicas = append(replicas, ReplicaInfo{
			IP:                rp.Status.PodIP,
			PodName:           rp.Name,
			ReplicationOffset: replInfo.SlaveReplOffset,
			Synced:            !replInfo.SyncInProgress && replInfo.MasterLinkStatus == "up",
			PodReady:          util.PodIsReady(&rp),
		})
	}

	return replicas, nil
}
