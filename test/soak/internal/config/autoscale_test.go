package config

import (
	"strings"
	"testing"
)

const autoscaleKinds = "{autoscale_load: 1, autoscale_limit: 1, autoscale_memory_max: 1, autoscale_mode: 1, " +
	"autoscale_state_lost: 1, autoscale_kill_master: 1, autoscale_kill_replica: 1}"

func TestAutoscale(t *testing.T) {
	c, err := Parse([]byte(`instances: [{name: a, namespace: ns, maxMemoryPolicy: noeviction, data: {fill: {percent: 10}, ledger: {}},
  autoscale: {limit: {min: 128, max: 192}, memoryMax: {min: 192, max: 384}},
  mutations: {kinds: ` + autoscaleKinds + `}}]`))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Instances[0].Autoscale
	if a == nil || a.Limit != (Range{Min: 128, Max: 192}) || a.MemoryMax != (Range{Min: 192, Max: 384}) {
		t.Errorf("%+v", a)
	}
	if !AutoscaleOnly(AutoscaleKillReplica) || AutoscaleOnly(KillReplica) {
		t.Error("AutoscaleOnly")
	}
}

func TestInvalidAutoscale(t *testing.T) {
	const data = "data: {fill: {percent: 10}}, "
	for name, c := range map[string]struct{ instance, want string }{
		"kind without autoscale": {"maxMemoryPolicy: noeviction, mutations: {kinds: {autoscale_mode: 1}}", "needs an instance with autoscale"},
		"cache":                  {"maxMemoryPolicy: allkeys-lru, autoscale: {}", "noeviction"},
		"no maxMemory":           {"autoscale: {}", "noeviction"},
		"memory kind":            {"maxMemoryPolicy: noeviction, autoscale: {}, mutations: {kinds: {redis_memory: 1}, redisMemory: {min: 128, max: 256}}", "outside the rules"},
		"load without data":      {"maxMemoryPolicy: noeviction, autoscale: {}, mutations: {kinds: {autoscale_load: 1}}", "needs data"},
		"kill without data":      {"maxMemoryPolicy: noeviction, autoscale: {}, mutations: {kinds: {autoscale_kill_master: 1}}", "needs data"},
		"limit without range":    {data + "maxMemoryPolicy: noeviction, autoscale: {}, mutations: {kinds: {autoscale_limit: 1}}", "limit"},
		"small limit":            {data + "maxMemoryPolicy: noeviction, autoscale: {limit: {min: 32, max: 128}}, mutations: {kinds: {autoscale_limit: 1}}", "at least 64"},
		"memoryMax without range": {data + "maxMemoryPolicy: noeviction, autoscale: {}, mutations: {kinds: {autoscale_memory_max: 1}}",
			"memoryMax"},
		"memoryMax below limit": {data + "maxMemoryPolicy: noeviction, autoscale: {limit: {min: 128, max: 256}, memoryMax: {min: 192, max: 384}}, " +
			"mutations: {kinds: {autoscale_limit: 1, autoscale_memory_max: 1}}", "memoryMax.min"},
	} {
		_, err := Parse([]byte("instances: [{name: a, namespace: ns, " + c.instance + "}]"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// A bootstrapping instance has no master.
	_, err := Parse([]byte(`instances:
- {name: src, namespace: ns, data: {fill: {sizeMi: 1}, ledger: {}}}
- {name: a, namespace: ns, maxMemoryPolicy: noeviction, bootstrap: {source: src}, autoscale: {}}`))
	if err == nil || !strings.Contains(err.Error(), "bootstrapping") {
		t.Errorf("bootstrap: %v", err)
	}
}
