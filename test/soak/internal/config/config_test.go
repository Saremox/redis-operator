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
	if len(c.Instances) != 2 || c.Instances[0].Name != "op-basic" || c.Instances[1].Mode != ModeSentinel {
		t.Fatalf("unexpected instances: %+v", c.Instances)
	}
}

func TestShippedConfigsParse(t *testing.T) {
	for _, path := range []string{"../../deploy/config.yaml", "../../e2e/config.yaml"} {
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
