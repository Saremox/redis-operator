package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

var (
	evictionWait         = 5 * time.Second
	evictionPollInterval = 250 * time.Millisecond
)

// MaxMemoryResult is the outcome of EnsureRedisMaxMemory.
type MaxMemoryResult struct {
	// Message explains why maxmemory, or the memory in use, is above its
	// target.
	Message string
	// HoldRollout is set when a lowered memory limit is pending but the
	// master's maxmemory or memory in use does not fit it yet. Replicas are
	// replaced first and a full sync ignores maxmemory, so they would be
	// OOM-killed.
	HoldRollout bool
}

// EnsureRedisMaxMemory applies the managed maxmemory-policy and maxmemory,
// except for keys set in customConfig. master is "" when bootstrapping.
//
// The target derives from the smallest pod's limit, capped by the configured
// one: replicas ignore maxmemory and hold the whole dataset, and a failover can
// promote any of them. With OnDelete, a raised limit therefore applies once
// every pod runs with it, a lowered one before smaller pods are rolled out.
// Lowering below the memory in use needs an allkeys-* policy, which evicts down
// to it. Otherwise it is verified on the master and rolled back if it does not
// fit, because a lowered value left in place would pass as applied on the next
// reconcile.
func (r *RedisFailoverHealer) EnsureRedisMaxMemory(rf *redisfailoverv1.RedisFailover, master string, redises []string) (MaxMemoryResult, error) {
	mm := rf.Spec.Redis.MaxMemory
	if mm == nil {
		return MaxMemoryResult{}, nil
	}
	if err := rf.ManagedMaxMemoryError(); err != nil {
		return MaxMemoryResult{Message: "maxmemory not managed: " + err.Error()}, nil
	}
	password, err := k8s.GetRedisPassword(r.k8sService, rf)
	if err != nil {
		return MaxMemoryResult{}, err
	}
	port := getRedisPort(rf.Spec.Redis.Port)

	policyManaged := !rf.CustomConfigSets("maxmemory-policy")
	setPolicy := func() error {
		if !policyManaged {
			return nil
		}
		return r.setRedisConfigOn(rf, redises, port, password, "maxmemory-policy "+mm.Policy)
	}

	// Removing "replica-ignore-maxmemory no" from customConfig does not
	// reset it on running replicas, and bootstrap mode applies customConfig
	// afterwards. Validation only lets customConfig set it to yes.
	if err := r.setRedisConfigOn(rf, redises, port, password, "replica-ignore-maxmemory yes"); err != nil {
		return MaxMemoryResult{}, err
	}
	if rf.CustomConfigSets("maxmemory") {
		return MaxMemoryResult{}, setPolicy()
	}
	rp, err := r.redisMemoryLimits(rf)
	if err != nil {
		return MaxMemoryResult{}, err
	}
	downsizing := rp.downsizing
	kept := func(msg string) MaxMemoryResult {
		return MaxMemoryResult{Message: msg, HoldRollout: downsizing}
	}
	if master == "" {
		if err := setPolicy(); err != nil {
			return MaxMemoryResult{}, err
		}
		msg, err := r.ensureMaxMemoryWithoutMaster(rf, redises, rp.limits, port, password)
		if err != nil || msg == "" {
			return MaxMemoryResult{}, err
		}
		return kept(msg), nil
	}

	if _, ok := rp.limits[master]; !ok {
		// A master below the managed minimum holds less than any pod it is
		// replaced with.
		return MaxMemoryResult{}, setPolicy()
	}
	if rp.smallest < redisfailoverv1.MinManagedMemoryLimit {
		// A replica below the managed minimum is replaced before the master.
		return MaxMemoryResult{}, setPolicy()
	}
	target := rf.MaxMemoryFor(rp.smallest)
	setMaxMemory := func(ips []string, bytes int64) error {
		return r.setRedisConfigOn(rf, ips, port, password, fmt.Sprintf("maxmemory %d", bytes))
	}
	current, err := r.redisClient.GetMemoryInfo(master, port, password)
	if err != nil {
		return MaxMemoryResult{}, err
	}
	policy := current.MaxMemoryPolicy
	if policyManaged {
		policy = mm.Policy
	}
	// Only allkeys-* surely evicts enough: volatile-* could evict every key
	// with a TTL and still not fit, which restoring maxmemory cannot undo.
	evicts := strings.HasPrefix(policy, "allkeys-")
	// Without the memory in use, which would change the status on every
	// reconcile and trigger the next one.
	evicting := fmt.Sprintf("maxmemory lowered to %s, evicting down to it", formatBytes(target))

	if !lowers(current.MaxMemory, target) {
		// Before the policy, so a new evicting policy does not evict what
		// fits under the raised value.
		if err := setMaxMemory(redises, target); err != nil {
			return MaxMemoryResult{}, err
		}
		if evicts && current.UsedMemory > target {
			return kept(evicting), setPolicy()
		}
		return MaxMemoryResult{}, setPolicy()
	}
	if err := setPolicy(); err != nil {
		return MaxMemoryResult{}, err
	}

	others := make([]string, 0, len(redises))
	for _, ip := range redises {
		if ip != master {
			others = append(others, ip)
		}
	}
	notFit := func() string {
		return fmt.Sprintf("maxmemory kept at %s: lowering it to %s would not fit the memory in use under policy %s",
			formatBytes(current.MaxMemory), formatBytes(target), policy)
	}

	if current.UsedMemory > target && !evicts {
		if current.MaxMemory == 0 {
			// E.g. a restarted master: bounded by the smallest running
			// container's reserve, or the memory in use.
			current.MaxMemory = max(current.UsedMemory, rp.smallestRunning-redisfailoverv1.MaxMemoryReserve)
			if err := r.setRedisConfigOn(rf, []string{master}, port, password, fmt.Sprintf("maxmemory %d", current.MaxMemory)); err != nil {
				return MaxMemoryResult{}, err
			}
		}
		// Replicas follow the kept value, so a promoted one behaves the same.
		return kept(notFit()), setMaxMemory(others, current.MaxMemory)
	}

	// Usage can grow or a failover happen meanwhile.
	wait := time.Duration(0)
	if evicts {
		wait = evictionWait
	}
	after, err := r.lowerAndWait(master, port, password, target, wait)
	if err == nil && after.Role == "master" && (evicts || after.UsedMemory <= target) {
		if err := setMaxMemory(others, target); err != nil || after.UsedMemory <= target {
			return MaxMemoryResult{}, err
		}
		// Redis keeps evicting in the background, which can take longer for
		// a large dataset.
		return kept(evicting), nil
	}
	if rerr := r.redisClient.SetCustomRedisConfig(master, port, []string{fmt.Sprintf("maxmemory %d", current.MaxMemory)}, password); rerr != nil || err != nil {
		return MaxMemoryResult{}, errors.Join(err, rerr)
	}
	if after.Role != "master" {
		// The new master is not known here, and capping it by its own
		// container would lower it without checking its usage.
		return kept(fmt.Sprintf("maxmemory kept at %s: %s stopped being the master while lowering it", formatBytes(current.MaxMemory), master)), nil
	}
	return kept(notFit()), setMaxMemory(others, current.MaxMemory)
}

// ensureMaxMemoryWithoutMaster handles bootstrapping, where every pod is a
// replica. Replicas do not evict, so each one is only lowered when it fits.
func (r *RedisFailoverHealer) ensureMaxMemoryWithoutMaster(rf *redisfailoverv1.RedisFailover, redises []string, limits map[string]int64, port, password string) (string, error) {
	msg := ""
	for _, ip := range redises {
		limit, ok := limits[ip]
		if !ok {
			continue
		}
		target := rf.MaxMemoryFor(limit)
		mi, err := r.redisClient.GetMemoryInfo(ip, port, password)
		if err != nil {
			if redis.IsUnreachableError(err) {
				continue
			}
			return "", err
		}
		if lowers(mi.MaxMemory, target) && mi.UsedMemory > target {
			msg = fmt.Sprintf("maxmemory kept at %s: lowering it to %s would not fit the memory in use on %s",
				formatBytes(mi.MaxMemory), formatBytes(target), ip)
			continue
		}
		if err := r.setRedisConfigOn(rf, []string{ip}, port, password, fmt.Sprintf("maxmemory %d", target)); err != nil {
			return "", err
		}
	}
	return msg, nil
}

// redisPods describes the redis pods by IP.
type redisPods struct {
	// limits holds the smaller of the configured and the running redis
	// container's limit, preferring the limit the kubelet reports as applied
	// over the pod spec. A pod without a limit counts as having the configured
	// one, which it gets on replacement; pods below MinManagedMemoryLimit are
	// left out until replaced.
	limits map[string]int64
	// smallest is the smallest limit, including pods left out of limits;
	// smallestRunning the same without capping by the configured limit.
	smallest, smallestRunning int64
	// downsizing reports whether a stale pod is about to be replaced with a
	// smaller limit.
	downsizing bool
}

func (r *RedisFailoverHealer) redisMemoryLimits(rf *redisfailoverv1.RedisFailover) (redisPods, error) {
	pods, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return redisPods{}, err
	}
	ss, err := r.k8sService.GetStatefulSet(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return redisPods{}, err
	}
	spec := rf.Spec.Redis.Resources.Limits.Memory().Value()
	rp := redisPods{limits: map[string]int64{}}
	for _, pod := range pods.Items {
		if pod.Status.PodIP == "" {
			continue
		}
		var podLimits corev1.ResourceList
		for _, c := range pod.Spec.Containers {
			if c.Name == redisContainerName {
				podLimits = c.Resources.Limits
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == redisContainerName && cs.Resources != nil {
				podLimits = cs.Resources.Limits
			}
		}
		limit, running := spec, spec
		if memory, ok := podLimits[corev1.ResourceMemory]; ok {
			running = memory.Value()
		}
		// A larger container that is up to date, e.g. resized by a VPA, is
		// not replaced. The update revision may still be the previous one
		// until the controller observes the latest spec.
		stale := ss.Status.ObservedGeneration < ss.Generation || pod.Labels[appsv1.ControllerRevisionHashLabelKey] != ss.Status.UpdateRevision
		if memory, ok := podLimits[corev1.ResourceMemory]; ok && memory.Value() < spec {
			limit = memory.Value()
		} else if stale && (!ok || memory.Value() > spec) {
			rp.downsizing = true
		}
		if limit >= redisfailoverv1.MinManagedMemoryLimit {
			rp.limits[pod.Status.PodIP] = limit
		}
		if rp.smallest == 0 || limit < rp.smallest {
			rp.smallest = limit
		}
		if rp.smallestRunning == 0 || running < rp.smallestRunning {
			rp.smallestRunning = running
		}
	}
	return rp, nil
}

// setRedisConfigOn applies one config to the given redises, skipping unreachable ones.
func (r *RedisFailoverHealer) setRedisConfigOn(rf *redisfailoverv1.RedisFailover, ips []string, port, password, config string) error {
	for _, ip := range ips {
		if err := r.redisClient.SetCustomRedisConfig(ip, port, []string{config}, password); err != nil {
			if redis.IsUnreachableError(err) {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Warningf("Skipping %s on unreachable redis %s: %s", config, ip, err.Error())
				continue
			}
			return err
		}
	}
	return nil
}

// lowerAndWait sets maxmemory on the master and returns its memory info once
// the memory in use fits, or after wait has passed.
func (r *RedisFailoverHealer) lowerAndWait(master, port, password string, target int64, wait time.Duration) (*redis.MemoryInfo, error) {
	if err := r.redisClient.SetCustomRedisConfig(master, port, []string{fmt.Sprintf("maxmemory %d", target)}, password); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		mi, err := r.redisClient.GetMemoryInfo(master, port, password)
		if err != nil {
			return nil, err
		}
		if mi.UsedMemory <= target || !time.Now().Before(deadline) {
			return mi, nil
		}
		time.Sleep(evictionPollInterval)
	}
}

// lowers reports whether setting maxmemory to target lowers it; 0 is unlimited.
func lowers(current, target int64) bool {
	return current == 0 || target < current
}

func formatBytes(b int64) string {
	if b == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%.1fMi", float64(b)/(1<<20))
}
