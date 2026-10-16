package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
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
	m := &maxMemoryRun{
		r:             r,
		rf:            rf,
		logger:        r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace),
		master:        master,
		redises:       redises,
		port:          getRedisPort(rf.Spec.Redis.Port),
		password:      password,
		policy:        mm.Policy,
		policyManaged: !rf.CustomConfigSets("maxmemory-policy"),
	}

	// Removing "replica-ignore-maxmemory no" from customConfig does not
	// reset it on running replicas, and bootstrap mode applies customConfig
	// afterwards. Validation only lets customConfig set it to yes.
	if err := r.setRedisConfigOn(rf, redises, m.port, password, "replica-ignore-maxmemory yes"); err != nil {
		return MaxMemoryResult{}, err
	}
	if rf.CustomConfigSets("maxmemory") {
		return MaxMemoryResult{}, m.setPolicy()
	}
	rp, err := r.redisMemoryLimits(rf)
	if err != nil {
		return MaxMemoryResult{}, err
	}
	m.downsizing = rp.downsizing
	if master == "" {
		return m.withoutMaster(rp)
	}

	if _, ok := rp.limits[master]; !ok {
		// A master below the managed minimum holds less than any pod it is
		// replaced with.
		return MaxMemoryResult{}, m.setPolicy()
	}
	if rp.smallest < redisfailoverv1.MinManagedMemoryLimit {
		// A replica below the managed minimum is replaced before the master.
		return MaxMemoryResult{}, m.setPolicy()
	}
	return m.onMaster(master, rp)
}

// maxMemoryRun holds one call of EnsureRedisMaxMemory. policy is the managed
// maxmemory-policy.
type maxMemoryRun struct {
	r                         *RedisFailoverHealer
	rf                        *redisfailoverv1.RedisFailover
	logger                    log.Logger
	master                    string
	redises                   []string
	port, password, policy    string
	policyManaged, downsizing bool
	// masterPolicy is the maxmemory-policy in force on the master: the one
	// read from it, or the managed one once the master accepted it.
	masterPolicy string
}

func (m *maxMemoryRun) setPolicy() error {
	if !m.policyManaged {
		return nil
	}
	return m.r.setRedisConfigEach(m.rf, m.redises, m.port, m.password, "maxmemory-policy "+m.policy, func(ip string) {
		if ip == m.master {
			m.masterPolicy = m.policy
		}
	})
}

// setMaxMemory sets maxmemory to bytes on ips. old and used are the values
// that the operator read from the master. The operator does not read the
// replicas, so it logs a replica only when bytes differs from the value read
// from the master, and then without a value of the replica.
func (m *maxMemoryRun) setMaxMemory(ips []string, old, bytes, used int64) error {
	return m.r.setRedisConfigEach(m.rf, ips, m.port, m.password, fmt.Sprintf("maxmemory %d", bytes), func(ip string) {
		if ip == m.master {
			logMaxMemoryChange(m.logger, ip, old, bytes, used, m.masterPolicy)
			return
		}
		logReplicaMaxMemory(m.logger, ip, old, bytes)
	})
}

func (m *maxMemoryRun) kept(msg string) MaxMemoryResult {
	return MaxMemoryResult{Message: msg, HoldRollout: m.downsizing}
}

func (m *maxMemoryRun) withoutMaster(rp redisPods) (MaxMemoryResult, error) {
	if err := m.setPolicy(); err != nil {
		return MaxMemoryResult{}, err
	}
	msg, err := m.r.ensureMaxMemoryWithoutMaster(m.rf, m.redises, rp.limits, m.port, m.password)
	if err != nil || msg == "" {
		return MaxMemoryResult{}, err
	}
	return m.kept(msg), nil
}

// onMaster applies maxmemory for the smallest pod, verified on the master.
func (m *maxMemoryRun) onMaster(master string, rp redisPods) (MaxMemoryResult, error) {
	target := m.rf.MaxMemoryFor(rp.smallest)
	current, err := m.r.redisClient.GetMemoryInfo(master, m.port, m.password)
	if err != nil {
		return MaxMemoryResult{}, err
	}
	if current.Loading {
		// E.g. a restarted master, which would seem to fit a lower value.
		return MaxMemoryResult{HoldRollout: m.downsizing}, m.setPolicy()
	}
	m.masterPolicy = current.MaxMemoryPolicy
	policy := current.MaxMemoryPolicy
	if m.policyManaged {
		policy = m.policy
	}
	evicts := evictsEnough(policy)
	if !lowers(current.MaxMemory, target) {
		// Before the policy, so a new evicting policy does not evict what
		// fits under the raised value.
		if err := m.setMaxMemory(m.redises, current.MaxMemory, target, current.UsedMemory); err != nil {
			return MaxMemoryResult{}, err
		}
		if evicts && m.downsizing && current.UsedMemory > target {
			return m.kept(evictingMessage(target)), m.setPolicy()
		}
		return MaxMemoryResult{}, m.setPolicy()
	}
	if err := m.setPolicy(); err != nil {
		return MaxMemoryResult{}, err
	}

	others := withoutIP(m.redises, master)
	if current.UsedMemory > target && !evicts {
		old := current.MaxMemory
		if current.MaxMemory == 0 {
			// E.g. a restarted master: bounded by the smallest running
			// container's reserve, or the memory in use.
			current.MaxMemory = max(current.UsedMemory, rp.smallestRunning-redisfailoverv1.MaxMemoryReserve)
			if err := m.setMaxMemory([]string{master}, old, current.MaxMemory, current.UsedMemory); err != nil {
				return MaxMemoryResult{}, err
			}
		}
		// Replicas follow the kept value, so a promoted one behaves the same.
		return m.kept(notFitMessage(current.MaxMemory, target, policy)), m.setMaxMemory(others, old, current.MaxMemory, current.UsedMemory)
	}
	return m.lowerOnMaster(master, others, current, target, policy)
}

// lowerOnMaster lowers maxmemory on the master to target, and on the others
// once it fits. It restores the value previous when it does not fit.
func (m *maxMemoryRun) lowerOnMaster(master string, others []string, current *redis.MemoryInfo, target int64, policy string) (MaxMemoryResult, error) {
	previous, used := current.MaxMemory, current.UsedMemory
	evicts := evictsEnough(policy)
	// Usage can grow or a failover happen meanwhile.
	wait := time.Duration(0)
	if evicts {
		wait = evictionWait
	}
	usedBefore := used
	logLowered := func() { logMaxMemoryChange(m.logger, master, previous, target, usedBefore, m.masterPolicy) }
	after, lowered, err := m.r.lowerAndWait(master, m.port, m.password, target, wait, logLowered)
	if after != nil {
		used = after.UsedMemory
	}
	if err == nil && after.Role == "master" && (evicts || after.UsedMemory <= target) {
		if err := m.setMaxMemory(others, previous, target, used); err != nil || after.UsedMemory <= target {
			return MaxMemoryResult{}, err
		}
		// Redis keeps evicting in the background, which can take longer for
		// a large dataset.
		return m.kept(evictingMessage(target)), nil
	}
	rerr := m.r.redisClient.SetCustomRedisConfig(master, m.port, []string{fmt.Sprintf("maxmemory %d", previous)}, m.password)
	if rerr == nil && lowered {
		logMaxMemoryChange(m.logger, master, target, previous, used, m.masterPolicy)
	}
	if rerr != nil || err != nil {
		return MaxMemoryResult{}, errors.Join(err, rerr)
	}
	if after.Role != "master" {
		// The new master is not known here, and capping it by its own
		// container would lower it without checking its usage.
		return m.kept(fmt.Sprintf("maxmemory kept at %s: %s stopped being the master while lowering it", formatBytes(previous), master)), nil
	}
	return m.kept(notFitMessage(previous, target, policy)), m.setMaxMemory(others, previous, previous, used)
}

// logMaxMemoryChange logs a change of the managed maxmemory on ip at Info, so
// that an OOM refusal of a write can be traced to the value. An unchanged
// value logs nothing, because the operator sets it at each reconcile.
func logMaxMemoryChange(logger log.Logger, ip string, old, value, used int64, policy string) {
	if old == value {
		return
	}
	logger.Infof("Set maxmemory %s -> %s on %s (used %s, policy %s)", formatBytes(old), formatBytes(value), ip, formatMi(used), policy)
}

// logReplicaMaxMemory logs the value that a replica accepted when it
// differs from old, the value read from the master. A replica can be set
// before the master, so the line does not claim a change of the master.
func logReplicaMaxMemory(logger log.Logger, ip string, old, value int64) {
	if old == value {
		return
	}
	logger.Infof("Set maxmemory %s on %s (replica, the master had %s)", formatBytes(value), ip, formatBytes(old))
}

// evictsEnough reports whether policy surely evicts enough. Only allkeys-*
// does: volatile-* could evict every key with a TTL and still not fit, which
// restoring maxmemory cannot undo.
func evictsEnough(policy string) bool {
	return strings.HasPrefix(policy, "allkeys-")
}

// evictingMessage returns the message of a lowered maxmemory. It has no
// memory in use, which would change the status on every reconcile and
// trigger the next one.
func evictingMessage(target int64) string {
	return fmt.Sprintf("maxmemory lowered to %s, evicting down to it", formatBytes(target))
}

func notFitMessage(current, target int64, policy string) string {
	return fmt.Sprintf("maxmemory kept at %s: lowering it to %s would not fit the memory in use under policy %s",
		formatBytes(current), formatBytes(target), policy)
}

// withoutIP returns ips without ip.
func withoutIP(ips []string, ip string) []string {
	others := make([]string, 0, len(ips))
	for _, other := range ips {
		if other != ip {
			others = append(others, other)
		}
	}
	return others
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
		if mi.Loading {
			continue
		}
		if lowers(mi.MaxMemory, target) && mi.UsedMemory > target {
			msg = fmt.Sprintf("maxmemory kept at %s: lowering it to %s would not fit the memory in use on %s",
				formatBytes(mi.MaxMemory), formatBytes(target), ip)
			continue
		}
		logger := r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
		if err := r.setRedisConfigEach(rf, []string{ip}, port, password, fmt.Sprintf("maxmemory %d", target), func(ip string) {
			logMaxMemoryChange(logger, ip, mi.MaxMemory, target, mi.UsedMemory, mi.MaxMemoryPolicy)
		}); err != nil {
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
		podLimits := redisContainerLimits(&pod)
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
		rp.add(pod.Status.PodIP, limit, running)
	}
	return rp, nil
}

// redisContainerLimits returns the limits of the redis container, preferring
// the limits that the kubelet reports as applied over the pod spec.
func redisContainerLimits(pod *corev1.Pod) corev1.ResourceList {
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
	return podLimits
}

func (rp *redisPods) add(ip string, limit, running int64) {
	if limit >= redisfailoverv1.MinManagedMemoryLimit {
		rp.limits[ip] = limit
	}
	if rp.smallest == 0 || limit < rp.smallest {
		rp.smallest = limit
	}
	if rp.smallestRunning == 0 || running < rp.smallestRunning {
		rp.smallestRunning = running
	}
}

// setRedisConfigOn applies one config to the given redises, skipping unreachable ones.
func (r *RedisFailoverHealer) setRedisConfigOn(rf *redisfailoverv1.RedisFailover, ips []string, port, password, config string) error {
	return r.setRedisConfigEach(rf, ips, port, password, config, nil)
}

// setRedisConfigEach is setRedisConfigOn, and it calls applied, when not
// nil, for each redis that accepted the config.
func (r *RedisFailoverHealer) setRedisConfigEach(rf *redisfailoverv1.RedisFailover, ips []string, port, password, config string, applied func(ip string)) error {
	for _, ip := range ips {
		if err := r.redisClient.SetCustomRedisConfig(ip, port, []string{config}, password); err != nil {
			if redis.IsUnreachableError(err) {
				r.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace).Warningf("Skipping %s on unreachable redis %s: %s", config, ip, err.Error())
				continue
			}
			return err
		}
		if applied != nil {
			applied(ip)
		}
	}
	return nil
}

// lowerAndWait sets maxmemory on the master and returns its memory info once
// the memory in use fits, or after wait has passed. The bool reports whether
// the master accepted the value. Then lowerAndWait calls set, which logs the
// change before the reads.
func (r *RedisFailoverHealer) lowerAndWait(master, port, password string, target int64, wait time.Duration, set func()) (*redis.MemoryInfo, bool, error) {
	if err := r.redisClient.SetCustomRedisConfig(master, port, []string{fmt.Sprintf("maxmemory %d", target)}, password); err != nil {
		return nil, false, err
	}
	set()
	deadline := time.Now().Add(wait)
	for {
		mi, err := r.redisClient.GetMemoryInfo(master, port, password)
		if err != nil {
			return nil, true, err
		}
		if mi.UsedMemory <= target || !time.Now().Before(deadline) {
			return mi, true, nil
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
	return formatMi(b)
}

func formatMi(b int64) string {
	return fmt.Sprintf("%.1fMi", float64(b)/(1<<20))
}
