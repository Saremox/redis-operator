package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const graph = `
versions:
  - {name: redis-7.2, image: "redis:7.2.16-alpine"}
  - {name: redis-7.4, image: "redis:7.4.11-alpine"}
  - {name: redis-8, image: "redis:8.10.2-alpine"}
  - {name: valkey-7.2, image: "valkey/valkey:7.2.14-alpine"}
  - {name: valkey-8, image: "valkey/valkey:8.1.10-alpine"}
edges:
  - {from: redis-7.2, to: redis-7.4, expect: ok}
  - {from: redis-7.4, to: redis-8, expect: ok}
  - {from: redis-7.2, to: valkey-7.2, expect: ok}
  - {from: valkey-7.2, to: valkey-8, expect: ok}
  - {from: redis-7.4, to: valkey-7.2, expect: unknown}
  - {from: redis-8, to: valkey-8, expect: unknown}
`

func TestVersions(t *testing.T) {
	c, err := Parse([]byte(graph + `
mutation:
  timeouts:
    image_upgrade: {base: 3m, perPod: 90s}
instances:
  - name: chain
    namespace: ns
    mode: sentinel
    template: chain.yaml
    chain: {start: [redis-7.2], versions: [redis-7.2, redis-7.4, redis-8], sentinel: follow}
    mutations: {kinds: {image_upgrade: 1, reset: 1}}
  - name: edge
    namespace: ns
    template: edge.yaml
    chain: {start: [redis-7.4, redis-8], versions: [redis-7.4, redis-8, valkey-7.2, valkey-8], expect: [unknown]}
  - name: mixed
    namespace: ns
    mode: sentinel
    template: mixed.yaml
    version: redis-7.2
    sentinelVersion: valkey-8
`))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.VersionNamed("valkey-8"); v.Server != "valkey" || v.Release != "8.1.10" {
		t.Errorf("valkey-8 = %+v", v)
	}
	if v, ok := c.VersionOf("redis:7.4.11-alpine"); !ok || v.Name != "redis-7.4" || v.Server != "redis" || v.Release != "7.4.11" {
		t.Errorf("VersionOf = %+v, %t", v, ok)
	}
	chain, edge, mixed := c.Instances[0], c.Instances[1], c.Instances[2]
	if chain.Version != "redis-7.2" || chain.SentinelVersion != "redis-7.2" || edge.Version != "redis-7.4" || edge.SentinelVersion != "" {
		t.Errorf("chain versions: %q %q, edge versions: %q %q", chain.Version, chain.SentinelVersion, edge.Version, edge.SentinelVersion)
	}
	if mixed.Version != "redis-7.2" || mixed.SentinelVersion != "valkey-8" {
		t.Errorf("mixed versions: %q %q", mixed.Version, mixed.SentinelVersion)
	}
	names := func(es []Edge) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.String())
		}
		return out
	}
	// valkey-7.2 is not one of chain's versions.
	if got := names(c.EdgesFrom("redis-7.2", chain.Chain)); !slices.Equal(got, []string{"redis-7.2 -> redis-7.4"}) {
		t.Errorf("chain's edges from redis-7.2: %v", got)
	}
	if got := names(c.EdgesFrom("redis-8", chain.Chain)); len(got) != 0 {
		t.Errorf("chain's edges from redis-8: %v", got)
	}
	// Only unknown edges: not redis-7.4 -> redis-8.
	if got := names(c.EdgesFrom("redis-7.4", edge.Chain)); !slices.Equal(got, []string{"redis-7.4 -> valkey-7.2"}) {
		t.Errorf("edge's edges from redis-7.4: %v", got)
	}
	if !c.Reaches(chain.Chain, "redis-7.2", "redis-8") || c.Reaches(chain.Chain, "redis-8", "redis-7.2") {
		t.Error("Reaches")
	}
	if d := c.Mutation.Timeout(ImageUpgrade, c.Observer, 2); d != 6*time.Minute {
		t.Errorf("image_upgrade timeout for 2 pods = %s", d)
	}
	if d := c.Mutation.Timeout(Reset, c.Observer, 3); d != 13*time.Minute {
		t.Errorf("reset timeout for 3 pods = %s", d)
	}
	if d := c.Mutation.Timeout(KillMaster, c.Observer, 3); d != 10*time.Minute {
		t.Errorf("kill_master timeout = %s", d)
	}
}

func TestInvalidVersions(t *testing.T) {
	chain := func(spec string) string {
		return graph + "instances: [{name: a, namespace: ns, mode: sentinel, template: a.yaml, chain: " + spec + "}]"
	}
	cases := map[string]string{
		"floating tag":     "versions: [{name: redis-7, image: \"redis:7-alpine\"}]\ninstances: [{name: a, namespace: ns}]",
		"duplicate name":   "versions: [{name: r, image: \"redis:7.2.16-alpine\"}, {name: r, image: \"redis:7.4.11-alpine\"}]\ninstances: [{name: a, namespace: ns}]",
		"duplicate image":  "versions: [{name: a, image: \"redis:7.2.16-alpine\"}, {name: b, image: \"redis:7.2.16-alpine\"}]\ninstances: [{name: a, namespace: ns}]",
		"unknown from":     graph + "  - {from: redis-6, to: redis-7.2, expect: ok}\ninstances: [{name: a, namespace: ns}]",
		"self edge":        graph + "  - {from: redis-8, to: redis-8, expect: ok}\ninstances: [{name: a, namespace: ns}]",
		"downgrade":        graph + "  - {from: redis-8, to: redis-7.4, expect: fail}\ninstances: [{name: a, namespace: ns}]",
		"bad expect":       graph + "  - {from: redis-7.2, to: redis-8, expect: maybe}\ninstances: [{name: a, namespace: ns}]",
		"duplicate edge":   graph + "  - {from: redis-7.2, to: redis-7.4, expect: unknown}\ninstances: [{name: a, namespace: ns}]",
		"unknown start":    chain("{start: [redis-6], versions: [redis-7.2]}"),
		"start not listed": chain("{start: [redis-7.2], versions: [redis-7.4, redis-8]}"),
		"dead start":       chain("{start: [redis-8], versions: [redis-8]}"),
		"unreachable":      chain("{start: [redis-7.2], versions: [redis-7.2, redis-7.4, valkey-8]}"),
		"no edge expected": chain("{start: [redis-7.2], versions: [redis-7.2, redis-7.4], expect: [unknown]}"),
		"bad sentinel":     chain("{start: [redis-7.2], versions: [redis-7.2, redis-7.4], sentinel: lead}"),
		"no template":      graph + "instances: [{name: a, namespace: ns, chain: {start: [redis-7.2], versions: [redis-7.2, redis-7.4]}}]",
		"sentinel operator": graph +
			"instances: [{name: a, namespace: ns, template: a.yaml, chain: {start: [redis-7.2], versions: [redis-7.2, redis-7.4], sentinel: follow}}]",
		"image_upgrade without chain": graph + "instances: [{name: a, namespace: ns, mutations: {kinds: {image_upgrade: 1}}}]",
		"sentinel upgrade following": graph + "instances: [{name: a, namespace: ns, mode: sentinel, template: a.yaml, " +
			"chain: {start: [redis-7.2], versions: [redis-7.2, redis-7.4], sentinel: follow}, mutations: {kinds: {sentinel_image_upgrade: 1}}}]",
		"reset without template": "instances: [{name: a, namespace: ns, mutations: {kinds: {reset: 1}}}]",
		"unknown version":        graph + "instances: [{name: a, namespace: ns, template: a.yaml, version: redis-6}]",
		"version and chain": graph +
			"instances: [{name: a, namespace: ns, template: a.yaml, version: redis-7.4, chain: {start: [redis-7.2], versions: [redis-7.2, redis-7.4]}}]",
		"bad timeout": "mutation: {timeouts: {kill_all: {base: 1m}}}\ninstances: [{name: a, namespace: ns}]",
		"flip on one version": graph +
			"instances: [{name: a, namespace: ns, mode: sentinel, mutations: {kinds: {sentinel_image_flip: 1}, sentinelImages: [redis-8, redis-8]}}]",
		"flip to an unknown version": graph +
			"instances: [{name: a, namespace: ns, mode: sentinel, mutations: {kinds: {sentinel_image_flip: 1}, sentinelImages: [redis-8, valkey-6]}}]",
		"flip without Sentinel": graph +
			"instances: [{name: a, namespace: ns, mutations: {kinds: {sentinel_image_flip: 1}, sentinelImages: [redis-8, valkey-8]}}]",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestTemplateRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte("instances: [{name: a, namespace: ns, template: templates/a.yaml}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Instances[0].Template; got != dir+"/templates/a.yaml" {
		t.Errorf("template = %s, want it relative to the config", got)
	}
}

// The server and release come from the image's repository name and tag,
// whatever its registry's port, and without its digest.
func TestVersionDefaults(t *testing.T) {
	for image, want := range map[string][2]string{
		"redis:7.2.16-alpine":                                             {"redis", "7.2.16"},
		"valkey/valkey:9.1.2-alpine":                                      {"valkey", "9.1.2"},
		"registry.example:5000/team/redis:7.2.16":                         {"redis", "7.2.16"},
		"registry.example:5000/team/valkey:8.1.10-alpine@sha256:0123abcd": {"valkey", "8.1.10"},
	} {
		v := Version{Name: "v", Image: image}
		v.setDefaults()
		if got := [2]string{v.Server, v.Release}; got != want {
			t.Errorf("%s: server and release %v, want %v", image, got, want)
		}
	}
}

// Redis before 7.0 does not wait for its replicas on SIGTERM.
func TestWaitsForReplicas(t *testing.T) {
	for image, want := range map[string]bool{
		"redis:6.2.24-alpine":  false,
		"redis:6.2.9":          false,
		"redis:7.0.0":          true,
		"redis:7.2.16-alpine":  true,
		"redis:8.10.2-alpine":  true,
		"valkey/valkey:7.2.14": true,
		"valkey/valkey:8.1.10": true,
	} {
		v := Version{Name: "v", Image: image}
		v.setDefaults()
		if got := v.WaitsForReplicas(); got != want {
			t.Errorf("%s waits for replicas: %t, want %t", image, got, want)
		}
	}
}

// The shipped config has the Redis 6.2 start of the upgrade chains: a pinned
// image, the edge to Redis 7.2, and the two instances.
func TestShippedRedis62(t *testing.T) {
	c, err := Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := c.VersionNamed("redis-6.2")
	if !ok || v.Server != "redis" || !strings.HasPrefix(v.Release, "6.2.") || v.WaitsForReplicas() {
		t.Fatalf("redis-6.2 = %+v, %t", v, ok)
	}
	if e, ok := c.Edge("redis-6.2", "redis-7.2"); !ok || e.Expect != ExpectOK {
		t.Errorf("edge redis-6.2 -> redis-7.2 = %+v, %t", e, ok)
	}
	if len(c.EdgesFrom("redis-6.2", &Chain{Versions: []string{"redis-6.2", "redis-7.2", "valkey-9"}})) != 1 {
		t.Error("redis-6.2 must have the one edge to redis-7.2")
	}
	want := []string{"redis-6.2", "redis-7.2", "redis-7.4", "redis-8"}
	for _, name := range []string{"redis-chain-62", "redis-chain-62-sent"} {
		i := slices.IndexFunc(c.Instances, func(in Instance) bool { return in.Name == name })
		if i < 0 {
			t.Fatalf("%s is not in deploy/config.yaml", name)
		}
		in := c.Instances[i]
		if !slices.Equal(in.Chain.Start, []string{"redis-6.2"}) || !slices.Equal(in.Chain.Versions, want) || in.Version != "redis-6.2" {
			t.Errorf("%s chain %+v, version %s", name, in.Chain, in.Version)
		}
		if !c.Reaches(in.Chain, "redis-6.2", "redis-8") {
			t.Errorf("%s does not reach redis-8", name)
		}
		if _, ok := in.Mutations.Kinds[ImageUpgrade]; !ok {
			t.Errorf("%s does not run image_upgrade", name)
		}
	}
}
