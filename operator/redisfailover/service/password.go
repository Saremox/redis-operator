package service

import (
	"errors"
	"fmt"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/redis"
	v1 "k8s.io/api/core/v1"
)

// ApplyPassword brings every Redis onto password. Redis reads requirepass only
// at startup, and restarting the pods one at a time can't apply a new one: a
// restarted replica can't authenticate to a master still on the old password.
// So a Redis still on previous is changed in place, and the rolling update then
// restarts the pods onto the secret.
//
// It returns an error if a running Redis refuses password and can't be
// changed, and true once every Redis pod runs and accepts it.
func (r *RedisFailoverHealer) ApplyPassword(rf *redisfailoverv1.RedisFailover, password, previous string) (bool, error) {
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return false, err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	complete := true
	var errs []error
	for _, rp := range rps.Items {
		if rp.DeletionTimestamp != nil {
			continue
		}
		// A pod yet to start may still come up on the old pod template.
		if rp.Status.Phase != v1.PodRunning {
			complete = false
			continue
		}
		_, err := r.redisClient.IsMaster(rp.Status.PodIP, port, password)
		if err == nil {
			continue
		}
		if !redis.IsAuthError(err) {
			complete = false
			continue
		}
		current := previous
		if redis.IsNoPasswordError(err) {
			current = ""
		}
		if current == password {
			errs = append(errs, fmt.Errorf("redis pod %s refuses the configured password and the operator doesn't know the one it runs with; put the previous password back in the secret until the RedisFailover is healthy, then change it again", rp.Name))
			continue
		}
		if err := r.redisClient.SetPassword(rp.Status.PodIP, port, current, password); err != nil {
			errs = append(errs, fmt.Errorf("changing the password of redis pod %s: %w", rp.Name, err))
			continue
		}
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Changed the password of redis pod %s", rp.Name)
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	return complete, nil
}

// ApplySentinelPassword gives the Sentinels the password to authenticate to
// Redis with. It returns true once every running Sentinel has it.
func (r *RedisFailoverHealer) ApplySentinelPassword(rf *redisfailoverv1.RedisFailover, password string) (bool, error) {
	if !rf.SentinelsAllowed() {
		return true, nil
	}
	sps, err := r.k8sService.GetDeploymentPods(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return false, err
	}
	complete := true
	for _, sp := range sps.Items {
		if sp.Status.Phase != v1.PodRunning || sp.DeletionTimestamp != nil {
			continue
		}
		if err := r.redisClient.SetSentinelAuthPass(sp.Status.PodIP, password); err != nil {
			if redis.IsUnreachableError(err) {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Warningf("Sentinel pod %s is unreachable, its password is changed later: %v", sp.Name, err)
				complete = false
				continue
			}
			return false, fmt.Errorf("changing the password of sentinel pod %s: %w", sp.Name, err)
		}
	}
	return complete, nil
}
