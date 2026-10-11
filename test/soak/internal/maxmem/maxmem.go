// Package maxmem tells what maxmemory and maxmemory-policy the operator sets
// on an instance with spec.redis.maxMemory, by the operator's own rules.
package maxmem

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// Pod is one redis pod as the operator sees it.
type Pod struct {
	Name string
	// Limit is the redis container's memory limit, as the kubelet reports
	// it applied or else the pod spec's; 0 without one.
	Limit int64
	// MaxMemory and Policy are what CONFIG GET reports, unless Err.
	MaxMemory int64
	Policy    string
	Err       error
}

const keptPrefix = "maxmemory kept at "

// Check reports how the pods differ from what the operator sets for rf,
// which has its defaults applied. Where the operator keeps maxmemory above
// its target, message, the status message, gives the kept value.
func Check(rf *redisfailoverv1.RedisFailover, message string, pods []Pod) error {
	mm := rf.Spec.Redis.MaxMemory
	if mm == nil || rf.ManagedMaxMemoryError() != nil {
		return nil
	}
	policy := mm.Policy
	if v, ok := customConfig(rf, "maxmemory-policy"); ok {
		policy = v
	}
	var errs []error
	var answered []Pod
	for _, p := range pods {
		if p.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, p.Err))
			continue
		}
		answered = append(answered, p)
		if !strings.EqualFold(p.Policy, policy) {
			errs = append(errs, fmt.Errorf("%s: maxmemory-policy %s, want %s", p.Name, p.Policy, policy))
		}
	}
	want, kept, ok := Expect(rf, message, pods)
	if !ok {
		return errors.Join(errs...)
	}
	// Where it is kept, every pod follows the master's kept value.
	for _, p := range answered {
		switch {
		case kept != "" && FormatBytes(p.MaxMemory) != kept:
			errs = append(errs, fmt.Errorf("%s: maxmemory %s, the status keeps %s", p.Name, FormatBytes(p.MaxMemory), kept))
		case kept == "" && p.MaxMemory != want:
			errs = append(errs, fmt.Errorf("%s: maxmemory %d (%s), want %d (%s)",
				p.Name, p.MaxMemory, FormatBytes(p.MaxMemory), want, FormatBytes(want)))
		}
	}
	return errors.Join(errs...)
}

// Expect returns the maxmemory the operator sets, or the value the status
// message keeps instead, formatted as the operator does. ok is false where
// the operator leaves maxmemory alone.
func Expect(rf *redisfailoverv1.RedisFailover, message string, pods []Pod) (want int64, kept string, ok bool) {
	if v, ok := customConfig(rf, "maxmemory"); ok {
		n, err := ParseMemory(v)
		return n, "", err == nil
	}
	smallest := Smallest(rf, pods)
	if smallest < redisfailoverv1.MinManagedMemoryLimit {
		return 0, "", false
	}
	want = rf.MaxMemoryFor(smallest)
	if v, reason, ok := Kept(message); ok {
		// Only allkeys-* surely evicts enough, so it is lowered anyway
		// unless the master changed meanwhile.
		evicts := strings.HasPrefix(rf.Spec.Redis.MaxMemory.Policy, "allkeys-")
		if !evicts || strings.Contains(reason, "stopped being the master") {
			kept = v
		}
	}
	return want, kept, true
}

// Smallest is the smallest limit maxmemory follows: each pod's, capped by
// the configured one.
func Smallest(rf *redisfailoverv1.RedisFailover, pods []Pod) int64 {
	spec := rf.Spec.Redis.Resources.Limits.Memory().Value()
	var smallest int64
	for _, p := range pods {
		limit := spec
		if p.Limit > 0 && p.Limit < spec {
			limit = p.Limit
		}
		if smallest == 0 || limit < smallest {
			smallest = limit
		}
	}
	if smallest == 0 {
		return spec
	}
	return smallest
}

// Kept parses a status message of a kept maxmemory into the kept value and
// the reason.
func Kept(message string) (value, reason string, ok bool) {
	rest, ok := strings.CutPrefix(message, keptPrefix)
	if !ok {
		return "", "", false
	}
	value, reason, ok = strings.Cut(rest, ": ")
	return value, reason, ok
}

// KeptBelowData reports whether message says the operator kept maxmemory
// because lowering it to target would not fit the memory in use.
func KeptBelowData(message string, target int64) bool {
	_, reason, ok := Kept(message)
	return ok && strings.HasPrefix(reason, "lowering it to "+FormatBytes(target)+" would not fit")
}

// FormatBytes formats like the operator's status messages.
func FormatBytes(b int64) string {
	if b == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%.1fMi", float64(b)/(1<<20))
}

func customConfig(rf *redisfailoverv1.RedisFailover, key string) (string, bool) {
	value, found := "", false
	for _, c := range rf.Spec.Redis.CustomConfig {
		if param, v, _ := strings.Cut(c, " "); strings.EqualFold(param, key) {
			value, found = strings.TrimSpace(v), true
		}
	}
	return value, found
}

// ParseMemory parses a redis memory value: bytes, or k, kb, m, mb, g, gb.
func ParseMemory(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"k", 1e3}, {"m", 1e6}, {"g", 1e9}} {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = v, u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n * mult, err
}

// RedisLimit returns the redis container's memory limit the kubelet reports
// as applied, or else the pod spec's; 0 without one.
func RedisLimit(p *corev1.Pod) int64 {
	var limits corev1.ResourceList
	for _, c := range p.Spec.Containers {
		if c.Name == "redis" {
			limits = c.Resources.Limits
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == "redis" && cs.Resources != nil {
			limits = cs.Resources.Limits
		}
	}
	if q, ok := limits[corev1.ResourceMemory]; ok {
		return q.Value()
	}
	return 0
}
