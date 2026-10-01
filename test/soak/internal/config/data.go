package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
	SizeMi     int64 `json:"sizeMi"`
	ValueBytes int   `json:"valueBytes"`
	// KeysPerSecond limits the filler, so it doesn't swamp the node.
	KeysPerSecond int `json:"keysPerSecond"`
	// Batch is the number of keys per pipeline.
	Batch int `json:"batch"`
	// TTL is set on every fill key, so volatile-* policies evict fill keys
	// only. 0 sets none.
	TTL metav1.Duration `json:"ttl"`
	// SampleKeys is the number of fill keys each verification checks.
	SampleKeys int `json:"sampleKeys"`
}

type Ledger struct {
	WritesPerSecond int `json:"writesPerSecond"`
	ValueBytes      int `json:"valueBytes"`
	// SampleKeys is the number of keys from before the previous
	// verification each verification checks again.
	SampleKeys int `json:"sampleKeys"`
	// VerifyInterval verifies the ledger when no event did for this long,
	// which bounds the keys it keeps.
	VerifyInterval metav1.Duration `json:"verifyInterval"`
}

func (d *Data) setDefaults() {
	f := &d.Fill
	if f.Percent == 0 {
		f.Percent = 50
	}
	if f.SizeMi == 0 {
		f.SizeMi = 16
	}
	if f.ValueBytes == 0 {
		f.ValueBytes = 1024
	}
	if f.KeysPerSecond == 0 {
		f.KeysPerSecond = 1000
	}
	if f.Batch == 0 {
		f.Batch = 50
	}
	if f.SampleKeys == 0 {
		f.SampleKeys = 100
	}
	if l := d.Ledger; l != nil {
		if l.WritesPerSecond == 0 {
			l.WritesPerSecond = 10
		}
		if l.ValueBytes == 0 {
			l.ValueBytes = 64
		}
		if l.SampleKeys == 0 {
			l.SampleKeys = 100
		}
		if l.VerifyInterval.Duration == 0 {
			l.VerifyInterval.Duration = 10 * time.Minute
		}
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
	case f.ValueBytes < 16 || f.ValueBytes > 1<<20:
		return errors.New("data.fill.valueBytes must be between 16 and 1048576")
	case f.KeysPerSecond < 1 || f.Batch < 1 || f.SampleKeys < 0:
		return errors.New("data.fill: keysPerSecond and batch must be at least 1")
	case f.TTL.Duration < 0:
		return errors.New("data.fill.ttl: negative duration")
	}
	l := d.Ledger
	if l == nil {
		return nil
	}
	switch {
	case l.WritesPerSecond < 1 || l.ValueBytes < 16 || l.SampleKeys < 0:
		return errors.New("data.ledger: writesPerSecond must be at least 1 and valueBytes at least 16")
	case l.VerifyInterval.Duration < 0:
		return errors.New("data.ledger.verifyInterval: negative duration")
	}
	// Ledger keys must never be evicted, or their loss couldn't be told
	// from an eviction.
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
