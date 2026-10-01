package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Kind is a kind of mutation, the value of the kind label.
type Kind string

const (
	RedisReplicas    Kind = "redis_replicas"
	SentinelReplicas Kind = "sentinel_replicas"
	RedisResources   Kind = "redis_resources"
	KillMaster       Kind = "kill_master"
	KillReplica      Kind = "kill_replica"
	KillSentinel     Kind = "kill_sentinel"
)

var kinds = []Kind{RedisReplicas, SentinelReplicas, RedisResources, KillMaster, KillReplica, KillSentinel}

func sentinelOnly(k Kind) bool {
	return k == SentinelReplicas || k == KillSentinel
}

// Mutations is an instance's mutation catalogue.
type Mutations struct {
	// Kinds are the enabled kinds and their weights.
	Kinds            map[Kind]int `json:"kinds"`
	RedisReplicas    Range        `json:"redisReplicas"`
	SentinelReplicas Range        `json:"sentinelReplicas"`
	Resources        Resources    `json:"resources"`
	// ForceDeleteProbability is the share of pod kills that delete the pod
	// with GracePeriodSeconds=0 instead of gracefully.
	ForceDeleteProbability float64 `json:"forceDeleteProbability"`
}

// Range is an inclusive range.
type Range struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

func (r Range) Set() bool { return r != Range{} }

// Resources bounds the redis container's requests and limits: cpu in
// millicores, memory in Mi. Only the values the RedisFailover already sets
// are changed, so the set of requests and limits stays the same.
type Resources struct {
	Requests ResourceRanges `json:"requests"`
	Limits   ResourceRanges `json:"limits"`
}

type ResourceRanges struct {
	CPU    Range `json:"cpu"`
	Memory Range `json:"memory"`
}

// minMemoryLimitMi is the smallest memory limit the operator derives
// maxmemory from.
const minMemoryLimitMi = 64

// Sorted returns the enabled kinds in a fixed order.
func (m Mutations) Sorted() []Kind {
	return slices.Sorted(maps.Keys(m.Kinds))
}

func (m Mutations) validate(mode Mode) error {
	for k, w := range m.Kinds {
		if !slices.Contains(kinds, k) {
			return fmt.Errorf("unknown kind %q", k)
		}
		if w < 1 {
			return fmt.Errorf("%s: weight must be at least 1", k)
		}
		if sentinelOnly(k) && mode != ModeSentinel {
			return fmt.Errorf("%s needs a sentinel instance", k)
		}
	}
	if m.ForceDeleteProbability < 0 || m.ForceDeleteProbability > 1 {
		return errors.New("forceDeleteProbability must be between 0 and 1")
	}
	if _, ok := m.Kinds[RedisReplicas]; ok {
		if err := m.RedisReplicas.validate(1); err != nil {
			return fmt.Errorf("redisReplicas: %w", err)
		}
	}
	if _, ok := m.Kinds[SentinelReplicas]; ok {
		if err := m.SentinelReplicas.validate(3); err != nil {
			return fmt.Errorf("sentinelReplicas: %w", err)
		}
	}
	if _, ok := m.Kinds[RedisResources]; ok {
		if err := m.Resources.validate(); err != nil {
			return fmt.Errorf("resources: %w", err)
		}
	}
	return nil
}

// validate checks that the range holds at least two values, so a mutation
// can always pick one different from the current.
func (r Range) validate(least int64) error {
	if r.Min < least {
		return fmt.Errorf("min must be at least %d", least)
	}
	if r.Max <= r.Min {
		return errors.New("max must be greater than min")
	}
	return nil
}

func (r Resources) validate() error {
	ranges := map[string]Range{
		"requests.cpu": r.Requests.CPU, "requests.memory": r.Requests.Memory,
		"limits.cpu": r.Limits.CPU, "limits.memory": r.Limits.Memory,
	}
	set := 0
	for _, name := range slices.Sorted(maps.Keys(ranges)) {
		if rg := ranges[name]; rg.Set() {
			set++
			if err := rg.validate(1); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if set == 0 {
		return errors.New("no bounds")
	}
	if r.Limits.Memory.Set() && r.Limits.Memory.Min < minMemoryLimitMi {
		return fmt.Errorf("limits.memory: min must be at least %dMi", minMemoryLimitMi)
	}
	// A request at its limit could turn a Burstable pod Guaranteed, which
	// the operator can't resize in place, and one above it is invalid.
	for name, rr := range map[string][2]Range{
		"cpu":    {r.Requests.CPU, r.Limits.CPU},
		"memory": {r.Requests.Memory, r.Limits.Memory},
	} {
		if rr[0].Set() && rr[1].Set() && rr[0].Max >= rr[1].Min {
			return fmt.Errorf("requests.%s must stay below limits.%s, which would change the QoS class", name, name)
		}
	}
	return nil
}
