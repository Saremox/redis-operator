package v1

import (
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The values of redis.autoscaling.mode and redis.autoscaling.workload.
const (
	AutoscalingModeOff       = "Off"
	AutoscalingModeRecommend = "Recommend"
	AutoscalingModeAuto      = "Auto"

	AutoscalingWorkloadAuto  = "Auto"
	AutoscalingWorkloadStore = "Store"
	AutoscalingWorkloadCache = "Cache"
)

// minRecreateAfter is the in-place resize timeout. A shorter wait would
// recreate a pod while its resize can still complete.
const minRecreateAfter = 5 * time.Minute

// AutoscalingEnabled reports whether redis.autoscaling has a mode other than Off.
func (r *RedisFailover) AutoscalingEnabled() bool {
	a := r.Spec.Redis.Autoscaling
	return a != nil && a.Mode != "" && a.Mode != AutoscalingModeOff
}

// MaxMemoryPolicy returns the maxmemory-policy that the spec sets. As in
// EnsureRedisMaxMemory, customConfig has priority over maxMemory.policy.
// Without both, it is the Redis default noeviction.
func (r *RedisFailover) MaxMemoryPolicy() string {
	for _, c := range r.Spec.Redis.CustomConfig {
		if param, value, _ := strings.Cut(c, " "); strings.EqualFold(param, "maxmemory-policy") {
			return strings.ToLower(strings.TrimSpace(value))
		}
	}
	if mm := r.Spec.Redis.MaxMemory; mm != nil && mm.Policy != "" {
		return mm.Policy
	}
	return defaultMaxMemoryPolicy
}

func (r *RedisFailover) validateAutoscaling() error {
	a := r.Spec.Redis.Autoscaling
	if a == nil {
		return nil
	}
	if a.Mode == "" {
		a.Mode = AutoscalingModeOff
	}
	if a.Workload == "" {
		a.Workload = AutoscalingWorkloadAuto
	}
	switch a.Mode {
	case AutoscalingModeOff, AutoscalingModeRecommend:
	case AutoscalingModeAuto:
		return errors.New("redis.autoscaling.mode Auto is not available, use Off or Recommend")
	default:
		return fmt.Errorf("redis.autoscaling.mode %q must be Off, Recommend or Auto", a.Mode)
	}
	switch a.Workload {
	case AutoscalingWorkloadAuto, AutoscalingWorkloadStore, AutoscalingWorkloadCache:
	default:
		return fmt.Errorf("redis.autoscaling.workload %q must be Auto, Store or Cache", a.Workload)
	}
	if ra := a.RecreateAfter; ra != nil && ra.Duration != 0 && ra.Duration < minRecreateAfter {
		return fmt.Errorf("redis.autoscaling.recreateAfter %s must be 0s or at least %s", ra.Duration, minRecreateAfter)
	}
	if a.Mode == AutoscalingModeOff {
		return nil
	}
	limit, ok := r.Spec.Redis.Resources.Limits[corev1.ResourceMemory]
	if !ok {
		return errors.New("redis.autoscaling requires redis.resources.limits.memory")
	}
	if a.MemoryMax == nil {
		return fmt.Errorf("redis.autoscaling.memoryMax is required for mode %s", a.Mode)
	}
	// Equal is valid: the user can apply a recommendation of memoryMax.
	if a.MemoryMax.Cmp(limit) < 0 {
		return fmt.Errorf("redis.autoscaling.memoryMax %s must not be less than redis.resources.limits.memory %s", a.MemoryMax.String(), limit.String())
	}
	// A request below the limit lets the node run out of memory before the
	// pod reaches its limit, and a store then loses its data.
	store := a.Workload == AutoscalingWorkloadStore || (a.Workload == AutoscalingWorkloadAuto && !strings.HasPrefix(r.MaxMemoryPolicy(), "allkeys-"))
	request, ok := r.Spec.Redis.Resources.Requests[corev1.ResourceMemory]
	if ok && store && request.Cmp(limit) != 0 {
		return fmt.Errorf("redis.autoscaling requires redis.resources.requests.memory %s equal to the limit %s for a store", request.String(), limit.String())
	}
	return nil
}
