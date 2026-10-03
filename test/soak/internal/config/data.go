package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// maxMemoryPolicies are the policies spec.redis.maxMemory.policy accepts.
var maxMemoryPolicies = []string{
	"noeviction",
	"allkeys-lru", "allkeys-lfu", "allkeys-random",
	"volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl",
}

// Data configures an instance's data: the fill, and optionally the ledger of
// acknowledged writes that is verified after every failover and mutation.
type Data struct {
	Fill   Fill    `json:"fill"`
	Ledger *Ledger `json:"ledger"`
}

type Fill struct {
	// Percent of maxmemory the filler keeps used_memory at, on instances
	// with maxmemory.
	Percent int64 `json:"percent"`
	// SizeMi is the used_memory the filler keeps without maxmemory.
	SizeMi int64 `json:"sizeMi"`
	// TTL is set on every fill key, so volatile-* policies evict fill keys
	// only. 0 sets none.
	TTL metav1.Duration `json:"ttl"`
}

// Ledger turns on the ledger. It has no settings.
type Ledger struct{}

func (d *Data) setDefaults() {
	if d.Fill.Percent == 0 {
		d.Fill.Percent = 50
	}
	if d.Fill.SizeMi == 0 {
		d.Fill.SizeMi = 16
	}
}

// Policies returns every maxmemory-policy the instance may run under: the
// one it is created with and those maxmemory_policy switches to.
func (in Instance) Policies() []string {
	var ps []string
	if in.MaxMemoryPolicy != "" {
		ps = append(ps, in.MaxMemoryPolicy)
	}
	if _, ok := in.Mutations.Kinds[MaxMemoryPolicy]; ok {
		ps = append(ps, in.Mutations.MaxMemoryPolicies...)
	}
	return ps
}

func (in Instance) validateData() error {
	if in.MaxMemoryPolicy != "" && !slices.Contains(maxMemoryPolicies, in.MaxMemoryPolicy) {
		return fmt.Errorf("maxMemoryPolicy %q must be one of %s", in.MaxMemoryPolicy, strings.Join(maxMemoryPolicies, ", "))
	}
	d := in.Data
	if d == nil {
		return nil
	}
	f := d.Fill
	switch {
	case f.Percent < 1 || f.Percent > 100:
		return errors.New("data.fill.percent must be between 1 and 100")
	case f.SizeMi < 1:
		return errors.New("data.fill.sizeMi must be at least 1")
	case f.TTL.Duration < 0:
		return errors.New("data.fill.ttl: negative duration")
	}
	if d.Ledger == nil {
		return nil
	}
	// Ledger keys must never be evicted, because the tester cannot tell their
	// loss from an eviction.
	for _, p := range in.Policies() {
		if strings.HasPrefix(p, "allkeys-") {
			return fmt.Errorf("data.ledger: policy %s can evict ledger keys; allkeys-* instances run only the filler", p)
		}
		if strings.HasPrefix(p, "volatile-") && f.TTL.Duration == 0 {
			return fmt.Errorf("data.ledger: policy %s needs data.fill.ttl, so it evicts fill keys and never ledger keys", p)
		}
	}
	return nil
}
