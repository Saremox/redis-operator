package service

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/redis"
	v1 "k8s.io/api/core/v1"
)

// ApplyPassword brings every Redis onto password. Redis reads requirepass only
// at startup, and restarting the pods one at a time cannot apply a new one: a
// restarted replica cannot authenticate to a master still on the old password.
// So a Redis still on one of the previous passwords is changed in place, and the
// rolling update then restarts the pods onto the secret.
//
// It returns an error if a running Redis refuses password and cannot be
// changed. complete is true once every Redis pod runs, accepts password and
// has a template with the password Secret of rf. stale is true while a pod
// has a template with another password Secret: a container restart gives
// Redis the password of that Secret again.
func (r *RedisFailoverHealer) ApplyPassword(rf *redisfailoverv1.RedisFailover, password string, previous []string) (complete, stale bool, err error) {
	rps, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return false, false, err
	}

	port := getRedisPort(rf.Spec.Redis.Port)
	target := redisPasswordRef(append(slices.Clone(rf.Spec.Redis.Env), getRedisEnv(rf)...))
	complete = true
	var errs []error
	var unknown []string
	for _, rp := range rps.Items {
		if rp.DeletionTimestamp != nil {
			continue
		}
		ref := podRedisPasswordRef(rp.Spec.Containers)
		podStale := !sameSecretKey(ref, target)
		stale = stale || podStale
		// A pod yet to start may still come up on the old pod template.
		if rp.Status.Phase != v1.PodRunning {
			complete = false
			continue
		}
		complete = complete && !podStale
		_, err := r.redisClient.IsMaster(rp.Status.PodIP, port, password)
		if err == nil {
			continue
		}
		if !redis.IsAuthError(err) {
			complete = false
			continue
		}
		candidates := previous
		if redis.IsNoPasswordError(err) {
			candidates = []string{""}
		}
		known, err := r.changePodPassword(rf, rp, ref, podStale, port, password, candidates)
		switch {
		case !known:
			unknown = append(unknown, rp.Name)
		case err != nil:
			errs = append(errs, err)
		}
	}
	if len(unknown) > 0 {
		errs = append(errs, unknownPasswordError(rf, unknown))
	}
	if err := errors.Join(errs...); err != nil {
		return false, false, err
	}
	return complete, stale, nil
}

// changePodPassword changes the password of the Redis of rp in place. With
// a stale pod template, it also tries the password of the Secret of the pod.
// known is false when no candidate is the password of the Redis.
func (r *RedisFailoverHealer) changePodPassword(rf *redisfailoverv1.RedisFailover, rp v1.Pod, ref *v1.SecretKeySelector, podStale bool, port, password string, candidates []string) (known bool, err error) {
	tried, err := r.changePassword(rp.Status.PodIP, port, password, candidates)
	if (!tried || redis.IsAuthError(err)) && podStale {
		if current := r.secretPassword(rf, ref); current != "" && !slices.Contains(candidates, current) {
			tried, err = r.changePassword(rp.Status.PodIP, port, password, []string{current})
		}
	}
	if !tried || redis.IsAuthError(err) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("changing the password of redis pod %s: %w", rp.Name, err)
	}
	r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Infof("Changed the password of redis pod %s", rp.Name)
	return true, nil
}

// unknownPasswordError tells the user how to recover the pods that refuse
// the password.
func unknownPasswordError(rf *redisfailoverv1.RedisFailover, unknown []string) error {
	refuse := "redis pod %s refuses the configured password and the operator doesn't know the one it runs with"
	if len(unknown) > 1 {
		refuse = "redis pods %s refuse the configured password and the operator doesn't know the ones they run with"
	}
	hint := "; put the previous password back in the secret until the RedisFailover is healthy, then change it again"
	if rf.Spec.Auth.SecretPath == "" {
		hint = "; set auth.secretPath to a Secret with the previous password until the RedisFailover is healthy, then remove it again"
	}
	return fmt.Errorf(refuse+hint, strings.Join(unknown, ", "))
}

// changePassword logs in to the Redis at ip with each candidate in turn,
// because a Redis refuses each password except the one it runs with.
func (r *RedisFailoverHealer) changePassword(ip, port, password string, candidates []string) (tried bool, err error) {
	for _, current := range candidates {
		if current == password {
			continue
		}
		tried = true
		if err = r.redisClient.SetPassword(ip, port, current, password); !redis.IsAuthError(err) {
			break
		}
	}
	return tried, err
}

// secretPassword returns "" when ref is nil or unreadable: the pod then stays
// unknown, and the error message tells the user how to recover.
func (r *RedisFailoverHealer) secretPassword(rf *redisfailoverv1.RedisFailover, ref *v1.SecretKeySelector) string {
	if ref == nil {
		return ""
	}
	secret, err := r.k8sService.GetSecret(rf.Namespace, ref.Name)
	if err != nil {
		r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Debugf("Unable to read Secret %s of a stale redis pod template: %v", ref.Name, err)
		return ""
	}
	return string(secret.Data[ref.Key])
}

// redisPasswordRef returns the Secret key of the last REDIS_PASSWORD in env,
// because the last one of a name wins.
func redisPasswordRef(env []v1.EnvVar) *v1.SecretKeySelector {
	var ref *v1.SecretKeySelector
	for _, e := range env {
		if e.Name == "REDIS_PASSWORD" {
			ref = nil
			if e.ValueFrom != nil {
				ref = e.ValueFrom.SecretKeyRef
			}
		}
	}
	return ref
}

func podRedisPasswordRef(containers []v1.Container) *v1.SecretKeySelector {
	for _, c := range containers {
		if c.Name == redisContainerName {
			return redisPasswordRef(c.Env)
		}
	}
	return nil
}

func sameSecretKey(a, b *v1.SecretKeySelector) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.Key == b.Key
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
