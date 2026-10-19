package service

import (
	"errors"
	"strconv"
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

// RedisFailoverCheck defines the interface able to check the correct status of redis failover.
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
	GetSentinelFailover(sentinel string) (redis.SentinelFailover, error)
	GetSentinelMasterIPs(rFailover *redisfailoverv1.RedisFailover) ([]string, error)
	GetUnknownSentinelAddrs(rFailover *redisfailoverv1.RedisFailover) (SentinelAddrs, error)
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
	GetAutoscalerInfo(rFailover *redisfailoverv1.RedisFailover, ip string) (*redis.AutoscalerInfo, error)
}

// RedisFailoverChecker implements RedisFailoverCheck.
type RedisFailoverChecker struct {
	k8sService    k8s.Services
	redisClient   redis.Client
	logger        log.Logger
	metricsClient metrics.Recorder
	opts          options
}

// NewRedisFailoverChecker creates an object of the RedisFailoverChecker struct.
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

// GetRedisesIPs returns the IPs of the Redis nodes.
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

// GetSentinelsIPs returns the IPs of the Sentinel nodes.
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

// GetMaxRedisPodTime returns the MAX uptime among the active Pods.
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

// ErrUpdateRevisionNotObserved is the error of GetStatefulSetUpdateRevision
// while the status of the StatefulSet does not show its latest template.
var ErrUpdateRevisionNotObserved = errors.New("the statefulset status does not show the latest template")

// GetStatefulSetUpdateRevision returns `status.updateRevision` of the redis
// StatefulSet. The rollout updates each pod whose revision label differs.
// Before the StatefulSet controller observes a template change, the status
// names the revision before the change, and each pod on the latest template
// looks stale. Then it returns ErrUpdateRevisionNotObserved.
func (r *RedisFailoverChecker) GetStatefulSetUpdateRevision(rFailover *redisfailoverv1.RedisFailover) (string, error) {
	ss, err := r.k8sService.GetStatefulSet(rFailover.Namespace, GetRedisName(rFailover))
	if err != nil {
		return "", err
	}

	if ss == nil {
		return "", errors.New("statefulSet not found")
	}
	if ss.Status.ObservedGeneration < ss.Generation || ss.Status.UpdateRevision == "" {
		return "", ErrUpdateRevisionNotObserved
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
	if ResizeRequested(pod) {
		return "", nil
	}

	val := pod.Labels[appsv1.ControllerRevisionHashLabelKey]

	return val, nil
}

// GetAutoscalerInfo reads the INFO fields of the memory autoscaler from the
// Redis at ip.
func (r *RedisFailoverChecker) GetAutoscalerInfo(rFailover *redisfailoverv1.RedisFailover, ip string) (*redis.AutoscalerInfo, error) {
	password, err := k8s.GetRedisPassword(r.k8sService, rFailover)
	if err != nil {
		return nil, err
	}
	return r.redisClient.GetAutoscalerInfo(ip, getRedisPort(rFailover.Spec.Redis.Port), password)
}

// IsRedisRunning returns true if all the pods are Running.
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

// IsSentinelRunning returns true if all the pods are Running.
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

// IsClusterRunning returns true if all the pods in the given redisfailover are Running.
func (r *RedisFailoverChecker) IsClusterRunning(rFailover *redisfailoverv1.RedisFailover) bool {
	return r.IsSentinelRunning(rFailover) && r.IsRedisRunning(rFailover)
}

func getRedisPort(p int32) string {
	return strconv.Itoa(int(p))
}

// AreAllRunning reports whether no pod is Pending or in deletion, and at least
// expectedRunningPods pods are not terminal.
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
