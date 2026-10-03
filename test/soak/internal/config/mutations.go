package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Kind is a kind of mutation, the value of the kind label.
type Kind string

const (
	RedisReplicas    Kind = "redis_replicas"
	SentinelReplicas Kind = "sentinel_replicas"
	RedisResources   Kind = "redis_resources"
	KillMaster       Kind = "kill_master"
	KillMasterForce  Kind = "kill_master_force"
	KillReplica      Kind = "kill_replica"
	KillSentinel     Kind = "kill_sentinel"
	RedisMemory      Kind = "redis_memory"
	MaxMemoryPolicy  Kind = "maxmemory_policy"
	MaxMemoryPercent Kind = "maxmemory_percent"
	FillBurst        Kind = "fill_burst"
	PasswordRotate   Kind = "password_rotate"
	AuthAdd          Kind = "auth_add"
	AuthRemove       Kind = "auth_remove"
	SentinelToggle   Kind = "sentinel_toggle"
	// PasswordRotateOffline is scenario C: the password changes while the
	// operator is stopped.
	PasswordRotateOffline Kind = "password_rotate_offline"
	// ImageUpgrade follows an edge of the transition graph with the redis
	// image, and the Sentinel image where it follows; at the end of the
	// chain it resets instead.
	ImageUpgrade Kind = "image_upgrade"
	// SentinelImageUpgrade follows an edge with the Sentinel image, towards
	// the redis image's version.
	SentinelImageUpgrade Kind = "sentinel_image_upgrade"
	// Reset deletes the RedisFailover and its volumes, recreates it from
	// its template on the chain's start, and refills it.
	Reset Kind = "reset"
	// SentinelImageFlip changes only the Sentinel image to a different value
	// in sentinelImages, for example between a Redis and a Valkey version.
	SentinelImageFlip Kind = "sentinel_image_flip"
)

var kinds = []Kind{
	RedisReplicas, SentinelReplicas, RedisResources, KillMaster, KillMasterForce, KillReplica, KillSentinel,
	RedisMemory, MaxMemoryPolicy, MaxMemoryPercent, FillBurst,
	PasswordRotate, AuthAdd, AuthRemove, SentinelToggle, PasswordRotateOffline,
	ImageUpgrade, SentinelImageUpgrade, Reset, SentinelImageFlip,
}

// bootstrapKinds are the kinds of a bootstrapping instance, which has no
// master of its own.
var bootstrapKinds = []Kind{RedisReplicas, RedisResources, KillReplica}

// Exclusive reports whether a kind affects every instance, so no other
// mutation may run meanwhile.
func Exclusive(k Kind) bool {
	return k == PasswordRotateOffline
}

func maxMemoryOnly(k Kind) bool {
	return k == RedisMemory || k == MaxMemoryPolicy || k == MaxMemoryPercent || k == FillBurst
}

func sentinelOnly(k Kind) bool {
	return k == SentinelReplicas || k == KillSentinel || k == SentinelImageFlip
}

// Mutations is an instance's mutation catalogue.
type Mutations struct {
	// Kinds are the enabled kinds and their weights.
	Kinds            map[Kind]int `json:"kinds"`
	RedisReplicas    Range        `json:"redisReplicas"`
	SentinelReplicas Range        `json:"sentinelReplicas"`
	Resources        Resources    `json:"resources"`
	// ForceDeleteProbability is the share of replica and Sentinel kills
	// that delete the pod with GracePeriodSeconds=0 instead of gracefully.
	// Master kills are kill_master or kill_master_force.
	ForceDeleteProbability float64 `json:"forceDeleteProbability"`
	// RedisMemory bounds the redis container's memory limit in Mi.
	RedisMemory       Range    `json:"redisMemory"`
	MaxMemoryPolicies []string `json:"maxMemoryPolicies"`
	MaxMemoryPercent  Range    `json:"maxMemoryPercent"`
	// FillBurstHold is how long fill_burst keeps writing past maxmemory.
	FillBurstHold metav1.Duration `json:"fillBurstHold"`
	// SentinelImages are the versions sentinel_image_flip changes the
	// Sentinel image between, by name.
	SentinelImages []string `json:"sentinelImages"`
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

// MutationKinds returns every kind the instance's mutator may run: the
// enabled ones, and those image_upgrade turns into at the end of a chain.
func (in Instance) MutationKinds() []Kind {
	kinds := in.Mutations.Sorted()
	if _, ok := in.Mutations.Kinds[ImageUpgrade]; ok {
		if in.Template != "" {
			kinds = append(kinds, Reset)
		}
		if in.Chain != nil && in.Chain.Sentinel == SentinelSeparate {
			kinds = append(kinds, SentinelImageUpgrade)
		}
	}
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// Events returns every value of an instance's event label: its mutation
// kinds, the chaos kinds, and failovers, periodic verifications and
// resets.
func (c *Config) Events(in Instance) []string {
	events := []string{EventFailover, EventPeriodic, EventReset}
	for _, k := range in.MutationKinds() {
		events = append(events, string(k))
	}
	if c.ChaosOn() {
		for _, k := range c.Chaos.Sorted() {
			events = append(events, string(k))
		}
	}
	slices.Sort(events)
	return slices.Compact(events)
}

func (m Mutations) validate(in Instance) error {
	for k, w := range m.Kinds {
		if !slices.Contains(kinds, k) {
			return fmt.Errorf("unknown kind %q", k)
		}
		if w < 1 {
			return fmt.Errorf("%s: weight must be at least 1", k)
		}
		if sentinelOnly(k) && in.Mode != ModeSentinel {
			return fmt.Errorf("%s needs a sentinel instance", k)
		}
		if maxMemoryOnly(k) && in.MaxMemoryPolicy == "" {
			return fmt.Errorf("%s needs an instance with maxMemoryPolicy", k)
		}
		if in.Bootstrap != nil && !slices.Contains(bootstrapKinds, k) {
			return fmt.Errorf("%s can't run on a bootstrapping instance", k)
		}
		if k == ImageUpgrade && in.Chain == nil {
			return fmt.Errorf("%s needs a chain", k)
		}
		if k == SentinelImageUpgrade && (in.Chain == nil || in.Chain.Sentinel != SentinelSeparate) {
			return fmt.Errorf("%s needs a chain whose sentinel is %s", k, SentinelSeparate)
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
		if err := m.Resources.validateData(in); err != nil {
			return fmt.Errorf("resources: %w", err)
		}
	}
	if _, ok := m.Kinds[RedisMemory]; ok {
		if err := m.RedisMemory.validate(minMemoryLimitMi); err != nil {
			return fmt.Errorf("redisMemory: %w", err)
		}
	}
	if _, ok := m.Kinds[MaxMemoryPolicy]; ok {
		// The mutation picks a policy other than the current one.
		if len(slices.Compact(slices.Sorted(slices.Values(m.MaxMemoryPolicies)))) < 2 {
			return errors.New("maxMemoryPolicies: at least two different policies")
		}
		for _, p := range m.MaxMemoryPolicies {
			if !slices.Contains(maxMemoryPolicies, p) {
				return fmt.Errorf("maxMemoryPolicies: %q must be one of %s", p, strings.Join(maxMemoryPolicies, ", "))
			}
		}
	}
	if _, ok := m.Kinds[MaxMemoryPercent]; ok {
		if err := m.MaxMemoryPercent.validate(10); err != nil || m.MaxMemoryPercent.Max > 95 {
			return errors.New("maxMemoryPercent: min and max must be between 10 and 95, max greater than min")
		}
	}
	if _, ok := m.Kinds[FillBurst]; ok {
		if in.Data == nil || !slices.Contains(in.Policies(), "noeviction") {
			return errors.New("fill_burst needs data and the noeviction policy")
		}
		if m.FillBurstHold.Duration <= 0 {
			return errors.New("fillBurstHold must be positive")
		}
	}
	return nil
}

// validateData keeps the memory limits of an instance with data above
// what the data needs. With maxMemory the data follows the limit, which
// only redis_memory changes knowing what the operator does then.
func (r Resources) validateData(in Instance) error {
	if !r.Requests.Memory.Set() && !r.Limits.Memory.Set() || in.Data == nil {
		return nil
	}
	if in.MaxMemoryPolicy != "" {
		return errors.New("memory of a maxMemory instance with data is changed by redis_memory only")
	}
	// A full sync forks the master, whose copy-on-write pages can double
	// the data, plus the operator's 32Mi reserve.
	if least := 2*in.Data.Fill.SizeMi + 32; r.Limits.Memory.Set() && r.Limits.Memory.Min < least {
		return fmt.Errorf("limits.memory: min must be at least %dMi for %dMi of data", least, in.Data.Fill.SizeMi)
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
	// A request equal to its limit can make a Burstable pod Guaranteed, which
	// the operator cannot resize in place. A request above its limit is not
	// valid.
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

// Events, the values of the event label besides the mutation kinds.
const (
	EventFailover = "failover"
	EventPeriodic = "periodic"
	// EventReset is a failover that loses the data by design: the only pod
	// of an instance without a PersistentVolumeClaim was replaced, or the
	// RedisFailover was recreated.
	EventReset = "reset"
)
