package service

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
	"github.com/saremox/redis-operator/service/k8s"
)

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
	logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return err
	}

	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return err
	}

	rport := getRedisPort(rf.Spec.Redis.Port)
	// The loop labels every pod before it returns an error. Otherwise, one
	// bad pod early in the list can stop the label of the new master. Then the
	// master Service has no endpoint.
	var wrong []*WrongMasterError
	for _, rp := range rps.Items {
		if rp.Status.PodIP == master {
			err = r.setMasterLabelIfNecessary(rf, rp)
		} else {
			err = r.setSlaveLabelIfNecessary(rf, rp, rport, password)
		}
		if err != nil {
			logWrongMasters(logger, wrong, -1)
			return err
		}

		if rp.DeletionTimestamp != nil {
			// A terminating pod is going away, and dialing its IP can block
			// for the whole connection timeout.
			continue
		}
		if rp.Status.PodIP == "" {
			logger.WithField("pod", rp.Name).Debugf("Pod %s has no IP yet", rp.Name)
			continue
		}
		slave, err := r.redisClient.GetSlaveOf(rp.Status.PodIP, rport, password)
		if err != nil {
			// The pod does not answer, usually the old master on a lost node.
			// Skip it, so that the heal of the reachable pods continues.
			podErrorLogf(logger, &rp)("Get slave of master failed, maybe this node is not ready, pod ip: %s: %v", rp.Status.PodIP, err)
			continue
		}
		if slave != "" && slave != master {
			wrong = append(wrong, &WrongMasterError{Pod: rp.Name, IP: rp.Status.PodIP, Master: master, Has: slave})
		}
	}
	if len(wrong) == 0 {
		return nil
	}
	// The caller logs the returned error at Warning, so each other replica
	// gets its own line. A different master can be a second master, so it
	// comes first.
	returned := max(slices.IndexFunc(wrong, func(w *WrongMasterError) bool { return !w.local() }), 0)
	logWrongMasters(logger, wrong, returned)
	return wrong[returned]
}

// logWrongMasters logs each replica in wrong, except the one at index skip.
// A replica with 127.0.0.1 as master started a short time ago.
func logWrongMasters(logger log.Logger, wrong []*WrongMasterError, skip int) {
	for i, w := range wrong {
		if i == skip {
			continue
		}
		podLogger := logger.WithField("pod", w.Pod)
		if w.local() {
			podLogger.Infof("%v", w)
		} else {
			podLogger.Warningf("%v", w)
		}
	}
}

// WrongMasterError gives the pod of a replica with a wrong master, so that the
// caller can log the pod as a field.
type WrongMasterError struct {
	Pod    string
	IP     string
	Master string
	Has    string
}

// Error names the replica, its pod, the expected master and the master that
// the replica has.
func (e *WrongMasterError) Error() string {
	return fmt.Sprintf("slave %s (pod %s) don't have the master %s, has %s", e.IP, e.Pod, e.Master, e.Has)
}

func (e *WrongMasterError) local() bool {
	return e.Has == "127.0.0.1"
}

// podErrorLogf logs at Debug for a pod that is not Ready, because such a pod
// usually restarts.
func podErrorLogf(logger log.Logger, pod *corev1.Pod) func(string, ...any) {
	logger = logger.WithField("pod", pod.Name)
	if util.PodIsReady(pod) {
		return logger.Warningf
	}
	return logger.Debugf
}
