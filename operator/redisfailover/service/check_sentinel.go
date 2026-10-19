package service

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/service/redis"
)

var errInsufficientSentinels = errors.New("insufficient sentinels to reach the quorum")

// CheckSentinelNumberInMemory returns an error when the Sentinel does not
// know the expected number of Sentinels.
func (r *RedisFailoverChecker) CheckSentinelNumberInMemory(sentinel string, rf *redisfailoverv1.RedisFailover) error {
	nSentinels, err := r.redisClient.GetNumberSentinelsInMemory(sentinel)
	if err != nil {
		return err
	}
	if nSentinels < rf.Spec.Sentinel.Replicas {
		// A Sentinel does not know a pod that does not run, for example a
		// Pending pod, and a reset does not change that. After a reset, the
		// Sentinels forget each other for some seconds, and a failover
		// cannot start in that time.
		running, err := r.GetSentinelsIPs(rf)
		if err != nil {
			return err
		}
		if int(nSentinels) >= len(running) {
			return nil
		}
	}
	if nSentinels != rf.Spec.Sentinel.Replicas {
		return errors.New("sentinels in memory mismatch")
	}
	return nil
}

// CheckSentinelsCannotFailover reports whether no Sentinel can fail over, so
// that a promotion by the operator cannot give two masters. docs/logic.md
// gives the conditions.
func (r *RedisFailoverChecker) CheckSentinelsCannotFailover(rf *redisfailoverv1.RedisFailover) (bool, error) {
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return false, err
	}
	// A pod in deletion or not running still counts. Its Redis can still
	// answer the Sentinel.
	pods := make(map[string]string, len(rps.Items))
	for _, rp := range rps.Items {
		if rp.Status.PodIP != "" {
			pods[rp.Status.PodIP] = rp.Name
		}
	}
	sentinels, err := r.GetSentinelsIPs(rf)
	if err != nil {
		return false, err
	}
	if len(sentinels) < int(rf.Spec.Sentinel.Replicas) {
		logger.Warningf("%d of %d Sentinels run, all are necessary to be sure that Sentinel cannot fail over", len(sentinels), rf.Spec.Sentinel.Replicas)
		return false, nil
	}
	for _, sip := range sentinels {
		if r.sentinelCanFailover(logger, sip, pods) {
			return false, nil
		}
	}
	return true, nil
}

// sentinelCanFailover reports whether the Sentinel at sip can fail over to a
// pod in pods, a map of pod IP to pod name. A Sentinel that does not answer
// can.
func (r *RedisFailoverChecker) sentinelCanFailover(logger log.Logger, sip string, pods map[string]string) bool {
	master, _, err := r.redisClient.GetSentinelMonitor(sip)
	if err != nil {
		logger.Warningf("Sentinel %s did not give its master, so it can know a replica: %v", sip, err)
		return true
	}
	if pod, ok := pods[master]; ok {
		logger.WithField("pod", pod).Infof("Sentinel %s monitors the pod %s (%s), so Sentinel can fail over", sip, pod, master)
		return true
	}
	// 127.0.0.1 is the address in the configuration of a new Sentinel.
	if master != "127.0.0.1" {
		down, err := r.redisClient.SentinelMasterDown(sip)
		if err != nil {
			logger.Warningf("Sentinel %s did not give the state of its master: %v", sip, err)
			return true
		}
		if !down {
			logger.Infof("Sentinel %s does not flag its master %s as down, so the master can still answer", sip, master)
			return true
		}
	}
	replicas, err := r.redisClient.GetSentinelReplicas(sip)
	if err != nil {
		logger.Warningf("Sentinel %s did not give its replicas, so it can know one: %v", sip, err)
		return true
	}
	for _, replica := range replicas {
		if pod, ok := pods[replica]; ok {
			logger.WithField("pod", pod).Infof("Sentinel %s knows the replica pod %s (%s), so Sentinel can fail over", sip, pod, replica)
			return true
		}
	}
	logger.Warningf("Sentinel %s monitors %s, which is not a pod, and knows no replica pod of %d replicas", sip, master, len(replicas))
	return false
}

// CheckSentinelQuorum returns an error when too few Sentinels run or pass
// SENTINEL CKQUORUM to fail over. The int is the count of unhealthy Sentinels.
func (r *RedisFailoverChecker) CheckSentinelQuorum(rFailover *redisfailoverv1.RedisFailover) (int, error) {

	// The caller logs the returned error with the unhealthy count.
	var unhealthyCnt = -1

	sentinels, err := r.GetSentinelsIPs(rFailover)
	if err != nil {
		return unhealthyCnt, err
	}
	if len(sentinels) < int(getQuorum(rFailover)) {
		unhealthyCnt = int(getQuorum(rFailover)) - len(sentinels)
		return unhealthyCnt, errInsufficientSentinels
	}

	unhealthyCnt = 0
	for _, sip := range sentinels {
		err = r.redisClient.SentinelCheckQuorum(sip)
		if err != nil {
			unhealthyCnt += 1
		} else {
			continue
		}
	}
	if unhealthyCnt < int(getQuorum(rFailover)) {
		return unhealthyCnt, nil
	} else {
		return unhealthyCnt, errInsufficientSentinels
	}
}

// CheckSentinelSlavesNumberInMemory returns an error when the Sentinel does
// not know the expected number of replicas.
func (r *RedisFailoverChecker) CheckSentinelSlavesNumberInMemory(sentinel string, rf *redisfailoverv1.RedisFailover) error {
	nSlaves, err := r.redisClient.GetNumberSentinelSlavesInMemory(sentinel)
	if err != nil {
		return err
	}
	// While bootstrapping, all the Redis pods are slaves of the bootstrap node.
	masters := 1
	if rf.Bootstrapping() {
		masters = 0
	}
	expected := rf.Spec.Redis.Replicas - int32(masters)
	if nSlaves < expected {
		// CheckSentinelNumberInMemory gives the reason to not reset for a
		// pod that does not run.
		running, err := r.GetRedisesIPs(rf)
		if err != nil {
			return err
		}
		if int(nSlaves) >= len(running)-masters {
			return nil
		}
	}
	if nSlaves != expected {
		return errors.New("redis slaves in sentinel memory mismatch")
	}
	return nil
}

// CheckSentinelSlavesNumberQuorumInMemory returns an error when the Sentinel
// knows less than a quorum (majority) of the expected replicas. The rollout
// checks it before it replaces the master. One replica can stay unavailable,
// for example with a PVC in a lost zone. Then a check for all the replicas
// blocks the rollout for all time.
func (r *RedisFailoverChecker) CheckSentinelSlavesNumberQuorumInMemory(sentinel string, rf *redisfailoverv1.RedisFailover) error {
	nSlaves, err := r.redisClient.GetNumberSentinelSlavesInMemory(sentinel)
	if err != nil {
		return err
	}
	expected := rf.Spec.Redis.Replicas - 1
	var quorum int32
	if expected > 0 {
		quorum = expected/2 + 1
	}
	if nSlaves < quorum {
		return fmt.Errorf("redis slaves in sentinel memory below quorum: have %d, need at least %d of %d expected", nSlaves, quorum, expected)
	}
	return nil

}

// CheckSentinelMonitor controls if the sentinels are monitoring the expected master.
func (r *RedisFailoverChecker) CheckSentinelMonitor(sentinel string, monitor ...string) error {
	monitorIP := monitor[0]
	monitorPort := ""
	if len(monitor) > 1 {
		monitorPort = monitor[1]
	}
	actualMonitorIP, actualMonitorPort, err := r.redisClient.GetSentinelMonitor(sentinel)
	if err != nil {
		return err
	}
	if actualMonitorIP != monitorIP || (monitorPort != "" && monitorPort != actualMonitorPort) {
		return fmt.Errorf("sentinel monitoring %s:%s instead %s:%s", actualMonitorIP, actualMonitorPort, monitorIP, monitorPort)
	}
	return nil
}

// GetSentinelFailover returns the failover state of the Sentinel.
func (r *RedisFailoverChecker) GetSentinelFailover(sentinel string) (redis.SentinelFailover, error) {
	return r.redisClient.GetSentinelFailover(sentinel)
}

// GetSentinelMasterIPs returns the master IP that each running Sentinel gives
// to the clients. A Sentinel in deletion counts, because it can still vote
// until it stops. A Sentinel that gives an error has an empty entry, because
// it can still have a master.
func (r *RedisFailoverChecker) GetSentinelMasterIPs(rf *redisfailoverv1.RedisFailover) ([]string, error) {
	pods, err := r.k8sService.GetDeploymentPods(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return nil, err
	}
	ips := make([]string, 0, len(pods.Items))
	for _, sp := range pods.Items {
		if sp.Status.Phase != corev1.PodRunning {
			continue
		}
		ip, err := r.redisClient.GetSentinelMasterIP(sp.Status.PodIP)
		if err != nil {
			ip = ""
		}
		ips = append(ips, ip)
	}
	return ips, nil
}
