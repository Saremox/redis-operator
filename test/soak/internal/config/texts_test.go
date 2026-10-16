package config

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The error texts of validateVersions, validateChain, Chaos.validate and
// validateAutoscale, one case for each check. An empty want accepts the
// config.
func TestValidateErrorTextsOfEachCheck(t *testing.T) {
	const in = "\ninstances: [{name: a, namespace: ns}]"
	edge := func(e string) string { return graph + "  - " + e + in }
	chain := func(spec string) string {
		return graph + "instances: [{name: a, namespace: ns, mode: sentinel, template: a.yaml, chain: " + spec + "}]"
	}
	upgrade := func(versions string) string {
		return "chaos: {kinds: {operator_upgrade: 1}, upgrade: {versions: " + versions + "}}" + in
	}
	const opA = `{name: a, chart: a.tgz, image: "op:a"}`
	autoscale := func(fields string) string {
		return "instances: [{name: a, namespace: ns, maxMemoryPolicy: noeviction, data: {fill: {percent: 10}}, " + fields + "}]"
	}
	cases := []struct{ name, yaml, want string }{
		{"version without image", "versions: [{name: v1}]" + in, "versions: name and image are required"},
		{"version without name", "versions: [{image: 'redis:7.2.4'}]" + in, "versions: name and image are required"},
		{"version name twice", "versions: [{name: r, image: 'redis:7.2.16'}, {name: r, image: 'redis:7.4.11'}]" + in,
			"versions: r is configured twice"},
		{"version image twice", "versions: [{name: a, image: 'redis:7.2.16'}, {name: b, image: 'redis:7.2.16'}]" + in,
			"versions: image redis:7.2.16 is configured twice"},
		{"floating tag", "versions: [{name: r, image: 'redis:7-alpine'}]" + in, "versions: r: redis:7-alpine is not an exact patch tag"},
		{"unknown from", edge("{from: redis-6, to: redis-7.2, expect: ok}"), "edges: redis-6 -> redis-7.2: unknown version"},
		{"unknown to", edge("{from: redis-8, to: redis-9, expect: ok}"), "edges: redis-8 -> redis-9: unknown version"},
		{"self edge", edge("{from: redis-8, to: redis-8, expect: ok}"), "edges: redis-8 -> redis-8: an edge must change the version"},
		{"bad expect", edge("{from: redis-7.2, to: redis-8, expect: maybe}"), "edges: redis-7.2 -> redis-8: expect must be ok, fail or unknown"},
		{"edge twice", edge("{from: redis-7.2, to: redis-7.4, expect: unknown}"), "edges: redis-7.2 -> redis-7.4 is configured twice"},
		{"downgrade", edge("{from: redis-8, to: redis-7.4, expect: ok}"), "edges: redis-8 -> redis-7.4 is a downgrade, so it cannot expect ok"},
		{"downgrade that fails", edge("{from: redis-8, to: redis-7.4, expect: fail}"), ""},
		{"other server", edge("{from: valkey-8, to: redis-7.2, expect: ok}"), ""},

		{"chain without start", chain("{versions: [redis-7.2]}"), `instance "a": chain: start and versions are required`},
		{"chain without versions", chain("{start: [redis-7.2]}"), `instance "a": chain: start and versions are required`},
		{"chain unknown version", chain("{start: [redis-7.2], versions: [redis-7.2, redis-6]}"), `instance "a": chain: unknown version redis-6`},
		{"chain bad expect", chain("{start: [redis-7.2], versions: [redis-7.2, redis-7.4], expect: [maybe]}"),
			`instance "a": chain: expect: "maybe" must be ok, fail or unknown`},
		{"start not listed", chain("{start: [redis-7.2], versions: [redis-7.4, redis-8]}"),
			`instance "a": chain: start redis-7.2 is not one of its versions`},
		{"dead start", chain("{start: [redis-8], versions: [redis-8]}"), `instance "a": chain: start redis-8 has no edge to take`},
		{"unreachable", chain("{start: [redis-7.2], versions: [redis-7.2, redis-7.4, valkey-8]}"),
			`instance "a": chain: version valkey-8 is not reachable from a start`},
		{"bad sentinel", chain("{start: [redis-7.2], versions: [redis-7.2, redis-7.4], sentinel: lead}"),
			`instance "a": chain: sentinel must be "follow", "separate" or empty`},
		{"chain", chain("{start: [redis-7.2], versions: [redis-7.2, redis-7.4], sentinel: separate}"), ""},

		{"chaos unknown kind", "chaos: {kinds: {delete_cluster: 1}}" + in, `chaos: unknown kind "delete_cluster"`},
		{"chaos weight", "chaos: {kinds: {node_drain: 0}}" + in, "chaos: node_drain: weight must be at least 1"},
		{"chaos interval", "chaos: {kinds: {node_drain: 1}, interval: -1s}" + in, "chaos: negative duration"},
		{"chaos jitter", "chaos: {kinds: {node_drain: 1}, jitter: -1s}" + in, "chaos: negative duration"},
		{"chaos timeout", "chaos: {kinds: {node_drain: 1}, timeout: -1s}" + in, "chaos: negative duration"},
		{"chaos hold", "chaos: {kinds: {node_drain: 1}, drain: {hold: -1s}}" + in, "chaos: negative duration"},
		{"chaos drain timeout", "chaos: {kinds: {node_drain: 1}, drain: {timeout: -1s}}" + in, "chaos: negative duration"},
		{"no upgrade", "chaos: {kinds: {node_drain: 1}, upgrade: {versions: [{name: a}]}}" + in, ""},
		{"one version", upgrade("[" + opA + "]"), "chaos: upgrade: at least two versions"},
		{"no name", upgrade("[" + opA + `, {chart: b.tgz, image: "op:b"}]`), "chaos: upgrade: every version needs a name, chart and image"},
		{"no chart", upgrade("[" + opA + `, {name: b, image: "op:b"}]`), "chaos: upgrade: every version needs a name, chart and image"},
		{"no image", upgrade("[" + opA + `, {name: b, chart: b.tgz}]`), "chaos: upgrade: every version needs a name, chart and image"},
		{"no tag", upgrade("[" + opA + `, {name: b, chart: b.tgz, image: "localhost:5001/op"}]`),
			"chaos: upgrade: b: image localhost:5001/op has no tag"},
		{"upgrade name twice", upgrade("[" + opA + `, {name: a, chart: b.tgz, image: "op:b"}]`),
			"chaos: upgrade: a or its image is configured twice"},
		{"upgrade image twice", upgrade("[" + opA + `, {name: b, chart: b.tgz, image: "op:a"}]`),
			"chaos: upgrade: b or its image is configured twice"},
		{"upgrade", upgrade("[" + opA + `, {name: b, chart: b.tgz, image: "op:b"}]`), ""},

		{"autoscale cache", "instances: [{name: a, namespace: ns, maxMemoryPolicy: allkeys-lru, autoscale: {}}]",
			`instance "a": autoscale: an instance with autoscale needs maxMemoryPolicy noeviction`},
		{"autoscale bootstrap", "instances: [{name: s, namespace: ns, data: {fill: {sizeMi: 1}, ledger: {}}}, " +
			"{name: a, namespace: ns, maxMemoryPolicy: noeviction, bootstrap: {source: s}, autoscale: {}}]",
			`instance "a": autoscale: a bootstrapping instance has no master to sample`},
		{"autoscale memory kind", autoscale("autoscale: {}, mutations: {kinds: {maxmemory_policy: 1}}"),
			`instance "a": autoscale: maxmemory_policy changes the memory outside the rules of mode Auto`},
		{"autoscale load", "instances: [{name: a, namespace: ns, maxMemoryPolicy: noeviction, autoscale: {}, mutations: {kinds: {autoscale_load: 1}}}]",
			`instance "a": autoscale: autoscale_load needs data`},
		{"autoscale kill replica", "instances: [{name: a, namespace: ns, maxMemoryPolicy: noeviction, autoscale: {}, mutations: {kinds: {autoscale_kill_replica: 1}}}]",
			`instance "a": autoscale: autoscale_kill_replica needs data`},
		{"autoscale limit", autoscale("autoscale: {limit: {min: 32, max: 128}}, mutations: {kinds: {autoscale_limit: 1}}"),
			`instance "a": autoscale: limit: min must be at least 64`},
		{"autoscale memoryMax", autoscale("autoscale: {memoryMax: {min: 128, max: 128}}, mutations: {kinds: {autoscale_memory_max: 1}}"),
			`instance "a": autoscale: memoryMax: max must be greater than min`},
		{"autoscale memoryMax below limit", autoscale("autoscale: {limit: {min: 128, max: 256}, memoryMax: {min: 192, max: 384}}, " +
			"mutations: {kinds: {autoscale_limit: 1, autoscale_memory_max: 1}}"),
			`instance "a": autoscale: memoryMax.min must not be less than limit.max`},
		{"autoscale limit range unused", autoscale("autoscale: {limit: {min: 32, max: 16}}"), ""},
		{"autoscale", autoscale("autoscale: {limit: {min: 128, max: 256}, memoryMax: {min: 256, max: 384}}, " +
			"mutations: {kinds: {autoscale_limit: 1, autoscale_memory_max: 1}}"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			switch {
			case c.want == "" && err != nil:
				t.Errorf("error %v, want none", err)
			case c.want != "" && (err == nil || err.Error() != c.want):
				t.Errorf("error %v\nwant  %s", err, c.want)
			}
		})
	}
}

// validateTemplate rejects a sentinel chain on an operator instance first,
// because the chain sets its sentinelVersion. validateChain has the same
// check for a direct call.
func TestValidateChainSentinelMode(t *testing.T) {
	c, err := Parse([]byte(graph + in1))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{SentinelFollow, SentinelSeparate} {
		in := Instance{Name: "a", Mode: ModeOperator, Chain: &Chain{Start: []string{"redis-7.2"}, Versions: []string{"redis-7.2", "redis-7.4"}, Sentinel: s}}
		if err := c.validateChain(in); err == nil || err.Error() != "sentinel: "+s+" needs a sentinel instance" {
			t.Errorf("%s: %v", s, err)
		}
		in.Mode = ModeSentinel
		if err := c.validateChain(in); err != nil {
			t.Errorf("%s on a sentinel instance: %v", s, err)
		}
	}
}

const in1 = "instances: [{name: a, namespace: ns}]"

// Chaos.validate checks each duration.
func TestChaosValidateDurations(t *testing.T) {
	neg := metav1.Duration{Duration: -time.Second}
	for name, ch := range map[string]Chaos{
		"interval":      {Interval: neg},
		"jitter":        {Jitter: neg},
		"timeout":       {Timeout: neg},
		"drain hold":    {Drain: Drain{Hold: neg}},
		"drain timeout": {Drain: Drain{Timeout: neg}},
	} {
		if err := ch.validate(); err == nil || err.Error() != "negative duration" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := (Chaos{}).validate(); err != nil {
		t.Errorf("empty chaos: %v", err)
	}
}
