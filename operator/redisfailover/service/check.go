package service

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

// ErrAmbiguousMasterCount is the error of GetMasterIP for zero masters and
// for more than one master. GetNumberMasters tells the two cases apart, so
// that a caller does not promote a replica during a split-brain.
var ErrAmbiguousMasterCount = errors.New("number of redis nodes known as master is different than 1")

// ErrRedisNotAnswering tells that no pod answered as master and a ready pod
// did not answer, so that pod can still be the master.
var ErrRedisNotAnswering = errors.New("ready redis pod did not answer")

// ReplicaInfo holds information about a Redis replica for failover decisions
type ReplicaInfo struct {
	IP                string
	PodName           string
	ReplicationOffset int64
	// Synced is true when `master_link_status` is `up` and no sync is in progress.
	Synced   bool
	PodReady bool
}

// RedisFailoverCheck defines the interface able to check the correct status of redis failover
type RedisFailoverCheck interface {
	CheckRedisNumber(rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelNumber(rFailover *redisfailoverv1.RedisFailover) error
	CheckAllSlavesFromMaster(master string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelNumberInMemory(sentinel string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelSlavesNumberInMemory(sentinel string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelSlavesNumberQuorumInMemory(sentinel string, rFailover *redisfailoverv1.RedisFailover) error
	CheckSentinelQuorum(rFailover *redisfailoverv1.RedisFailover) (int, error)
	CheckIfMasterLocalhost(rFailover *redisfailoverv1.RedisFailover) (bool, error)
	CheckSentinelsCannotFailover(rFailover *redisfailoverv1.RedisFailover) (bool, error)
	CheckSentinelMonitor(sentinel string, monitor ...string) error
	GetMasterIP(rFailover *redisfailoverv1.RedisFailover) (string, error)
	GetNumberMasters(rFailover *redisfailoverv1.RedisFailover) (int, error)
	GetRedisesIPs(rFailover *redisfailoverv1.RedisFailover) ([]string, error)
	GetSentinelsIPs(rFailover *redisfailoverv1.RedisFailover) ([]string, error)
	GetMaxRedisPodTime(rFailover *redisfailoverv1.RedisFailover) (time.Duration, error)
	GetRedisesSlavesPods(rFailover *redisfailoverv1.RedisFailover) ([]string, error)
	GetRedisesMasterPod(rFailover *redisfailoverv1.RedisFailover) (string, error)
	GetStatefulSetUpdateRevision(rFailover *redisfailoverv1.RedisFailover) (string, error)
	GetRedisRevisionHash(podName string, rFailover *redisfailoverv1.RedisFailover) (string, error)
	CheckRedisSlavesReady(slaveIP string, rFailover *redisfailoverv1.RedisFailover) (bool, error)
	IsRedisRunning(rFailover *redisfailoverv1.RedisFailover) bool
	IsRedisRunningQuorum(rFailover *redisfailoverv1.RedisFailover) bool
	IsSentinelRunning(rFailover *redisfailoverv1.RedisFailover) bool
	IsSentinelRunningQuorum(rFailover *redisfailoverv1.RedisFailover) bool
	IsClusterRunning(rFailover *redisfailoverv1.RedisFailover) bool
	// Operator-managed failover methods
	CheckMasterHealth(rFailover *redisfailoverv1.RedisFailover) (bool, string, error)
	GetBestReplicaForPromotion(rFailover *redisfailoverv1.RedisFailover) (*ReplicaInfo, error)
	GetReplicaReplicationOffsets(rFailover *redisfailoverv1.RedisFailover) ([]ReplicaInfo, error)
}

// RedisFailoverChecker implements RedisFailoverCheck.
type RedisFailoverChecker struct {
	k8sService    k8s.Services
	redisClient   redis.Client
	logger        log.Logger
	metricsClient metrics.Recorder
	opts          options
}

// NewRedisFailoverChecker creates an object of the RedisFailoverChecker struct
func NewRedisFailoverChecker(k8sService k8s.Services, redisClient redis.Client, logger log.Logger, metricsClient metrics.Recorder, opts ...Option) *RedisFailoverChecker {
	return &RedisFailoverChecker{
		k8sService:    k8sService,
		redisClient:   redisClient,
		logger:        logger,
		metricsClient: metricsClient,
		opts:          applyOptions(opts),
	}
}

// CheckRedisNumber returns an error when the StatefulSet replicas differ from
// redis.replicas.
func (r *RedisFailoverChecker) CheckRedisNumber(rf *redisfailoverv1.RedisFailover) error {
	ss, err := r.k8sService.GetStatefulSet(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	if rf.Spec.Redis.Replicas != *ss.Spec.Replicas {
		return errors.New("number of redis pods differ from specification")
	}
	return nil
}

// CheckSentinelNumber returns an error when the Deployment replicas differ
// from sentinel.replicas.
func (r *RedisFailoverChecker) CheckSentinelNumber(rf *redisfailoverv1.RedisFailover) error {
	d, err := r.k8sService.GetDeployment(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return err
	}
	if rf.Spec.Sentinel.Replicas != *d.Spec.Replicas {
		return errors.New("number of sentinel pods differ from specification")
	}
	return nil
}

// IsMasterPod reports whether the pod is labelled as the redis master.
func IsMasterPod(pod *corev1.Pod) bool {
	return pod.Labels[redisRoleLabelKey] == redisRoleLabelMaster
}

// applyMasterEvictionAnnotation keeps the cluster-autoscaler safe-to-evict
// annotation in sync with a pod's role, but only when the RedisFailover opts in
// via spec.redis.preventMasterEviction. The master is pinned (false) and slaves
// are marked evictable (true). It reads the desired state off the already-fetched
// pod object and skips the patch when the annotation is already correct, so it is
// safe to call on every reconcile without extra API writes.
func applyMasterEvictionAnnotation(k8sService k8s.Services, rf *redisfailoverv1.RedisFailover, pod corev1.Pod, isMaster bool) error {
	if !rf.Spec.Redis.PreventMasterEviction {
		return resetMasterEvictionAnnotation(k8sService, rf, pod)
	}
	desired := "true"
	if isMaster {
		desired = "false"
	}
	if pod.Annotations[masterSafeToEvictAnnotation] == desired {
		return nil
	}
	return k8sService.UpdatePodAnnotations(rf.Namespace, pod.Name, map[string]string{masterSafeToEvictAnnotation: desired})
}

// resetMasterEvictionAnnotation gives the pod the safe-to-evict value from
// spec.redis.podAnnotations, or removes the master pin "false". A pin that stays
// after the user turns preventMasterEviction off blocks the node drains of the
// cluster autoscaler. A "true" can come from a policy or the user, so it stays.
func resetMasterEvictionAnnotation(k8sService k8s.Services, rf *redisfailoverv1.RedisFailover, pod corev1.Pod) error {
	current, present := pod.Annotations[masterSafeToEvictAnnotation]
	if desired, ok := rf.Spec.Redis.PodAnnotations[masterSafeToEvictAnnotation]; ok {
		if present && current == desired {
			return nil
		}
		return k8sService.UpdatePodAnnotations(rf.Namespace, pod.Name, map[string]string{masterSafeToEvictAnnotation: desired})
	}
	if !present || current != "false" {
		return nil
	}
	return k8sService.RemovePodAnnotation(rf.Namespace, pod.Name, masterSafeToEvictAnnotation)
}

func (r *RedisFailoverChecker) setMasterLabelIfNecessary(rf *redisfailoverv1.RedisFailover, pod corev1.Pod) error {
	if err := applyMasterEvictionAnnotation(r.k8sService, rf, pod, true); err != nil {
		return err
	}
	for labelKey, labelValue := range pod.Labels {
		if labelKey == redisRoleLabelKey && labelValue == redisRoleLabelMaster {
			return nil
		}
	}
	return r.k8sService.UpdatePodLabels(rf.Namespace, pod.Name, generateRedisMasterRoleLabel())
}

func (r *RedisFailoverChecker) setSlaveLabelIfNecessary(rf *redisfailoverv1.RedisFailover, pod corev1.Pod, port, password string) error {
	return setSlaveLabel(r.k8sService, r.opts, rf, pod, port, password)
}

// CheckAllSlavesFromMaster returns an error when a reachable replica has a
// master other than master. It also sets the role label and the safe-to-evict
// annotation of each pod. When a pod loses the master label, it starts the
// disconnect of its clients.
func (r *RedisFailoverChecker) CheckAllSlavesFromMaster(master string, rf *redisfailoverv1.RedisFailover) error {
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	rport := getRedisPort(rf.Spec.Redis.Port)
	// The loop labels every pod before it returns wrongMasterErr. Otherwise, one
	// bad pod early in the list can stop the label of the new master. Then the
	// master Service has no endpoint.
	var wrongMasterErr error
	for _, rp := range rps.Items {
		if rp.Status.PodIP == master {
			err = r.setMasterLabelIfNecessary(rf, rp)
			if err != nil {
				return err
			}
		} else {
			err = r.setSlaveLabelIfNecessary(rf, rp, rport, password)
			if err != nil {
				return err
			}
		}

		if rp.DeletionTimestamp != nil {
			// A terminating pod is going away, and dialing its IP can block
			// for the whole connection timeout.
			continue
		}
		slave, err := r.redisClient.GetSlaveOf(rp.Status.PodIP, rport, password)
		if err != nil {
			// The pod does not answer, usually the old master on a lost node.
			// Skip it, so that the heal of the reachable pods continues.
			r.logger.Errorf("Get slave of master failed, maybe this node is not ready, pod ip: %s", rp.Status.PodIP)
			continue
		}
		if slave != "" && slave != master {
			newErr := fmt.Errorf("slave %s don't have the master %s, has %s", rp.Status.PodIP, master, slave)
			r.logger.Errorf("%v", newErr)
			if wrongMasterErr == nil {
				wrongMasterErr = newErr
			}
		}
	}
	return wrongMasterErr
}

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

// CheckIfMasterLocalhost reports whether each Running Redis pod has 127.0.0.1
// as master. This is the state after the first start of all the pods. It
// returns an error when it finds no pod IP, when a pod does not answer, or
// when a pod is a master.
func (r *RedisFailoverChecker) CheckIfMasterLocalhost(rFailover *redisfailoverv1.RedisFailover) (bool, error) {

	var lhmaster = 0
	redisIps, err := r.GetRedisesIPs(rFailover)
	if len(redisIps) == 0 || err != nil {
		r.logger.Warningf("CheckIfMasterLocalhost GetRedisesIPs Failed- unable to fetch any redis Ips Currently")
		return false, errors.New("unable to fetch any redis Ips Currently")
	}
	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		r.logger.Errorf("CheckIfMasterLocalhost -- GetRedisPassword Failed")
		return false, err
	}
	rport := getRedisPort(rFailover.Spec.Redis.Port)
	for _, sip := range redisIps {
		master, err := r.redisClient.GetSlaveOf(sip, rport, password)
		if err != nil {
			r.logger.Warningf("CheckIfMasterLocalhost -- GetSlaveOf Failed")
			return false, err
		} else if master == "" {
			r.logger.Warningf("CheckIfMasterLocalhost -- Master already available ?? check manually")
			return false, errors.New("unexpected master state, fix manually")
		} else {
			if master == "127.0.0.1" {
				lhmaster++
			}
		}
	}
	if lhmaster == len(redisIps) {
		r.logger.Infof("all available redis configured localhost as master , operator must heal")
		return true, nil
	}
	r.logger.Infof("atleast one pod does not have localhost as master , operator should not heal")
	return false, nil
}

// CheckSentinelsCannotFailover reports whether no Sentinel can fail over, so
// that a promotion by the operator cannot give two masters. docs/logic.md
// gives the conditions.
func (r *RedisFailoverChecker) CheckSentinelsCannotFailover(rf *redisfailoverv1.RedisFailover) (bool, error) {
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
		r.logger.Warningf("%d of %d Sentinels run, all are necessary to be sure that Sentinel cannot fail over", len(sentinels), rf.Spec.Sentinel.Replicas)
		return false, nil
	}
	for _, sip := range sentinels {
		master, _, err := r.redisClient.GetSentinelMonitor(sip)
		if err != nil {
			r.logger.Warningf("Sentinel %s did not give its master, so it can know a replica: %v", sip, err)
			return false, nil
		}
		if pod, ok := pods[master]; ok {
			r.logger.Infof("Sentinel %s monitors the pod %s (%s), so Sentinel can fail over", sip, pod, master)
			return false, nil
		}
		// 127.0.0.1 is the address in the configuration of a new Sentinel.
		if master != "127.0.0.1" {
			down, err := r.redisClient.SentinelMasterDown(sip)
			if err != nil {
				r.logger.Warningf("Sentinel %s did not give the state of its master: %v", sip, err)
				return false, nil
			}
			if !down {
				r.logger.Infof("Sentinel %s does not flag its master %s as down, so the master can still answer", sip, master)
				return false, nil
			}
		}
		replicas, err := r.redisClient.GetSentinelReplicas(sip)
		if err != nil {
			r.logger.Warningf("Sentinel %s did not give its replicas, so it can know one: %v", sip, err)
			return false, nil
		}
		for _, replica := range replicas {
			if pod, ok := pods[replica]; ok {
				r.logger.Infof("Sentinel %s knows the replica pod %s (%s), so Sentinel can fail over", sip, pod, replica)
				return false, nil
			}
		}
		r.logger.Warningf("Sentinel %s monitors %s, which is not a pod, and knows no replica pod of %d replicas", sip, master, len(replicas))
	}
	return true, nil
}

// CheckSentinelQuorum returns an error when too few Sentinels run or pass
// SENTINEL CKQUORUM to fail over. The int is the count of unhealthy Sentinels.
func (r *RedisFailoverChecker) CheckSentinelQuorum(rFailover *redisfailoverv1.RedisFailover) (int, error) {

	var unhealthyCnt = -1

	sentinels, err := r.GetSentinelsIPs(rFailover)
	if err != nil {
		r.logger.Warningf("CheckSentinelQuorum Error in getting sentinel Ip's")
		return unhealthyCnt, err
	}
	if len(sentinels) < int(getQuorum(rFailover)) {
		unhealthyCnt = int(getQuorum(rFailover)) - len(sentinels)
		r.logger.Warningf("insufficnet sentinel to reach Quorum - Unhealthy count: %d", unhealthyCnt)
		return unhealthyCnt, errors.New("insufficnet sentinel to reach Quorum")
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
		r.logger.Errorf("insufficnet sentinel to reach Quorum - Unhealthy count: %d", unhealthyCnt)
		return unhealthyCnt, errors.New("insufficnet sentinel to reach Quorum")
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

// CheckSentinelMonitor controls if the sentinels are monitoring the expected master
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

// GetMasterIP connects to all redis and returns the master of the redis failover
func (r *RedisFailoverChecker) GetMasterIP(rf *redisfailoverv1.RedisFailover) (string, error) {
	rips, err := r.GetRedisesIPs(rf)
	if err != nil {
		return "", err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return "", err
	}

	var masters []string
	rport := getRedisPort(rf.Spec.Redis.Port)
	for _, rip := range rips {
		master, err := r.redisClient.IsMaster(rip, rport, password)
		if err != nil {
			r.logger.Errorf("Get redis info failed, maybe this node is not ready, pod ip: %s", rip)
			continue
		}
		if master {
			masters = append(masters, rip)
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
		r.logger.Error(err.Error())
		return nMasters, err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		r.logger.Errorf("Error getting password: %s", err.Error())
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
			r.logger.Errorf("Get redis info failed, maybe this node is not ready, pod ip: %s", rp.Status.PodIP)
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

// GetRedisesIPs returns the IPs of the Redis nodes
func (r *RedisFailoverChecker) GetRedisesIPs(rf *redisfailoverv1.RedisFailover) ([]string, error) {
	redises := []string{}
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return nil, err
	}
	for _, rp := range rps.Items {
		if rp.Status.Phase == corev1.PodRunning && rp.DeletionTimestamp == nil { // Only work with running pods
			redises = append(redises, rp.Status.PodIP)
		}
	}
	return redises, nil
}

// GetSentinelsIPs returns the IPs of the Sentinel nodes
func (r *RedisFailoverChecker) GetSentinelsIPs(rf *redisfailoverv1.RedisFailover) ([]string, error) {
	sentinels := []string{}
	rps, err := r.k8sService.GetDeploymentPods(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return nil, err
	}
	for _, sp := range rps.Items {
		if sp.Status.Phase == corev1.PodRunning && sp.DeletionTimestamp == nil { // Only work with running pods
			sentinels = append(sentinels, sp.Status.PodIP)
		}
	}
	return sentinels, nil
}

// GetMaxRedisPodTime returns the MAX uptime among the active Pods
func (r *RedisFailoverChecker) GetMaxRedisPodTime(rf *redisfailoverv1.RedisFailover) (time.Duration, error) {
	maxTime := 0 * time.Hour
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return maxTime, err
	}
	for _, redisNode := range rps.Items {
		if redisNode.Status.StartTime == nil {
			continue
		}
		start := redisNode.Status.StartTime.Round(time.Second)
		alive := time.Since(start)
		r.logger.Debugf("Pod %s has been alive for %.f seconds", redisNode.Status.PodIP, alive.Seconds())
		if alive > maxTime {
			maxTime = alive
		}
	}
	return maxTime, nil
}

// GetRedisesSlavesPods returns pods names of the Redis secondary nodes
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

// GetStatefulSetUpdateRevision returns `status.updateRevision` of the redis
// StatefulSet. The rollout updates each pod whose revision label differs.
func (r *RedisFailoverChecker) GetStatefulSetUpdateRevision(rFailover *redisfailoverv1.RedisFailover) (string, error) {
	ss, err := r.k8sService.GetStatefulSet(rFailover.Namespace, GetRedisName(rFailover))
	if err != nil {
		return "", err
	}

	if ss == nil {
		return "", errors.New("statefulSet not found")
	}

	return ss.Status.UpdateRevision, nil
}

// GetRedisRevisionHash returns the controller-revision-hash label of the pod.
// It returns "" while an in-place resize is pending, so the rollout treats the
// pod as stale until the resize is applied.
func (r *RedisFailoverChecker) GetRedisRevisionHash(podName string, rFailover *redisfailoverv1.RedisFailover) (string, error) {
	pod, err := r.k8sService.GetPod(rFailover.Namespace, podName)
	if err != nil {
		return "", err
	}

	if pod == nil {
		return "", errors.New("pod not found")
	}

	if pod.Labels == nil {
		return "", errors.New("labels not found")
	}

	// A pod being resized in place is on no revision until the resize is
	// applied, even when its label matches again, for example after a revert.
	if pod.Annotations[resizeRequestedAnnotation] != "" {
		return "", nil
	}

	val := pod.Labels[appsv1.ControllerRevisionHashLabelKey]

	return val, nil
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

// IsRedisRunning returns true if all the pods are Running
func (r *RedisFailoverChecker) IsRedisRunning(rFailover *redisfailoverv1.RedisFailover) bool {
	dp, err := r.k8sService.GetStatefulSetPods(rFailover.Namespace, GetRedisName(rFailover))
	return err == nil && len(dp.Items) > int(rFailover.Spec.Redis.Replicas-1) && AreAllRunning(dp, int(rFailover.Spec.Redis.Replicas))
}

// IsRedisRunningQuorum returns true when at least a majority (quorum) of the
// redis pods are Running. Unlike IsRedisRunning it does not require the full set,
// so healing can still proceed while a minority of pods are stuck Pending.
func (r *RedisFailoverChecker) IsRedisRunningQuorum(rFailover *redisfailoverv1.RedisFailover) bool {
	dp, err := r.k8sService.GetStatefulSetPods(rFailover.Namespace, GetRedisName(rFailover))
	return err == nil && AreQuorumRunning(dp, int(rFailover.Spec.Redis.Replicas))
}

// IsSentinelRunning returns true if all the pods are Running
func (r *RedisFailoverChecker) IsSentinelRunning(rFailover *redisfailoverv1.RedisFailover) bool {
	dp, err := r.k8sService.GetDeploymentPods(rFailover.Namespace, GetSentinelName(rFailover))
	return err == nil && len(dp.Items) > int(rFailover.Spec.Sentinel.Replicas-1) && AreAllRunning(dp, int(rFailover.Spec.Sentinel.Replicas))
}

// IsSentinelRunningQuorum returns true when at least a majority (quorum) of the
// sentinel pods are Running, so the operator can reconfigure the surviving
// sentinels even while a minority are stuck Pending.
func (r *RedisFailoverChecker) IsSentinelRunningQuorum(rFailover *redisfailoverv1.RedisFailover) bool {
	dp, err := r.k8sService.GetDeploymentPods(rFailover.Namespace, GetSentinelName(rFailover))
	return err == nil && AreQuorumRunning(dp, int(rFailover.Spec.Sentinel.Replicas))
}

// IsClusterRunning returns true if all the pods in the given redisfailover are Running
func (r *RedisFailoverChecker) IsClusterRunning(rFailover *redisfailoverv1.RedisFailover) bool {
	return r.IsSentinelRunning(rFailover) && r.IsRedisRunning(rFailover)
}

// CheckMasterHealth checks if the current master is healthy and reachable.
// Returns (healthy, masterIP, error)
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
		r.logger.WithField("ip", masterIP).Warnf("Master health check failed: %v", err)
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
	r.logger.Infof("Promotion candidates: %s", strings.Join(candidates, ", "))

	r.logger.Infof("Selected replica %s (pod: %s, pod ready: %t, offset: %d) for promotion", best.IP, best.PodName, best.PodReady, best.ReplicationOffset)
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

// GetReplicaReplicationOffsets returns replication offset information for all replicas
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
			r.logger.WithField("ip", rp.Status.PodIP).Warnf("Failed to get replication info: %v", err)
			continue
		}

		// Skip masters
		if replInfo.Role == "master" {
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

func getRedisPort(p int32) string {
	return strconv.Itoa(int(p))
}

func AreAllRunning(pods *corev1.PodList, expectedRunningPods int) bool {
	var runningPods int
	for _, pod := range pods.Items {
		if util.PodIsScheduling(&pod) {
			return false
		}
		if util.PodIsTerminal(&pod) {
			continue
		}
		runningPods++
	}
	return runningPods >= expectedRunningPods
}

// AreQuorumRunning reports whether a quorum (majority) of the expected pods
// runs. It does not count scheduling and terminal pods. After a partial
// outage, the operator can thus heal the other pods while a minority of pods
// is Pending.
func AreQuorumRunning(pods *corev1.PodList, expectedReplicas int) bool {
	var runningPods int
	for i := range pods.Items {
		pod := &pods.Items[i]
		if util.PodIsScheduling(pod) || util.PodIsTerminal(pod) {
			continue
		}
		runningPods++
	}
	return runningPods >= expectedReplicas/2+1
}
