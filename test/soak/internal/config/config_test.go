package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadExample(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Instances) != 6 || c.Instances[0].Name != "op-basic" || c.Instances[1].Mode != ModeSentinel || c.Instances[4].Bootstrap == nil ||
		c.Instances[5].Chain == nil || len(c.Versions) != 3 || len(c.Edges) != 3 {
		t.Fatalf("unexpected instances: %+v", c.Instances)
	}
}

func TestShippedConfigsParse(t *testing.T) {
	for _, path := range []string{"../../deploy/config.yaml", "../../e2e/config.yaml", "../../e2e/config-versions.yaml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(b); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte("instances: [{name: a, namespace: ns}]"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Probe.Interval.Duration != time.Second || c.Probe.Timeout.Duration != time.Second {
		t.Errorf("probe defaults: %+v", c.Probe)
	}
	if c.Observer.Interval.Duration != 5*time.Second || c.Observer.ConvergenceTimeout.Duration != 10*time.Minute {
		t.Errorf("observer defaults: %+v", c.Observer)
	}
	if c.Operator.Namespace != "redis-operator" || c.Operator.Deployment != "redis-operator" {
		t.Errorf("operator defaults: %+v", c.Operator)
	}
	in := c.Instances[0]
	if in.Mode != ModeOperator || in.Port != 6379 {
		t.Errorf("instance defaults: %+v", in)
	}
	mu := c.Mutation
	if !mu.On() || mu.Interval.Duration != 2*time.Minute || mu.Jitter.Duration != 0 || mu.MinDwell.Duration != 15*time.Second || mu.Seed == 0 {
		t.Errorf("mutation defaults: %+v", mu)
	}
	if len(in.Mutations.Kinds) != 0 {
		t.Errorf("mutations enabled by default: %+v", in.Mutations)
	}
}

func TestMutations(t *testing.T) {
	c, err := Parse([]byte(`
mutation: {enabled: false, seed: 7}
instances:
  - name: a
    namespace: ns
    mode: sentinel
    mutations:
      kinds: {redis_replicas: 2, sentinel_replicas: 1, redis_resources: 1, kill_master: 1, kill_replica: 1, kill_sentinel: 1}
      redisReplicas: {min: 1, max: 5}
      sentinelReplicas: {min: 3, max: 5}
      resources:
        requests: {cpu: {min: 25, max: 100}, memory: {min: 32, max: 96}}
        limits: {cpu: {min: 200, max: 400}, memory: {min: 128, max: 256}}
      forceDeleteProbability: 0.5
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mutation.On() || c.Mutation.Seed != 7 {
		t.Errorf("mutation: %+v", c.Mutation)
	}
	want := []Kind{KillMaster, KillReplica, KillSentinel, RedisReplicas, RedisResources, SentinelReplicas}
	if got := c.Instances[0].Mutations.Sorted(); !slices.Equal(got, want) {
		t.Errorf("kinds %v, want %v", got, want)
	}
}

func TestInvalidMutations(t *testing.T) {
	instance := func(mode, mutations string) string {
		return "instances: [{name: a, namespace: ns, mode: " + mode + ", mutations: " + mutations + "}]"
	}
	cases := map[string]string{
		"unknown kind":          instance("operator", "{kinds: {kill_all: 1}}"),
		"zero weight":           instance("operator", "{kinds: {kill_master: 0}}"),
		"sentinel kind":         instance("operator", "{kinds: {kill_sentinel: 1}}"),
		"sentinel replicas":     instance("operator", "{kinds: {sentinel_replicas: 1}, sentinelReplicas: {min: 3, max: 5}}"),
		"no replica range":      instance("operator", "{kinds: {redis_replicas: 1}}"),
		"zero replicas":         instance("operator", "{kinds: {redis_replicas: 1}, redisReplicas: {min: 0, max: 3}}"),
		"one replica value":     instance("operator", "{kinds: {redis_replicas: 1}, redisReplicas: {min: 3, max: 3}}"),
		"inverted range":        instance("operator", "{kinds: {redis_replicas: 1}, redisReplicas: {min: 5, max: 1}}"),
		"two sentinels":         instance("sentinel", "{kinds: {sentinel_replicas: 1}, sentinelReplicas: {min: 2, max: 5}}"),
		"no resource bounds":    instance("operator", "{kinds: {redis_resources: 1}}"),
		"zero cpu":              instance("operator", "{kinds: {redis_resources: 1}, resources: {requests: {cpu: {min: 0, max: 100}}}}"),
		"small memory limit":    instance("operator", "{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 32, max: 256}}}}"),
		"request reaches limit": instance("operator", "{kinds: {redis_resources: 1}, resources: {requests: {memory: {min: 64, max: 128}}, limits: {memory: {min: 128, max: 256}}}}"),
		"cpu request over limit": instance("operator",
			"{kinds: {redis_resources: 1}, resources: {requests: {cpu: {min: 100, max: 500}}, limits: {cpu: {min: 200, max: 400}}}}"),
		"force probability": instance("operator", "{kinds: {kill_master: 1}, forceDeleteProbability: 1.5}"),
		"long dwell":        "mutation: {minDwell: 1h}\n" + instance("operator", "{}"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"no instances":  "probe: {interval: 1s}",
		"unknown field": "instances: [{name: a, namespace: ns, bogus: 1}]",
		"no namespace":  "instances: [{name: a}]",
		"long name":     "instances: [{name: " + strings.Repeat("a", 49) + ", namespace: ns}]",
		"bad mode":      "instances: [{name: a, namespace: ns, mode: cluster}]",
		"bad port":      "instances: [{name: a, namespace: ns, port: 70000}]",
		"duplicate":     "instances: [{name: a, namespace: ns}, {name: a, namespace: ns}]",
		"bad duration":  "probe: {interval: soon}\ninstances: [{name: a, namespace: ns}]",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestData(t *testing.T) {
	c, err := Parse([]byte(`
instances:
  - name: a
    namespace: ns
    maxMemoryPolicy: noeviction
    data:
      fill: {percent: 70, ttl: 1h}
      ledger: {}
    mutations:
      kinds: {redis_memory: 1, maxmemory_policy: 1, maxmemory_percent: 1, fill_burst: 1}
      redisMemory: {min: 128, max: 256}
      maxMemoryPolicies: [noeviction, volatile-lru]
      maxMemoryPercent: {min: 10, max: 95}
  - name: b
    namespace: ns
    maxMemoryPolicy: allkeys-lru
    data: {fill: {percent: 90}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Probe.WaitEvery != 10 || c.Probe.WaitTimeout.Duration != 500*time.Millisecond {
		t.Errorf("probe defaults: %+v", c.Probe)
	}
	d := c.Instances[0].Data
	if d.Fill.Percent != 70 || d.Fill.SizeMi != 16 || d.Fill.ValueBytes != 1024 || d.Fill.KeysPerSecond != 1000 || d.Fill.Batch != 50 {
		t.Errorf("fill defaults: %+v", d.Fill)
	}
	if l := d.Ledger; l.WritesPerSecond != 10 || l.ValueBytes != 64 || l.SampleKeys != 100 || l.VerifyInterval.Duration != 10*time.Minute {
		t.Errorf("ledger defaults: %+v", l)
	}
	if c.Instances[0].Mutations.FillBurstHold.Duration != 10*time.Second {
		t.Errorf("fillBurstHold default: %v", c.Instances[0].Mutations.FillBurstHold)
	}
	if got := c.Instances[0].Policies(); !slices.Equal(got, []string{"noeviction", "noeviction", "volatile-lru"}) {
		t.Errorf("policies %v", got)
	}
	if c.Instances[1].Data.Ledger != nil {
		t.Error("ledger on by default")
	}
}

func TestInvalidData(t *testing.T) {
	instance := func(policy, data, mutations string) string {
		return "instances: [{name: a, namespace: ns, maxMemoryPolicy: '" + policy + "', data: " + data + ", mutations: " + mutations + "}]"
	}
	cases := map[string]string{
		"unknown policy":       instance("lru", "{fill: {}}", "{}"),
		"ledger under allkeys": instance("allkeys-lru", "{fill: {}, ledger: {}}", "{}"),
		"ledger switched to allkeys": instance("noeviction", "{fill: {}, ledger: {}}",
			"{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction, allkeys-lfu]}"),
		"ledger under volatile without ttl": instance("volatile-lru", "{fill: {}, ledger: {}}", "{}"),
		"fill percent":                      instance("noeviction", "{fill: {percent: 120}}", "{}"),
		"tiny values":                       instance("noeviction", "{fill: {valueBytes: 4}}", "{}"),
		"maxMemory kind without maxMemory":  instance("", "{fill: {}}", "{kinds: {maxmemory_percent: 1}, maxMemoryPercent: {min: 10, max: 95}}"),
		"no memory range":                   instance("noeviction", "{fill: {}}", "{kinds: {redis_memory: 1}}"),
		"memory below 64Mi":                 instance("noeviction", "{fill: {}}", "{kinds: {redis_memory: 1}, redisMemory: {min: 32, max: 128}}"),
		"one policy":                        instance("noeviction", "{fill: {}}", "{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction]}"),
		"bad policy":                        instance("noeviction", "{fill: {}}", "{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction, lru]}"),
		"percent above 95":                  instance("noeviction", "{fill: {}}", "{kinds: {maxmemory_percent: 1}, maxMemoryPercent: {min: 10, max: 99}}"),
		"burst without noeviction":          instance("allkeys-lru", "{fill: {}}", "{kinds: {fill_burst: 1}}"),
		"burst without data":                "instances: [{name: a, namespace: ns, maxMemoryPolicy: noeviction, mutations: {kinds: {fill_burst: 1}}}]",
		"memory resources with maxMemory": instance("noeviction", "{fill: {}}",
			"{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 192, max: 512}}}}"),
		"memory below the data": instance("", "{fill: {sizeMi: 100}}",
			"{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 192, max: 512}}}}"),
		"wait timeout": "probe: {timeout: 1s, waitTimeout: 1s}\ninstances: [{name: a, namespace: ns}]",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Error("expected an error")
			}
		})
	}
	// cpu stays changeable on a maxMemory instance with data, and memory
	// with room for the data.
	for _, in := range []string{
		instance("noeviction", "{fill: {}}", "{kinds: {redis_resources: 1}, resources: {requests: {cpu: {min: 25, max: 100}}}}"),
		instance("", "{fill: {sizeMi: 32}}", "{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 192, max: 512}}}}"),
		instance("volatile-lru", "{fill: {ttl: 1h}, ledger: {}}", "{}"),
	} {
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
}

func TestBootstrap(t *testing.T) {
	c, err := Parse([]byte(`
instances:
  - {name: src, namespace: src, data: {fill: {}, ledger: {}}, mutations: {kinds: {kill_master_force: 1, password_rotate: 1, auth_add: 1, auth_remove: 1, password_rotate_offline: 1, sentinel_toggle: 1}}}
  - name: boot
    namespace: boot
    bootstrap: {source: src}
    mutations: {kinds: {redis_replicas: 1, kill_replica: 1}, redisReplicas: {min: 1, max: 3}}
`))
	if err != nil {
		t.Fatal(err)
	}
	src, boot := c.Instances[0], c.Instances[1]
	if src.AuthSecret != "src-auth" {
		t.Errorf("authSecret default %q", src.AuthSecret)
	}
	if b := boot.Bootstrap; b.SampleKeys != 100 || b.VerifyInterval.Duration != time.Minute {
		t.Errorf("bootstrap defaults: %+v", b)
	}
	if !Exclusive(PasswordRotateOffline) || Exclusive(PasswordRotate) {
		t.Error("exclusive kinds")
	}

	base := "  - {name: src, namespace: src, data: {fill: {}, ledger: {}}}\n"
	cases := map[string]string{
		"unknown source":   "  - {name: boot, namespace: boot, bootstrap: {source: other}}\n",
		"source no ledger": "  - {name: src2, namespace: src, data: {fill: {}}}\n  - {name: boot, namespace: boot, bootstrap: {source: src2}}\n",
		"with data":        "  - {name: boot, namespace: boot, bootstrap: {source: src}, data: {fill: {}}}\n",
		"sentinel":         "  - {name: boot, namespace: boot, mode: sentinel, bootstrap: {source: src}}\n",
		"from itself":      "  - {name: boot, namespace: boot, bootstrap: {source: boot}}\n",
		"master kill":      "  - {name: boot, namespace: boot, bootstrap: {source: src}, mutations: {kinds: {kill_master: 1}}}\n",
		"toggle":           "  - {name: boot, namespace: boot, bootstrap: {source: src}, mutations: {kinds: {sentinel_toggle: 1}}}\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte("instances:\n" + base + in)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
