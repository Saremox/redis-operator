package v1

import (
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The values of redis.autoscaling.mode and redis.autoscaling.workload.
const (
	// AutoscalingModeOff disables the autoscaler.
	AutoscalingModeOff = "Off"
	// AutoscalingModeRecommend shows a memory limit in the status and
	// changes no pod.
	AutoscalingModeRecommend = "Recommend"
	// AutoscalingModeAuto also raises the memory limit of the pods in place.
	AutoscalingModeAuto = "Auto"

	// AutoscalingWorkloadAuto derives Store or Cache from the
	// maxmemory-policy.
	AutoscalingWorkloadAuto = "Auto"
	// AutoscalingWorkloadStore is a Redis whose clients need all the keys.
	AutoscalingWorkloadStore = "Store"
	// AutoscalingWorkloadCache is a Redis whose clients accept the loss of
	// keys.
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
	if err := validateAutoscalingSettings(a); err != nil {
		return err
	}
	if a.Mode == AutoscalingModeOff {
		return nil
	}
	return r.validateAutoscalingMemory(a)
}

// validateAutoscalingSettings sets the default mode and workload and checks
// the fields that each mode accepts.
func validateAutoscalingSettings(a *AutoscalingSettings) error {
	if a.Mode == "" {
		a.Mode = AutoscalingModeOff
	}
	if a.Workload == "" {
		a.Workload = AutoscalingWorkloadAuto
	}
	switch a.Mode {
	case AutoscalingModeOff, AutoscalingModeRecommend, AutoscalingModeAuto:
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
	return nil
}

// validateAutoscalingMemory checks the memory rules of the modes Recommend
// and Auto.
func (r *RedisFailover) validateAutoscalingMemory(a *AutoscalingSettings) error {
	if r.Bootstrapping() {
		return fmt.Errorf("redis.autoscaling.mode %s is not available with bootstrapNode, because the RedisFailover has no master to sample", a.Mode)
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
	if a.Mode == AutoscalingModeAuto {
		if err := r.validateAutoscalingAuto(limit); err != nil {
			return err
		}
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

// validateAutoscalingAuto checks the rules of a step. The operator resizes the
// pods in place and never recreates them for a step, and maxmemory must
// follow the limit. Otherwise the memory use stays high after a step, and the
// steps continue up to memoryMax.
func (r *RedisFailover) validateAutoscalingAuto(limit resource.Quantity) error {
	redis := r.Spec.Redis
	switch {
	case redis.MaxMemory == nil:
		return errors.New("redis.autoscaling.mode Auto requires redis.maxMemory")
	case r.ManagedMaxMemoryError() != nil:
		return fmt.Errorf("redis.autoscaling.mode Auto requires a managed maxmemory: %w", r.ManagedMaxMemoryError())
	case r.CustomConfigSets("maxmemory"):
		return errors.New("redis.autoscaling.mode Auto requires no maxmemory in redis.customConfig")
	case redis.InPlaceResize == "Disabled":
		return errors.New("redis.autoscaling.mode Auto requires redis.inPlaceResize Enabled, because a step resizes the pods in place")
	case redis.Autoscaling.RecreateAfter != nil:
		return errors.New("redis.autoscaling.recreateAfter is not available with mode Auto, because a step never recreates a pod")
	}
	// This applies to each workload. A step raises the request with the
	// limit only when they are equal. A request below the limit lets the node
	// run out of memory first.
	if request, ok := redis.Resources.Requests[corev1.ResourceMemory]; ok && request.Cmp(limit) != 0 {
		return fmt.Errorf("redis.autoscaling.mode Auto requires redis.resources.requests.memory %s equal to the limit %s", request.String(), limit.String())
	}
	return nil
}
