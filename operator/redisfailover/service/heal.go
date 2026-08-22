package service

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
	v1 "k8s.io/api/core/v1"
)

// ErrPartialReconciliation is returned by PromoteBestReplica when the new
// master was promoted successfully but one or more replicas could not be
// repointed or relabelled.  The caller should treat this as an incomplete
// failover, not a total failure.
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
}

// RedisFailoverHealer is our implementation of RedisFailoverCheck interface
type RedisFailoverHealer struct {
	k8sService  k8s.Services
	redisClient redis.Client
	logger      log.Logger
}

// NewRedisFailoverHealer creates an object of the RedisFailoverChecker struct
func NewRedisFailoverHealer(k8sService k8s.Services, redisClient redis.Client, logger log.Logger) *RedisFailoverHealer {
	logger = logger.With("service", "redis.healer")
	return &RedisFailoverHealer{
		k8sService:  k8sService,
		redisClient: redisClient,
		logger:      logger,
	}
}

func (r *RedisFailoverHealer) setMasterLabelIfNecessary(namespace string, pod v1.Pod) error {
	for labelKey, labelValue := range pod.Labels {
		if labelKey == redisRoleLabelKey && labelValue == redisRoleLabelMaster {
			return nil
		}
	}
	return r.k8sService.UpdatePodLabels(namespace, pod.Name, generateRedisMasterRoleLabel())
}

func (r *RedisFailoverHealer) setSlaveLabelIfNecessary(namespace string, pod v1.Pod) error {
	for labelKey, labelValue := range pod.Labels {
		if labelKey == redisRoleLabelKey && labelValue == redisRoleLabelSlave {
			return nil
		}
	}
	return r.k8sService.UpdatePodLabels(namespace, pod.Name, generateRedisSlaveRoleLabel())
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
			return r.setMasterLabelIfNecessary(rf.Namespace, rp)
		}
	}
	return nil
}

// SetOldestAsMaster puts all redis to the same master, choosen by order of appearance
func (r *RedisFailoverHealer) SetOldestAsMaster(rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}
	if len(ssp.Items) < 1 {
		return errors.New("number of redis pods are 0")
	}

	// Order the pods so we start by the oldest one
	sort.Slice(ssp.Items, func(i, j int) bool {
		return ssp.Items[i].CreationTimestamp.Before(&ssp.Items[j].CreationTimestamp)
	})

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	newMasterIP := ""
	for _, pod := range ssp.Items {
		if newMasterIP == "" {
			newMasterIP = pod.Status.PodIP
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("New master is %s with ip %s", pod.Name, newMasterIP)
			if err := r.redisClient.MakeMaster(newMasterIP, port, password); err != nil {
				newMasterIP = ""
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Make new master failed, master ip: %s, error: %v", pod.Status.PodIP, err)
				continue
			}

			err = r.setMasterLabelIfNecessary(rf.Namespace, pod)
			if err != nil {
				return err
			}

			newMasterIP = pod.Status.PodIP
		} else {
			r.logger.Infof("Making pod %s slave of %s", pod.Name, newMasterIP)
			if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, newMasterIP, port, password); err != nil {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Make slave failed, slave pod ip: %s, master ip: %s, error: %v", pod.Status.PodIP, newMasterIP, err)
			}

			err = r.setSlaveLabelIfNecessary(rf.Namespace, pod)
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

// podIPBelongsTo reports whether ip is currently the PodIP of one of the
// given pods. GetStatefulSetPods scopes its List() call by namespace and by
// the owning StatefulSet's own label selector, so a freshly-fetched pods
// list can only ever contain this RedisFailover's own pods: Kubernetes never
// hands the same live IP to two Running pods at once. Checking membership
// against a list fetched right before a mutating call therefore closes the
// window where a master/replica IP resolved earlier in the reconcile could
// since have been reassigned (e.g. after node churn) to an unrelated pod,
// possibly belonging to a different RedisFailover in another namespace. See
// https://github.com/spotahome/redis-operator/issues/698.
func podIPBelongsTo(pods *v1.PodList, ip string) bool {
	for _, pod := range pods.Items {
		if pod.Status.PodIP == ip {
			return true
		}
	}
	return false
}

// SetMasterOnAll puts all redis nodes as a slave of a given master
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
		//During this configuration process if there is a new master selected , bailout
		isMaster, err := r.redisClient.IsMaster(masterIP, port, password)
		if err != nil || !isMaster {
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("check master failed maybe this node is not ready(ip changed), or sentinel made a switch: %s", masterIP)
			return err
		} else {
			if pod.Status.PodIP == masterIP {
				continue
			}
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Making pod %s slave of %s", pod.Name, masterIP)
			if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, masterIP, port, password); err != nil {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Errorf("Make slave failed, slave ip: %s, master ip: %s, error: %v", pod.Status.PodIP, masterIP, err)
				return err
			}

			err = r.setSlaveLabelIfNecessary(rf.Namespace, pod)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// SetExternalMasterOnAll puts all redis nodes as a slave of a given master outside of
// the current RedisFailover instance
func (r *RedisFailoverHealer) SetExternalMasterOnAll(masterIP, masterPort string, rf *redisfailoverv1.RedisFailover) error {
	ssp, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	for _, pod := range ssp.Items {
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Making pod %s slave of %s:%s", pod.Name, masterIP, masterPort)
		if err := r.redisClient.MakeSlaveOfWithPort(pod.Status.PodIP, masterIP, masterPort, password); err != nil {
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

// RestoreSentinel clear the number of sentinels on memory
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

// DeletePod delete a failing pod so kubernetes relaunch it again
func (r *RedisFailoverHealer) DeletePod(podName string, rFailover *redisfailoverv1.RedisFailover) error {
	r.logger.WithField("redisfailover", rFailover.Name).WithField("namespace", rFailover.Namespace).Infof("Deleting pods %s...", podName)
	return r.k8sService.DeletePod(rFailover.Namespace, podName)
}

// PromoteBestReplica promotes a replica to master and reconfigures all other replicas.
// This is used for operator-managed failover when Sentinel is disabled.
func (r *RedisFailoverHealer) PromoteBestReplica(newMasterIP string, rf *redisfailoverv1.RedisFailover) error {
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	port := getRedisPort(rf.Spec.Redis.Port)

	// Fetch this RedisFailover's own pods fresh, immediately before acting on
	// newMasterIP, and verify it's still one of them. This closes the race
	// where newMasterIP was resolved earlier in the reconcile and has since
	// been reassigned to an unrelated pod, possibly in a different
	// namespace/RedisFailover. See https://github.com/spotahome/redis-operator/issues/698.
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
			if err := r.setMasterLabelIfNecessary(rf.Namespace, rp); err != nil {
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

		if err := r.redisClient.MakeSlaveOfWithPort(rp.Status.PodIP, newMasterIP, port, password); err != nil {
			r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).
				Errorf("Failed to make %s slave of %s: %v", rp.Status.PodIP, newMasterIP, err)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}

		if err := r.setSlaveLabelIfNecessary(rf.Namespace, rp); err != nil {
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
