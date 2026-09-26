package v1

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// MaxMemoryReserve is the minimum headroom below the limit, which matters
	// more than Percent for small limits.
	MaxMemoryReserve = 32 << 20
	// MinManagedMemoryLimit is the smallest memory limit managed mode accepts.
	MinManagedMemoryLimit = 64 << 20
)

var maxMemoryPolicies = []string{
	"noeviction",
	"allkeys-lru", "allkeys-lfu", "allkeys-random",
	"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl",
}

// MaxMemoryFor returns the managed maxmemory for a memory limit, or 0 when
// maxmemory is not managed.
func (r *RedisFailover) MaxMemoryFor(limit int64) int64 {
	mm := r.Spec.Redis.MaxMemory
	if mm == nil {
		return 0
	}
	target := limit * int64(mm.Percent) / 100
	if capped := limit - MaxMemoryReserve; capped < target {
		target = capped
	}
	return target
}

// CustomConfigSets reports whether redis customConfig sets the given key. It
// parses entries like SetCustomRedisConfig, which skips e.g. a leading space.
func (r *RedisFailover) CustomConfigSets(key string) bool {
	for _, c := range r.Spec.Redis.CustomConfig {
		if param, _, _ := strings.Cut(c, " "); strings.EqualFold(param, key) {
			return true
		}
	}
	return false
}

func (r *RedisFailover) validateMaxMemory() error {
	mm := r.Spec.Redis.MaxMemory
	if mm == nil {
		return nil
	}
	if mm.Percent == 0 {
		mm.Percent = defaultMaxMemoryPercent
	}
	if mm.Percent < 10 || mm.Percent > 95 {
		return fmt.Errorf("redis.maxMemory.percent %d must be between 10 and 95", mm.Percent)
	}
	if mm.Policy == "" {
		mm.Policy = defaultMaxMemoryPolicy
	}
	valid := false
	for _, p := range maxMemoryPolicies {
		valid = valid || mm.Policy == p
	}
	if !valid {
		return fmt.Errorf("redis.maxMemory.policy %q must be one of %s", mm.Policy, strings.Join(maxMemoryPolicies, ", "))
	}
	// Replicas enforcing maxmemory would evict on their own and diverge from
	// the master. Fatal, as skipping management would leave the managed
	// values on the replicas while customConfig enables enforcing them.
	for _, c := range r.Spec.Redis.CustomConfig {
		if param, value, _ := strings.Cut(c, " "); (strings.EqualFold(param, "replica-ignore-maxmemory") || strings.EqualFold(param, "slave-ignore-maxmemory")) && strings.EqualFold(strings.TrimSpace(value), "no") {
			return fmt.Errorf("redis.maxMemory does not support %q in customConfig", c)
		}
	}
	return nil
}

// ManagedMaxMemoryError reports why maxmemory cannot be managed. Unlike
// Validate errors, which stop the whole reconcile, it only disables managed
// maxmemory: the API server accepts these specs.
func (r *RedisFailover) ManagedMaxMemoryError() error {
	limit, ok := r.Spec.Redis.Resources.Limits[corev1.ResourceMemory]
	if !ok {
		return fmt.Errorf("redis.maxMemory requires redis.resources.limits.memory")
	}
	if limit.Value() < MinManagedMemoryLimit {
		return fmt.Errorf("redis.maxMemory requires redis.resources.limits.memory of at least 64Mi, got %s", limit.String())
	}
	return nil
}
