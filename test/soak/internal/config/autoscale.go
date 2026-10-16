package config

import (
	"errors"
	"fmt"
)

// Autoscale marks an instance whose template sets redis.autoscaling.mode
// Auto. The observer judges each step of the autoscaler on it. The ranges
// bound the autoscale kinds, in Mi.
type Autoscale struct {
	// Limit bounds the memory limit and request of autoscale_limit.
	Limit Range `json:"limit"`
	// MemoryMax bounds memoryMax of autoscale_memory_max.
	MemoryMax Range `json:"memoryMax"`
}

// memoryKinds change the memory outside the rules of mode Auto: Auto needs a
// request equal to the limit, and the load is sized for the percent of the
// template.
var memoryKinds = []Kind{RedisResources, RedisMemory, MaxMemoryPolicy, MaxMemoryPercent}

func (in Instance) validateAutoscale() error {
	a := in.Autoscale
	if a == nil {
		return nil
	}
	// The autoscaler gives a cache no step, and a load under an eviction
	// policy evicts the fill.
	if in.MaxMemoryPolicy != "noeviction" {
		return errors.New("an instance with autoscale needs maxMemoryPolicy noeviction")
	}
	if in.Bootstrap != nil {
		return errors.New("a bootstrapping instance has no master to sample")
	}
	m := in.Mutations.Kinds
	for _, k := range memoryKinds {
		if _, ok := m[k]; ok {
			return fmt.Errorf("%s changes the memory outside the rules of mode Auto", k)
		}
	}
	for _, k := range []Kind{AutoscaleLoad, AutoscaleKillMaster, AutoscaleKillReplica} {
		if _, ok := m[k]; ok && in.Data == nil {
			return fmt.Errorf("%s needs data", k)
		}
	}
	return validateAutoscaleRanges(a, m)
}

// validateAutoscaleRanges checks the ranges of the autoscale kinds in m.
func validateAutoscaleRanges(a *Autoscale, m map[Kind]int) error {
	_, limit := m[AutoscaleLimit]
	_, memoryMax := m[AutoscaleMemoryMax]
	if limit {
		if err := a.Limit.validate(minMemoryLimitMi); err != nil {
			return fmt.Errorf("limit: %w", err)
		}
	}
	if memoryMax {
		if err := a.MemoryMax.validate(minMemoryLimitMi); err != nil {
			return fmt.Errorf("memoryMax: %w", err)
		}
	}
	// Validation rejects a memoryMax below the limit.
	if limit && memoryMax && a.MemoryMax.Min < a.Limit.Max {
		return errors.New("memoryMax.min must not be less than limit.max")
	}
	return nil
}
