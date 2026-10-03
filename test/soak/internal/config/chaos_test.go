package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestChaos(t *testing.T) {
	c, err := Parse([]byte(`
observer: {convergenceTimeout: 7m}
chaos:
  kinds: {operator_restart: 1, node_drain: 2}
instances: [{name: a, namespace: ns, mutations: {kinds: {kill_master: 1}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	ch := c.Chaos
	if !c.ChaosOn() || ch.Interval.Duration != 10*time.Minute || ch.Timeout.Duration != 7*time.Minute {
		t.Errorf("chaos defaults: %+v", ch)
	}
	if ch.Upgrade.Release != "redis-operator" || ch.Upgrade.HookJob != "redis-operator-crds-upgrade" || ch.Upgrade.Helm != "helm" ||
		ch.Upgrade.CRD != "redisfailovers.databases.spotahome.com" || c.Operator.Lease != "redis-failover-lease" {
		t.Errorf("upgrade defaults: %+v, operator %+v", ch.Upgrade, c.Operator)
	}
	if ch.Drain.NodeSelector != "!node-role.kubernetes.io/control-plane" || ch.Drain.Hold.Duration != time.Minute || ch.Drain.Timeout.Duration != 5*time.Minute {
		t.Errorf("drain defaults: %+v", ch.Drain)
	}
	want := []string{"failover", "kill_master", "node_drain", "operator_restart", "periodic", "reset"}
	if got := c.Events(c.Instances[0]); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}

	c, err = Parse([]byte("mutation: {enabled: false}\nchaos: {kinds: {operator_restart: 1}}\ninstances: [{name: a, namespace: ns}]"))
	if err != nil || c.ChaosOn() {
		t.Errorf("chaos on with mutation off: %v", err)
	}
}

func TestOperatorVersionImage(t *testing.T) {
	v := OperatorVersion{Image: "localhost:5001/saremox/redis-operator:4.2.0-rc2"}
	if v.Repository() != "localhost:5001/saremox/redis-operator" || v.Tag() != "4.2.0-rc2" {
		t.Errorf("%s : %s", v.Repository(), v.Tag())
	}
}

func TestInvalidChaos(t *testing.T) {
	const in = "\ninstances: [{name: a, namespace: ns}]"
	upgrade := func(versions string) string {
		return "chaos: {kinds: {operator_upgrade: 1}, upgrade: {versions: " + versions + "}}" + in
	}
	cases := map[string]string{
		"unknown kind":        "chaos: {kinds: {delete_cluster: 1}}" + in,
		"zero weight":         "chaos: {kinds: {node_drain: 0}}" + in,
		"negative hold":       "chaos: {kinds: {node_drain: 1}, drain: {hold: -1s}}" + in,
		"one version":         upgrade("[{name: a, chart: a.tgz, image: \"op:a\"}]"),
		"no image tag":        upgrade("[{name: a, chart: a.tgz, image: \"op:a\"}, {name: b, chart: b.tgz, image: \"localhost:5001/op\"}]"),
		"no chart":            upgrade("[{name: a, chart: a.tgz, image: \"op:a\"}, {name: b, image: \"op:b\"}]"),
		"duplicate name":      upgrade("[{name: a, chart: a.tgz, image: \"op:a\"}, {name: a, chart: b.tgz, image: \"op:b\"}]"),
		"duplicate image":     upgrade("[{name: a, chart: a.tgz, image: \"op:a\"}, {name: b, chart: b.tgz, image: \"op:a\"}]"),
		"unknown chaos field": "chaos: {kinds: {node_drain: 1}, drain: {nodes: [w1]}}" + in,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// Local charts are relative to the config file, like templates; OCI
// references stay as they are.
func TestChartsRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
chaos:
  kinds: {operator_upgrade: 1}
  upgrade:
    versions:
      - {name: rc1, chart: "oci://ghcr.io/saremox/redis-operator/charts/redis-operator", version: 4.2.0-rc1, image: "ghcr.io/saremox/redis-operator:4.2.0-rc1"}
      - {name: main, chart: redis-operator-main.tgz, image: "redis-operator:main"}
instances: [{name: a, namespace: ns}]`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	v := c.Chaos.Upgrade.Versions
	if v[0].Chart != "oci://ghcr.io/saremox/redis-operator/charts/redis-operator" || v[1].Chart != filepath.Join(dir, "redis-operator-main.tgz") {
		t.Errorf("charts %s, %s", v[0].Chart, v[1].Chart)
	}
}
