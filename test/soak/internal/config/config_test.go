package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadExample(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Instances) != 1 || c.Instances[0].Name != "op-basic" {
		t.Fatalf("unexpected instances: %+v", c.Instances)
	}
}

func TestDeployConfigParses(t *testing.T) {
	b, err := os.ReadFile("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b); err != nil {
		t.Fatal(err)
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
	if c.Operator.Namespace != "redis-operator" || c.Operator.Deployment != "redis-operator" {
		t.Errorf("operator defaults: %+v", c.Operator)
	}
	in := c.Instances[0]
	if in.Mode != ModeOperator || in.Port != 6379 {
		t.Errorf("instance defaults: %+v", in)
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
