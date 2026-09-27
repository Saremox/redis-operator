package service

import (
	"errors"
	"fmt"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
	v1 "k8s.io/api/core/v1"
)

// ApplyPassword brings every Redis onto the password in the auth
// secret. Redis reads requirepass only at startup, and restarting the pods one
// at a time can't apply a new one: a restarted replica can't authenticate to a
// master still on the old password. So a Redis still on previous is changed in
// place, and the rolling update then restarts the pods onto the secret. The
// Sentinels are given the password too.
//
// It returns true once every Redis pod runs and accepts the password.
func (r *RedisFailoverHealer) ApplyPassword(rf *redisfailoverv1.RedisFailover, previous string) (bool, error) {
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return false, err
	}
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
		if previous == password {
			errs = append(errs, fmt.Errorf("redis pod %s refuses the password in secret %q and the operator doesn't know the one it runs with; delete the redis pods to restart them onto it", rp.Name, rf.Spec.Auth.SecretPath))
			continue
		}
		if err := r.redisClient.SetPassword(rp.Status.PodIP, port, previous, password); err != nil {
			errs = append(errs, fmt.Errorf("changing the password of redis pod %s: %w", rp.Name, err))
			continue
		}
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Changed the password of redis pod %s", rp.Name)
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	if !complete || !rf.SentinelsAllowed() {
		return complete, nil
	}

	sps, err := r.k8sService.GetDeploymentPods(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return false, err
	}
	for _, sp := range sps.Items {
		if sp.Status.Phase != v1.PodRunning || sp.DeletionTimestamp != nil {
			continue
		}
		if err := r.redisClient.SetSentinelAuthPass(sp.Status.PodIP, password); err != nil {
			return false, fmt.Errorf("changing the password of sentinel pod %s: %w", sp.Name, err)
		}
	}
	return true, nil
}
