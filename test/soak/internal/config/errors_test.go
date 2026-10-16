package config

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The error texts of Config.validate and Mutations.validate. Each case has one
// fault, because the kinds are a map that has no order.
func TestValidateErrorTexts(t *testing.T) {
	in := func(fields string) string {
		return "instances: [{name: a, namespace: ns" + fields + "}]"
	}
	mut := func(fields, mutations string) string {
		return in(fields + ", mutations: " + mutations)
	}
	const (
		sentinel = ", mode: sentinel"
		maxMem   = ", maxMemoryPolicy: noeviction"
		data     = ", maxMemoryPolicy: noeviction, data: {fill: {}}"
		versions = "versions: [{name: v1, image: 'redis:7.2.4'}, {name: v2, image: 'redis:7.4.1'}]\n"
	)
	cases := []struct{ name, yaml, want string }{
		{"no instances", "probe: {interval: 1s}", "no instances configured"},
		{"negative interval", "mutation: {interval: -1s}\n" + in(""), "mutation: negative duration"},
		{"negative jitter", "mutation: {jitter: -1s}\n" + in(""), "mutation: negative duration"},
		{"negative stopAfter", "mutation: {stopAfter: -1s}\n" + in(""), "mutation: negative duration"},
		{"short convergence", "observer: {convergenceTimeout: 15s}\n" + in(""), "observer.convergenceTimeout must be longer than 15s"},
		{"unknown timeout kind", "mutation: {timeouts: {kill_all: {base: 1m}}}\n" + in(""),
			"mutation.timeouts: kill_all: a known kind, base longer than 15s, perPod not negative"},
		{"short timeout base", "mutation: {timeouts: {kill_master: {base: 15s}}}\n" + in(""),
			"mutation.timeouts: kill_master: a known kind, base longer than 15s, perPod not negative"},
		{"negative perPod", "mutation: {timeouts: {kill_master: {perPod: -1s}}}\n" + in(""),
			"mutation.timeouts: kill_master: a known kind, base longer than 15s, perPod not negative"},
		{"versions", "versions: [{name: v1}]\n" + in(""), "versions: name and image are required"},
		{"chaos", "chaos: {kinds: {kill_all: 1}}\n" + in(""), `chaos: unknown kind "kill_all"`},
		{"no name", "instances: [{namespace: ns}]", "instance: name is required"},
		{"long name", "instances: [{name: " + strings.Repeat("a", 49) + "}]",
			`instance "` + strings.Repeat("a", 49) + `": name is longer than 48 characters`},
		{"mode", in(", mode: cluster"), `instance "a": mode must be "operator" or "sentinel"`},
		{"port", in(", port: 70000"), `instance "a": invalid port 70000`},
		{"data", in(", maxMemoryPolicy: lru"),
			`instance "a": maxMemoryPolicy "lru" must be one of noeviction, allkeys-lru, allkeys-lfu, allkeys-random, volatile-lru, volatile-lfu, volatile-random, volatile-ttl`},
		{"autoscale", in(", autoscale: {limit: {min: 128, max: 256}, memoryMax: {min: 256, max: 512}}"),
			`instance "a": autoscale: an instance with autoscale needs maxMemoryPolicy noeviction`},
		{"bootstrap", in(", bootstrap: {source: b}"), `instance "a": bootstrap: source "b" is not configured`},
		{"template", in(", version: v1"), `instance "a": chain, version, sentinelVersion and reset need a template`},
		{"flip", versions + mut(sentinel, "{kinds: {sentinel_image_flip: 1}, sentinelImages: [v1, v1]}"),
			`instance "a": mutations: sentinel_image_flip needs at least two sentinelImages`},
		{"flip unknown", versions + mut(sentinel, "{kinds: {sentinel_image_flip: 1}, sentinelImages: [v1, v3]}"),
			`instance "a": mutations: sentinelImages: unknown version v3`},
		{"twice", "instances: [{name: a, namespace: ns1}, {name: a, namespace: ns2}]",
			"instance a is configured twice: names must be unique across namespaces"},

		{"unknown kind", mut("", "{kinds: {kill_all: 1}}"), `instance "a": mutations: unknown kind "kill_all"`},
		{"weight", mut("", "{kinds: {kill_master: 0}}"), `instance "a": mutations: kill_master: weight must be at least 1`},
		{"sentinel kind", mut("", "{kinds: {kill_sentinel: 1}}"), `instance "a": mutations: kill_sentinel needs a sentinel instance`},
		{"maxMemory kind", mut("", "{kinds: {fill_burst: 1}}"), `instance "a": mutations: fill_burst needs an instance with maxMemoryPolicy`},
		{"autoscale kind", mut("", "{kinds: {autoscale_load: 1}}"), `instance "a": mutations: autoscale_load needs an instance with autoscale`},
		{"bootstrap kind", "instances: [{name: s, data: {fill: {}, ledger: {}}}, {name: a, bootstrap: {source: s}, mutations: {kinds: {kill_master: 1}}}]",
			`instance "a": mutations: kill_master can't run on a bootstrapping instance`},
		{"image upgrade", mut("", "{kinds: {image_upgrade: 1}}"), `instance "a": mutations: image_upgrade needs a chain`},
		{"sentinel image upgrade", mut("", "{kinds: {sentinel_image_upgrade: 1}}"),
			`instance "a": mutations: sentinel_image_upgrade needs a chain whose sentinel is separate`},
		{"force probability", mut("", "{forceDeleteProbability: -0.1}"),
			`instance "a": mutations: forceDeleteProbability must be between 0 and 1`},
		{"redis replicas", mut("", "{kinds: {redis_replicas: 1}, redisReplicas: {min: 0, max: 3}}"),
			`instance "a": mutations: redisReplicas: min must be at least 1`},
		{"redis replicas range", mut("", "{kinds: {redis_replicas: 1}, redisReplicas: {min: 3, max: 3}}"),
			`instance "a": mutations: redisReplicas: max must be greater than min`},
		{"sentinel replicas", mut(sentinel, "{kinds: {sentinel_replicas: 1}, sentinelReplicas: {min: 2, max: 5}}"),
			`instance "a": mutations: sentinelReplicas: min must be at least 3`},
		{"no resource bounds", mut("", "{kinds: {redis_resources: 1}}"), `instance "a": mutations: resources: no bounds`},
		{"resource range", mut("", "{kinds: {redis_resources: 1}, resources: {limits: {cpu: {min: 0, max: 100}}}}"),
			`instance "a": mutations: resources: limits.cpu: min must be at least 1`},
		{"small memory limit", mut("", "{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 32, max: 256}}}}"),
			`instance "a": mutations: resources: limits.memory: min must be at least 64Mi`},
		{"qos class", mut("", "{kinds: {redis_resources: 1}, resources: {requests: {cpu: {min: 100, max: 200}}, limits: {cpu: {min: 200, max: 400}}}}"),
			`instance "a": mutations: resources: requests.cpu must stay below limits.cpu, which would change the QoS class`},
		{"memory with maxMemory", mut(data, "{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 192, max: 512}}}}"),
			`instance "a": mutations: resources: memory of a maxMemory instance with data is changed by redis_memory only`},
		{"memory below the data", mut(", data: {fill: {sizeMi: 100}}", "{kinds: {redis_resources: 1}, resources: {limits: {memory: {min: 192, max: 512}}}}"),
			`instance "a": mutations: resources: limits.memory: min must be at least 232Mi for 100Mi of data`},
		{"redis memory", mut(maxMem, "{kinds: {redis_memory: 1}, redisMemory: {min: 32, max: 128}}"),
			`instance "a": mutations: redisMemory: min must be at least 64`},
		{"one policy", mut(maxMem, "{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction, noeviction]}"),
			`instance "a": mutations: maxMemoryPolicies: at least two different policies`},
		{"bad policy", mut(maxMem, "{kinds: {maxmemory_policy: 1}, maxMemoryPolicies: [noeviction, lru]}"),
			`instance "a": mutations: maxMemoryPolicies: "lru" must be one of noeviction, allkeys-lru, allkeys-lfu, allkeys-random, volatile-lru, volatile-lfu, volatile-random, volatile-ttl`},
		{"percent range", mut(maxMem, "{kinds: {maxmemory_percent: 1}, maxMemoryPercent: {min: 5, max: 50}}"),
			`instance "a": mutations: maxMemoryPercent: min and max must be between 10 and 95, max greater than min`},
		{"percent above 95", mut(maxMem, "{kinds: {maxmemory_percent: 1}, maxMemoryPercent: {min: 10, max: 99}}"),
			`instance "a": mutations: maxMemoryPercent: min and max must be between 10 and 95, max greater than min`},
		{"no room for a fork", mut(data, "{kinds: {redis_memory: 2, maxmemory_percent: 1}, redisMemory: {min: 128, max: 256}, maxMemoryPercent: {min: 10, max: 90}}"),
			`instance "a": mutations: maxMemoryPercent: max 90 leaves no room for a fork of the data at redisMemory.min 128Mi`},
		{"burst without data", mut(maxMem, "{kinds: {fill_burst: 1}}"), `instance "a": mutations: fill_burst needs data and the noeviction policy`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil || err.Error() != c.want {
				t.Errorf("error %v\nwant  %s", err, c.want)
			}
		})
	}
}

// setDefaults fills each unset field, and keeps each set one.
func TestSetDefaultsValues(t *testing.T) {
	c := Config{
		Mutation: Mutation{Seed: 3, Timeouts: map[Kind]Timeout{
			KillMaster: {PerPod: metav1.Duration{Duration: time.Second}},
			Reset:      {Base: metav1.Duration{Duration: time.Hour}},
		}},
		Versions: []Version{{Name: "v1", Image: "redis:7.2.4"}},
		Instances: []Instance{
			{Name: "a"},
			{Name: "b", Namespace: "ns", Mode: ModeSentinel, Port: 7000, Data: &Data{},
				Chain: &Chain{Start: []string{"v1", "v2"}, Sentinel: SentinelSeparate}},
			{Name: "c", Version: "v2", SentinelVersion: "v1", Chain: &Chain{Start: []string{"v1"}, Sentinel: SentinelFollow}},
			{Name: "d", Chain: &Chain{}},
		},
	}
	c.setDefaults()
	if c.Operator != (Operator{Namespace: "redis-operator", Deployment: "redis-operator"}) {
		t.Errorf("operator %+v", c.Operator)
	}
	if c.Probe.Interval.Duration != time.Second || c.Probe.Timeout.Duration != time.Second {
		t.Errorf("probe %+v", c.Probe)
	}
	if c.Observer.Interval.Duration != 5*time.Second || c.Observer.ConvergenceTimeout.Duration != 10*time.Minute {
		t.Errorf("observer %+v", c.Observer)
	}
	if c.Mutation.Interval.Duration != 2*time.Minute || c.Mutation.Seed != 3 {
		t.Errorf("mutation %+v", c.Mutation)
	}
	d := func(x time.Duration) metav1.Duration { return metav1.Duration{Duration: x} }
	want := map[Kind]Timeout{
		KillMaster:           {Base: d(10 * time.Minute), PerPod: d(time.Second)},
		Reset:                {Base: d(time.Hour)},
		ImageUpgrade:         {Base: d(10 * time.Minute), PerPod: d(2 * time.Minute)},
		SentinelImageUpgrade: {Base: d(10 * time.Minute), PerPod: d(time.Minute)},
	}
	if len(c.Mutation.Timeouts) != len(want) {
		t.Errorf("timeouts %+v", c.Mutation.Timeouts)
	}
	for k, w := range want {
		if c.Mutation.Timeouts[k] != w {
			t.Errorf("timeout %s: %+v, want %+v", k, c.Mutation.Timeouts[k], w)
		}
	}
	type inst struct {
		ns              string
		mode            Mode
		port            int
		version, sentin string
	}
	for i, w := range []inst{
		{DefaultNamespace, ModeOperator, 6379, "", ""},
		{"ns", ModeSentinel, 7000, "v1", "v1"},
		{DefaultNamespace, ModeOperator, 6379, "v2", "v1"},
		{DefaultNamespace, ModeOperator, 6379, "", ""},
	} {
		in := c.Instances[i]
		if got := (inst{in.Namespace, in.Mode, in.Port, in.Version, in.SentinelVersion}); got != w {
			t.Errorf("instance %s: %+v, want %+v", in.Name, got, w)
		}
	}
	if f := c.Instances[1].Data.Fill; f.Percent == 0 || f.SizeMi == 0 {
		t.Errorf("data defaults: %+v", f)
	}
	c.Mutation.Seed = 0
	c.setDefaults()
	if c.Mutation.Seed == 0 {
		t.Error("no seed picked")
	}
}
