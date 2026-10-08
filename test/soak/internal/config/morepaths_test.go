package config

import (
	"maps"
	"slices"
	"testing"
)

// The shipped graph has the skips and the downgrades in two groups, and only
// the instances that name a group take them.
func TestShippedMorePaths(t *testing.T) {
	c, err := Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	grouped := map[string][]string{}
	for _, e := range c.Edges {
		if e.Group != "" {
			grouped[e.Group] = append(grouped[e.Group], e.String()+" "+e.Expect)
		}
	}
	want := map[string][]string{
		"skip": {"redis-6.2 -> redis-7.4 ok", "redis-6.2 -> redis-8 ok", "redis-7.2 -> redis-8 ok", "valkey-7.2 -> valkey-9 ok"},
		"downgrade": {
			"redis-7.4 -> redis-7.2 unknown", "redis-8 -> redis-7.4 unknown", "redis-8 -> redis-7.2 unknown",
			"valkey-8 -> valkey-7.2 unknown", "valkey-9 -> valkey-8 unknown", "valkey-9 -> valkey-7.2 unknown",
		},
	}
	for group, edges := range want {
		if !slices.Equal(grouped[group], edges) {
			t.Errorf("group %s: %v, want %v", group, grouped[group], edges)
		}
	}
	instance := func(name string) Instance {
		i := slices.IndexFunc(c.Instances, func(in Instance) bool { return in.Name == name })
		if i < 0 {
			t.Fatalf("%s is not in deploy/config.yaml", name)
		}
		return c.Instances[i]
	}
	taken := func(in Instance) []string {
		var out []string
		for _, v := range in.Chain.Versions {
			for _, e := range c.EdgesFrom(v, in.Chain) {
				out = append(out, e.String()+" "+e.Expect)
			}
		}
		return out
	}
	for _, in := range c.Instances {
		if in.Chain == nil || in.Chain.Group != "" {
			continue
		}
		for _, v := range in.Chain.Versions {
			for _, e := range c.EdgesFrom(v, in.Chain) {
				if e.Group != "" {
					t.Errorf("%s takes the edge %s of the group %s", in.Name, e, e.Group)
				}
			}
		}
	}
	for _, group := range []string{"skip", "downgrade"} {
		in := instance(group)
		if in.Chain.Group != group || in.Mode != ModeOperator || !slices.Equal(taken(in), want[group]) {
			t.Errorf("%s: group %s, mode %s, edges %v", group, in.Chain.Group, in.Mode, taken(in))
		}
	}
	// The Sentinel twin of edge: the same versions and edges.
	edge, sent := instance("edge"), instance("edge-sent")
	if sent.Mode != ModeSentinel || sent.Chain.Sentinel != SentinelSeparate || sent.SentinelVersion != "redis-7.4" ||
		!slices.Equal(sent.Chain.Start, edge.Chain.Start) || !slices.Equal(sent.Chain.Versions, edge.Chain.Versions) ||
		!slices.Equal(sent.Chain.Expect, []string{ExpectUnknown}) || !slices.Equal(taken(sent), taken(edge)) {
		t.Errorf("edge-sent chain %+v, mode %s", sent.Chain, sent.Mode)
	}
	big, chain := instance("redis-chain-big"), instance("redis-chain")
	if big.Data.Fill.SizeMi != 256 || big.Data.Ledger == nil || !maps.Equal(big.Mutations.Kinds, chain.Mutations.Kinds) ||
		!slices.Equal(big.Chain.Versions, chain.Chain.Versions) || !slices.Equal(big.Chain.Start, chain.Chain.Start) {
		t.Errorf("redis-chain-big: %+v %+v", big.Data, big.Chain)
	}
}
