package service

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
	v1 "k8s.io/api/core/v1"
)

// ErrPartialReconciliation is the error of PromoteBestReplica when the new
// master is promoted, but the change of a replica or of its label failed.
// The failover is then incomplete, but the new master works.
var ErrPartialReconciliation = errors.New("promotion succeeded but replica reconciliation incomplete")

// RedisFailoverHeal defines the interface able to fix the problems on the redis failovers
type RedisFailoverHeal interface {
	MakeMaster(ip string, rFailover *redisfailoverv1.RedisFailover) error
	SetOldestAsMaster(rFailover *redisfailoverv1.RedisFailover) error
	SetMasterOnAll(masterIP string, rFailover *redisfailoverv1.RedisFailover) error
	SetExternalMasterOnAll(masterIP string, masterPort string, rFailover *redisfailoverv1.RedisFailover) error
	NewSentinelMonitor(ip string, monitor string, rFailover *redisfailoverv1.RedisFailover) error
	NewSentinelMonitorWithPort(ip string, monitor string, port string, rFailover *redisfailoverv1.RedisFailover) error
	RestoreSentinel(ip string) error
	SetSentinelCustomConfig(ip string, rFailover *redisfailoverv1.RedisFailover) error
	SetRedisCustomConfig(ip string, rFailover *redisfailoverv1.RedisFailover) error
	DeletePod(podName string, rFailover *redisfailoverv1.RedisFailover) error
	PromoteBestReplica(newMasterIP string, rFailover *redisfailoverv1.RedisFailover) error
	HandOverMaster(masterIP, targetIP string, rFailover *redisfailoverv1.RedisFailover) (HandoverResult, error)
	EnsureRedisMaxMemory(rFailover *redisfailoverv1.RedisFailover, master string, redises []string) (MaxMemoryResult, error)
	ResizePodInPlace(rFailover *redisfailoverv1.RedisFailover, podName, updateRevision string) (ResizeResult, error)
	ApplyPassword(rFailover *redisfailoverv1.RedisFailover, password string, previous []string) (bool, error)
	ApplySentinelPassword(rFailover *redisfailoverv1.RedisFailover, password string) (bool, error)
}

// RedisFailoverHealer implements RedisFailoverHeal.
type RedisFailoverHealer struct {
	k8sService  k8s.Services
	redisClient redis.Client
	logger      log.Logger
	opts        options
}

// NewRedisFailoverHealer returns a RedisFailoverHealer.
func NewRedisFailoverHealer(k8sService k8s.Services, redisClient redis.Client, logger log.Logger, opts ...Option) *RedisFailoverHealer {
	logger = logger.With("service", "redis.healer")
	return &RedisFailoverHealer{
		k8sService:  k8sService,
		redisClient: redisClient,
		logger:      logger,
		opts:        applyOptions(opts),
	}
}

func (r *RedisFailoverHealer) setMasterLabelIfNecessary(rf *redisfailoverv1.RedisFailover, pod v1.Pod) error {
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

func (r *RedisFailoverHealer) setSlaveLabelIfNecessary(rf *redisfailoverv1.RedisFailover, pod v1.Pod, port, password string) error {
	return setSlaveLabel(r.k8sService, r.opts, rf, pod, port, password)
}

func (r *RedisFailoverHealer) MakeMaster(ip string, rf *redisfailoverv1.RedisFailover) error {
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	err = r.redisClient.MakeMaster(ip, port, password)
	if err != nil {
		return err
	}

	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	for _, rp := range rps.Items {
		if rp.Status.PodIP == ip {
			return r.setMasterLabelIfNecessary(rf, rp)
		}
	}
	return nil
}

// SetOldestAsMaster makes the first pod from masterCandidates the master of
// all the Redis pods. While a Ready master pod is in deletion, it returns nil
// and elects no master.
func (r *RedisFailoverHealer) SetOldestAsMaster(rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	for i := range ssp.Items {
		// A terminating Ready master still accepts writes. Another master
		// gives two writable masters.
		if pod := &ssp.Items[i]; pod.DeletionTimestamp != nil && IsMasterPod(pod) && util.PodIsReady(pod) {
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Master pod %s is stopping, waiting for it to exit before electing a master", pod.Name)
			return nil
		}
	}
	pods := masterCandidates(ssp.Items)
	if len(pods) < 1 {
		return errors.New("number of redis pods are 0")
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	newMasterIP := ""
	for _, pod := range pods {
		if newMasterIP == "" {
			if pod.Status.PodIP == "" {
				r.logger.Debugf("Pod %s has no IP yet, so it cannot be the master", pod.Name)
				continue
			}
			newMasterIP = pod.Status.PodIP
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("New master is %s with ip %s", pod.Name, newMasterIP)
			if err := r.redisClient.MakeMaster(newMasterIP, port, password); err != nil {
				newMasterIP = ""
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Make new master failed, master ip: %s, error: %v", pod.Status.PodIP, err)
				continue
			}

			err = r.setMasterLabelIfNecessary(rf, pod)
			if err != nil {
				return err
			}

			newMasterIP = pod.Status.PodIP
		} else {
			if pod.Status.PodIP == "" {
				r.logger.Debugf("Pod %s has no IP yet, so it is not made a replica", pod.Name)
			} else {
				r.logger.Infof("Making pod %s slave of %s", pod.Name, newMasterIP)
				if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, port, newMasterIP, port, password); err != nil {
					r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Make slave failed, slave pod ip: %s, master ip: %s, error: %v", pod.Status.PodIP, newMasterIP, err)
				}
			}

			err = r.setSlaveLabelIfNecessary(rf, pod, port, password)
			if err != nil {
				return err
			}
		}
	}
	if newMasterIP == "" {
		return errors.New("SetOldestAsMaster- unable to set master")
	} else {
		return nil
	}
}

// masterCandidates removes terminating pods, because they stop soon. Ready
// pods come first, because a Ready replica synced recently.
func masterCandidates(items []v1.Pod) []v1.Pod {
	pods := make([]v1.Pod, 0, len(items))
	for _, pod := range items {
		if pod.DeletionTimestamp == nil {
			pods = append(pods, pod)
		}
	}
	sort.SliceStable(pods, func(i, j int) bool {
		if ri, rj := util.PodIsReady(&pods[i]), util.PodIsReady(&pods[j]); ri != rj {
			return ri
		}
		return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
	return pods
}

// podIPBelongsTo reports whether ip is the PodIP of one of the pods. The
// caller reads the pods of the RedisFailover just before a change. An IP read
// earlier in the reconcile can belong to a different pod at that time, also
// to a pod of a different RedisFailover. See
// https://github.com/spotahome/redis-operator/issues/698.
func podIPBelongsTo(pods *v1.PodList, ip string) bool {
	for _, pod := range pods.Items {
		if pod.Status.PodIP == ip {
			return true
		}
	}
	return false
}

// SetMasterOnAll makes each Redis pod a replica of masterIP.
func (r *RedisFailoverHealer) SetMasterOnAll(masterIP string, rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}

	if !podIPBelongsTo(ssp, masterIP) {
		err := fmt.Errorf("refusing to set master %s: it is not currently a pod of %s/%s, bailing out this round", masterIP, rf.Namespace, rf.Name)
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Error(err.Error())
		return err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	for _, pod := range ssp.Items {
		// Stop when masterIP is not the master any more, for example after a
		// Sentinel failover.
		isMaster, err := r.redisClient.IsMaster(masterIP, port, password)
		if err != nil || !isMaster {
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("check master failed maybe this node is not ready(ip changed), or sentinel made a switch: %s", masterIP)
			if err != nil {
				return err
			}
			return fmt.Errorf("refusing to continue: %s is no longer the master, bailing out this round", masterIP)
		} else {
			if pod.Status.PodIP == masterIP {
				continue
			}
			if pod.Status.PodIP == "" {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Debugf("Pod %s has no IP yet, skipped", pod.Name)
				continue
			}
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Making pod %s slave of %s", pod.Name, masterIP)
			unlabelledMaster, err := r.isUnlabelledMaster(pod, port, password)
			if redis.IsUnreachableError(err) {
				// REPLICAOF would wait for the same timeout and fail too.
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Make slave skipped, pod %s does not answer: %v", pod.Name, err)
				continue
			}
			if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, port, masterIP, port, password); err != nil {
				// The pod does not answer, usually the old master on a lost node.
				// Skip it, so that the other replicas still change. When the pod
				// answers again, a later reconcile makes it a replica.
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Make slave failed, slave ip: %s, master ip: %s, error: %v", pod.Status.PodIP, masterIP, err)
				continue
			}

			// The pod is a replica now, so the disconnect does not wait for the
			// label: a failed label update must not keep the clients connected.
			if unlabelledMaster {
				r.opts.disconnector.DisconnectDemoted(rf, pod, port, password)
			}
			err = r.setSlaveLabelIfNecessary(rf, pod, port, password)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// isUnlabelledMaster tells if pod answers as a Redis master but has no master
// label. An old master that comes back after a failover usually has the slave
// label, and setSlaveLabel disconnects clients only when it removes the master label.
func (r *RedisFailoverHealer) isUnlabelledMaster(pod v1.Pod, port, password string) (bool, error) {
	if r.opts.disconnector == nil || pod.Labels[redisRoleLabelKey] == redisRoleLabelMaster {
		return false, nil
	}
	isMaster, err := r.redisClient.IsMaster(pod.Status.PodIP, port, password)
	return err == nil && isMaster, err
}

// SetExternalMasterOnAll makes each Redis pod a replica of a master outside
// the RedisFailover, the bootstrap node.
func (r *RedisFailoverHealer) SetExternalMasterOnAll(masterIP, masterPort string, rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	// The target pods' own port, which is not necessarily masterPort - the
	// external bootstrap master can be configured on a different port than
	// this RedisFailover's own Redis pods.
	port := getRedisPort(rf.Spec.Redis.Port)
	for _, pod := range ssp.Items {
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Making pod %s slave of %s:%s", pod.Name, masterIP, masterPort)
		if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, port, masterIP, masterPort, password); err != nil {
			return err
		}

	}
	return nil
}

// NewSentinelMonitor changes the master that Sentinel has to monitor
func (r *RedisFailoverHealer) NewSentinelMonitor(ip string, monitor string, rf *redisfailoverv1.RedisFailover) error {
	quorum := strconv.Itoa(int(getQuorum(rf)))

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	return r.redisClient.MonitorRedisWithPort(ip, monitor, port, quorum, password)
}

// NewSentinelMonitorWithPort changes the master that Sentinel has to monitor by the provided IP and Port
func (r *RedisFailoverHealer) NewSentinelMonitorWithPort(ip string, monitor string, monitorPort string, rf *redisfailoverv1.RedisFailover) error {
	quorum := strconv.Itoa(int(getQuorum(rf)))

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	return r.redisClient.MonitorRedisWithPort(ip, monitor, monitorPort, quorum, password)
}

// RestoreSentinel sends SENTINEL RESET *. The Sentinel then forgets the other
// Sentinels and the replicas until it finds them again. The checks use it when
// a Sentinel knows a wrong number of Sentinels or of replicas.
func (r *RedisFailoverHealer) RestoreSentinel(ip string) error {
	r.logger.Debugf("Restoring sentinel %s", ip)
	return r.redisClient.ResetSentinel(ip)
}

// SetSentinelCustomConfig will call sentinel to set the configuration given in config
func (r *RedisFailoverHealer) SetSentinelCustomConfig(ip string, rf *redisfailoverv1.RedisFailover) error {
	r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Debugf("Setting the custom config on sentinel %s...", ip)
	return r.redisClient.SetCustomSentinelConfig(ip, rf.Spec.Sentinel.CustomConfig)
}

// SetRedisCustomConfig will call redis to set the configuration given in config
func (r *RedisFailoverHealer) SetRedisCustomConfig(ip string, rf *redisfailoverv1.RedisFailover) error {
	r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Debugf("Setting the custom config on redis %s...", ip)

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	return r.redisClient.SetCustomRedisConfig(ip, port, rf.Spec.Redis.CustomConfig, password)
}

// DeletePod deletes a stale pod in the rollout. The StatefulSet creates it
// again on the update revision.
func (r *RedisFailoverHealer) DeletePod(podName string, rFailover *redisfailoverv1.RedisFailover) error {
	r.logger.WithField("redisfailover", rFailover.Name).WithField("namespace", rFailover.Namespace).Infof("Deleting pods %s...", podName)
	return r.k8sService.DeletePod(rFailover.Namespace, podName)
}

// PromoteBestReplica makes newMasterIP the master and the other running pods
// its replicas. The operator-managed failover uses it, and electMasterForSentinel
// uses it when no Sentinel can fail over.
func (r *RedisFailoverHealer) PromoteBestReplica(newMasterIP string, rf *redisfailoverv1.RedisFailover) error {
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)

	// newMasterIP can belong to a different pod at this time, so read the
	// pods again just before the promotion. podIPBelongsTo gives the reason.
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	if !podIPBelongsTo(rps, newMasterIP) {
		err := fmt.Errorf("refusing to promote %s: it is not currently a pod of %s/%s, bailing out this round", newMasterIP, rf.Namespace, rf.Name)
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Error(err.Error())
		return err
	}

	// Step 1: Promote the selected replica to master
	r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
		Infof("Promoting replica %s to master", newMasterIP)

	if err := r.redisClient.MakeMaster(newMasterIP, port, password); err != nil {
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
			Errorf("Failed to promote replica %s to master: %v", newMasterIP, err)
		return err
	}

	// Step 2: Update pod labels for the new master
	for _, rp := range rps.Items {
		if rp.Status.PodIP == newMasterIP {
			if err := r.setMasterLabelIfNecessary(rf, rp); err != nil {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
					Errorf("Failed to set master label on pod %s: %v", rp.Name, err)
				return err
			}
			break
		}
	}

	// Step 3: Reconfigure all other replicas to point to the new master
	r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
		Infof("Reconfiguring replicas to use new master %s", newMasterIP)

	var reconcileErrs []error
	for _, rp := range rps.Items {
		if rp.Status.PodIP == newMasterIP {
			continue
		}
		if rp.Status.Phase != v1.PodRunning || rp.DeletionTimestamp != nil {
			continue
		}

		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
			Infof("Making pod %s slave of %s", rp.Name, newMasterIP)

		if err := r.redisClient.MakeSlaveOfWithPort(rp.Status.PodIP, port, newMasterIP, port, password); err != nil {
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
				Errorf("Failed to make %s slave of %s: %v", rp.Status.PodIP, newMasterIP, err)
			reconcileErrs = append(reconcileErrs, err)
			// The old master that does not answer loses its master label, so
			// that the new master is the only pod with the label when the old
			// one answers again as a master.
			if rp.Labels[redisRoleLabelKey] == redisRoleLabelMaster {
				if err := r.setSlaveLabelIfNecessary(rf, rp, port, password); err != nil {
					reconcileErrs = append(reconcileErrs, err)
				}
			}
			continue
		}

		if err := r.setSlaveLabelIfNecessary(rf, rp, port, password); err != nil {
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
				Errorf("Failed to set slave label on pod %s: %v", rp.Name, err)
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	if joinedErr := errors.Join(reconcileErrs...); joinedErr != nil {
		return fmt.Errorf("%w: %w", ErrPartialReconciliation, joinedErr)
	}

	r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
		Infof("Failover completed: %s is now master", newMasterIP)

	return nil
}
