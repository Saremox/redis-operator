package instances

import (
	"testing"

	"github.com/saremox/redis-operator/test/soak/internal/config"
)

// The memory limit of a redis pod holds the data twice, for the fork of a
// full sync that copies each page, plus the operator reserve of 32Mi. A save
// writes a second file next to the first, so a volume holds the RDB file
// twice. The memory request of a large data set holds the data once.
func TestShippedSizes(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sized := 0
	for _, in := range cfg.Instances {
		if in.Data == nil || in.MaxMemoryPolicy != "" || in.Bootstrap != nil {
			continue
		}
		rf, err := LoadTemplate(in.Template, in.Name)
		if err != nil {
			t.Fatal(err)
		}
		data := in.Data.Fill.SizeMi
		res := rf.Spec.Redis.Resources
		if limit := res.Limits.Memory().Value() >> 20; limit < 2*data+32 {
			t.Errorf("%s: the limit is %dMi for %dMi of data, want at least %dMi", in.Name, limit, data, 2*data+32)
		}
		if request := res.Requests.Memory().Value() >> 20; data >= 64 && request < data {
			t.Errorf("%s: the request is %dMi for %dMi of data", in.Name, request, data)
		}
		if pvc := rf.Spec.Redis.Storage.PersistentVolumeClaim; pvc != nil {
			if size := pvc.Spec.Resources.Requests.Storage().Value() >> 20; size < 2*data {
				t.Errorf("%s: the volume is %dMi for %dMi of data, want at least %dMi", in.Name, size, data, 2*data)
			}
		}
		sized++
	}
	if sized < 10 {
		t.Errorf("checked %d instances", sized)
	}
}

// An instance with autoscale runs in mode Auto. The load of autoscale_load
// is 97 percent of maxmemory, and it fits two times in the smallest limit,
// plus the operator reserve of 32Mi, for a fork. autoscale_memory_max keeps
// memoryMax at or above the limit of the template.
func TestShippedAutoscale(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, in := range cfg.Instances {
		if in.Autoscale == nil {
			continue
		}
		n++
		rf, err := LoadTemplate(in.Template, in.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := rf.Validate(); err != nil {
			t.Errorf("%s: %v", in.Name, err)
			continue
		}
		a := rf.Spec.Redis.Autoscaling
		if a == nil || a.Mode != "Auto" {
			t.Errorf("%s: the template is not in mode Auto", in.Name)
			continue
		}
		limit := rf.Spec.Redis.Resources.Limits.Memory().Value()
		smallest := limit
		if in.Autoscale.Limit.Set() {
			smallest = min(smallest, in.Autoscale.Limit.Min<<20)
		}
		if load := rf.MaxMemoryFor(smallest) * 97 / 100; 2*load+32<<20 > smallest {
			t.Errorf("%s: a load of %dMi does not fit two times in %dMi", in.Name, load>>20, smallest>>20)
		}
		if mm := in.Autoscale.MemoryMax; mm.Set() && mm.Min<<20 < limit {
			t.Errorf("%s: memoryMax.min is below the limit", in.Name)
		}
	}
	if n < 2 {
		t.Errorf("%d instances with autoscale", n)
	}
}

// Each instance has its own name and its own auth Secret.
func TestShippedSecrets(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{}
	for _, in := range cfg.Instances {
		rf, err := LoadTemplate(in.Template, in.Name)
		if err != nil {
			t.Fatal(err)
		}
		secret := rf.Spec.Auth.SecretPath
		if secret == "" {
			continue
		}
		if other, ok := secrets[secret]; ok {
			t.Errorf("%s and %s use the Secret %s", in.Name, other, secret)
		}
		if secret != in.Name+"-auth" {
			t.Errorf("%s uses the Secret %s", in.Name, secret)
		}
		secrets[secret] = in.Name
	}
	for _, name := range []string{"skip", "redis-chain-big"} {
		if secrets[name+"-auth"] != name {
			t.Errorf("%s has no Secret of its own", name)
		}
	}
}
