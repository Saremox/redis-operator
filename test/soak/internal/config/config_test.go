package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestShippedConfigsParse(t *testing.T) {
	for _, path := range []string{"../../deploy/config.yaml", "../../e2e/config.yaml", "../../e2e/config-versions.yaml", "../../e2e/config-chaos.yaml"} {
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
	if mu.Interval.Duration != 2*time.Minute || mu.Jitter.Duration != 0 || mu.Seed == 0 {
		t.Errorf("mutation defaults: %+v", mu)
	}
	if len(in.Mutations.Kinds) != 0 {
		t.Errorf("mutations enabled by default: %+v", in.Mutations)
	}
}

func TestMutations(t *testing.T) {
	c, err := Parse([]byte(`
mutation: {seed: 7}
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
	if c.Mutation.Seed != 7 {
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
		"short convergence": "observer: {convergenceTimeout: 10s}\n" + instance("operator", "{}"),
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
		"no name":       "instances: [{namespace: ns}]",
		"long name":     "instances: [{name: " + strings.Repeat("a", 49) + ", namespace: ns}]",
		"bad mode":      "instances: [{name: a, namespace: ns, mode: cluster}]",
		"bad port":      "instances: [{name: a, namespace: ns, port: 70000}]",
		"duplicate":     "instances: [{name: a, namespace: ns}, {name: a, namespace: ns}]",
		// The tester keys every instance by name, as a bootstrap's source.
		"duplicate name": "instances: [{name: a, namespace: ns1}, {name: a, namespace: ns2}]",
		"bad duration":   "probe: {interval: soon}\ninstances: [{name: a, namespace: ns}]",
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
	if d := c.Instances[0].Data; d.Fill.Percent != 70 || d.Fill.SizeMi != 16 {
		t.Errorf("fill defaults: %+v", d.Fill)
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
		"maxMemory kind without maxMemory":  instance("", "{fill: {}}", "{kinds: {maxmemory_percent: 1}, maxMemoryPercent: {min: 10, max: 95}}"),
		"no memory range":                   instance("noeviction", "{fill: {}}", "{kinds: {redis_memory: 1}}"),
		"memory below 64Mi":                 instance("noeviction", "{fill: {}}", "{kinds: {redis_memory: 1}, redisMemory: {min: 32, max: 128}}"),
		"one policy":                        instance("noeviction", "{fill: {}}", "{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction]}"),
		// maxmemory_policy picks a policy other than the current one.
		"same policy twice":        instance("noeviction", "{fill: {}}", "{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction, noeviction]}"),
		"bad policy":               instance("noeviction", "{fill: {}}", "{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction, lru]}"),
		"percent above 95":         instance("noeviction", "{fill: {}}", "{kinds: {maxmemory_percent: 1}, maxMemoryPercent: {min: 10, max: 99}}"),
		"burst without noeviction": instance("allkeys-lru", "{fill: {}}", "{kinds: {fill_burst: 1}}"),
		"burst without data":       "instances: [{name: a, namespace: ns, maxMemoryPolicy: noeviction, mutations: {kinds: {fill_burst: 1}}}]",
		"memory resources with maxMemory": instance("noeviction", "{fill: {}}",
			"{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 192, max: 512}}}}"),
		"memory below the data": instance("", "{fill: {sizeMi: 100}}",
			"{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 192, max: 512}}}}"),
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
    bootstrap: {source: src}
    mutations: {kinds: {redis_replicas: 1, kill_replica: 1}, redisReplicas: {min: 1, max: 3}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if src := c.Instances[0]; src.AuthSecret() != "src-auth" || src.Namespace != "src" || c.Instances[1].Namespace != DefaultNamespace {
		t.Errorf("instance defaults: %+v", c.Instances)
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
