package customconfig

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

func rfWith(redis, sentinel []string, maxMemory bool) *redisfailoverv1.RedisFailover {
	rf := &redisfailoverv1.RedisFailover{}
	rf.Spec.Redis.CustomConfig = redis
	rf.Spec.Sentinel.CustomConfig = sentinel
	if maxMemory {
		rf.Spec.Redis.MaxMemory = &redisfailoverv1.MaxMemorySettings{}
		rf.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
	}
	_ = rf.Validate()
	return rf
}

func TestRedis(t *testing.T) {
	rf := rfWith([]string{
		"hz 15", "maxmemory-samples 7", "requirepass nope", "aclfile /data/users.acl",
		"maxmemory 100mb", "replica-ignore-maxmemory yes", `save ""`, "HZ 20",
	}, nil, false)
	keys := RedisKeys(rf)
	// The operator's default replica-priority comes first; requirepass and
	// aclfile aren't the customConfig's to set; the later hz wins.
	want := []string{"replica-priority", "maxmemory-samples", "maxmemory", "replica-ignore-maxmemory", "save", "hz"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	got := map[string]string{
		"replica-priority": "100", "maxmemory-samples": "7", "maxmemory": "104857600",
		"replica-ignore-maxmemory": "yes", "save": "", "hz": "20",
	}
	if err := Redis(rf, got); err != nil {
		t.Errorf("applied: %v", err)
	}
	got["hz"] = "10"
	delete(got, "save")
	err := Redis(rf, got)
	if err == nil || !strings.Contains(err.Error(), `hz is "10", customConfig sets "20"`) || !strings.Contains(err.Error(), "save not reported") {
		t.Errorf("not applied: %v", err)
	}

	// With maxMemory, the maxmem check owns maxmemory and the operator sets
	// replica-ignore-maxmemory itself.
	rf = rfWith([]string{"maxmemory 100mb", "maxmemory-policy allkeys-lru", "slave-ignore-maxmemory yes", "hz 20"}, nil, true)
	if keys := RedisKeys(rf); !slices.Equal(keys, []string{"replica-priority", "hz"}) {
		t.Errorf("keys with maxMemory: %v", keys)
	}

	// A bootstrapping instance never becomes the master.
	rf = rfWith(nil, nil, false)
	rf.Spec.BootstrapNode = &redisfailoverv1.BootstrapSettings{Host: "10.0.0.1"}
	rf.Spec.Redis.CustomConfig = nil
	_ = rf.Validate()
	if err := Redis(rf, map[string]string{"replica-priority": "100"}); err == nil {
		t.Error("a bootstrapping pod may be promoted")
	}
}

// sentinelMaster is a SENTINEL MASTER mymaster reply of Redis 7.2.
var sentinelMaster = map[string]string{
	"name": "mymaster", "ip": "10.244.0.11", "port": "6380", "flags": "master",
	"down-after-milliseconds": "4000", "info-refresh": "2098", "role-reported": "master",
	"config-epoch": "0", "num-slaves": "1", "num-other-sentinels": "2", "quorum": "2",
	"failover-timeout": "9000", "parallel-syncs": "1",
}

func TestSentinel(t *testing.T) {
	rf := rfWith(nil, []string{
		"down-after-milliseconds 4000", "failover-timeout 9000", "parallel-syncs 1", "auth-pass secret", "quorum 2",
	}, false)
	// auth-pass isn't reported, so it can't be checked.
	if err := Sentinel(rf, sentinelMaster); err != nil {
		t.Errorf("applied: %v", err)
	}
	fields := map[string]string{}
	for k, v := range sentinelMaster {
		fields[k] = v
	}
	fields["down-after-milliseconds"] = "1000"
	fields["parallel-syncs"] = "2"
	err := Sentinel(rf, fields)
	if err == nil || !strings.Contains(err.Error(), `down-after-milliseconds is "1000"`) || !strings.Contains(err.Error(), `parallel-syncs is "2"`) {
		t.Errorf("not applied: %v", err)
	}
	// Without customConfig, the operator's defaults apply.
	if err := Sentinel(rfWith(nil, nil, false), sentinelMaster); err == nil ||
		!strings.Contains(err.Error(), `down-after-milliseconds is "4000", customConfig sets "5000"`) {
		t.Errorf("defaults: %v", err)
	}
}
