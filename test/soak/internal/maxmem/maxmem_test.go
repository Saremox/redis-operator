package maxmem

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

const mi = 1 << 20

func rf(limit, policy string, percent int32, customConfig ...string) *redisfailoverv1.RedisFailover {
	r := &redisfailoverv1.RedisFailover{Spec: redisfailoverv1.RedisFailoverSpec{Redis: redisfailoverv1.RedisSettings{
		Replicas:     3,
		CustomConfig: customConfig,
		MaxMemory:    &redisfailoverv1.MaxMemorySettings{Percent: percent, Policy: policy},
	}}}
	if limit != "" {
		r.Spec.Redis.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}
	}
	_ = r.Validate()
	return r
}

func pods(maxmemory int64, policy string, limits ...int64) []Pod {
	var ps []Pod
	for i, l := range limits {
		ps = append(ps, Pod{Name: "rfr-x-" + string(rune('0'+i)), Limit: l, MaxMemory: maxmemory, Policy: policy})
	}
	return ps
}

func TestExpectReadme(t *testing.T) {
	// The operator README's table, at the default 75%.
	for limit, want := range map[string]int64{"64Mi": 32 * mi, "96Mi": 64 * mi, "128Mi": 96 * mi, "1Gi": 768 * mi} {
		r := rf(limit, "", 0)
		q := resource.MustParse(limit)
		got, kept, ok := Expect(r, "", pods(0, "", q.Value(), q.Value(), q.Value()))
		if !ok || got != want || kept != "" {
			t.Errorf("%s: %d %q %v, want %d", limit, got, kept, ok, want)
		}
		if err := Check(r, "", pods(want, "noeviction", q.Value(), q.Value(), q.Value())); err != nil {
			t.Errorf("%s: %v", limit, err)
		}
	}
}

func TestCheck(t *testing.T) {
	const l192, l256 = 192 * mi, 256 * mi
	keptMsg := "maxmemory kept at 144.0Mi: lowering it to 96.0Mi would not fit the memory in use under policy noeviction"
	cases := []struct {
		name    string
		rf      *redisfailoverv1.RedisFailover
		message string
		pods    []Pod
		ok      bool
	}{
		{"applied", rf("192Mi", "noeviction", 75), "", pods(144*mi, "noeviction", l192, l192, l192), true},
		{"wrong policy", rf("192Mi", "volatile-lru", 75), "", pods(144*mi, "noeviction", l192, l192, l192), false},
		{"wrong maxmemory", rf("192Mi", "noeviction", 75), "", pods(100*mi, "noeviction", l192, l192, l192), false},
		{"unset after a restart", rf("192Mi", "noeviction", 75), "", append(pods(144*mi, "noeviction", l192, l192), pods(0, "noeviction", l192)...), false},
		{"percent", rf("192Mi", "noeviction", 50), "", pods(96*mi, "noeviction", l192, l192, l192), true},
		{"reserve beats percent", rf("192Mi", "noeviction", 95), "", pods(160*mi, "noeviction", l192, l192, l192), true},
		// A raised limit applies once every pod runs with it.
		{"raised, a pod not resized yet", rf("256Mi", "noeviction", 75), "", pods(144*mi, "noeviction", l192, l256, l256), true},
		{"raised early", rf("256Mi", "noeviction", 75), "", pods(192*mi, "noeviction", l192, l256, l256), false},
		// A lowered one applies before the pods are resized.
		{"lowered before the pods", rf("128Mi", "allkeys-lru", 75), "", pods(96*mi, "allkeys-lru", l192, l192, l192), true},
		{"kept below the data", rf("128Mi", "noeviction", 75), keptMsg, pods(144*mi, "noeviction", l192, l192, l192), true},
		{"kept, other value", rf("128Mi", "noeviction", 75), keptMsg, pods(150*mi, "noeviction", l192, l192, l192), false},
		{"kept, lowered on a replica", rf("128Mi", "noeviction", 75), keptMsg,
			append(pods(144*mi, "noeviction", l192, l192), pods(96*mi, "noeviction", l192)...), false},
		{"kept without a message", rf("128Mi", "noeviction", 75), "", pods(144*mi, "noeviction", l192, l192, l192), false},
		{"kept under volatile", rf("128Mi", "volatile-lru", 75),
			"maxmemory kept at 144.0Mi: lowering it to 96.0Mi would not fit the memory in use under policy volatile-lru",
			pods(144*mi, "volatile-lru", l192, l192, l192), true},
		{"allkeys is never kept below the data", rf("128Mi", "allkeys-lru", 75),
			"maxmemory kept at 144.0Mi: lowering it to 96.0Mi would not fit the memory in use under policy allkeys-lru",
			pods(144*mi, "allkeys-lru", l192, l192, l192), false},
		{"allkeys kept as the master changed", rf("128Mi", "allkeys-lru", 75),
			"maxmemory kept at 144.0Mi: 10.0.0.10 stopped being the master while lowering it",
			pods(144*mi, "allkeys-lru", l192, l192, l192), true},
		{"allkeys evicting", rf("128Mi", "allkeys-lru", 75), "maxmemory lowered to 96.0Mi, evicting down to it",
			pods(96*mi, "allkeys-lru", 128*mi, 128*mi, 128*mi), true},
		{"customConfig", rf("192Mi", "allkeys-lru", 75, "maxmemory 100mb", "maxmemory-policy volatile-ttl"), "",
			pods(100*mi, "volatile-ttl", l192, l192, l192), true},
		{"customConfig ignored", rf("192Mi", "allkeys-lru", 75, "maxmemory 100mb"), "", pods(144*mi, "allkeys-lru", l192, l192, l192), false},
		{"not managed without a limit", rf("", "noeviction", 75), "", pods(0, "", 0, 0, 0), true},
		{"not managed below 64Mi", rf("32Mi", "noeviction", 75), "", pods(0, "", 32*mi, 32*mi, 32*mi), true},
		{"a pod below 64Mi", rf("192Mi", "noeviction", 75), "", pods(0, "noeviction", 32*mi, l192, l192), true},
		{"unreachable pod", rf("192Mi", "noeviction", 75), "",
			append(pods(144*mi, "noeviction", l192, l192), Pod{Name: "rfr-x-2", Limit: l192, Err: errors.New("refused")}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Check(c.rf, c.message, c.pods)
			if (err == nil) != c.ok {
				t.Errorf("Check = %v, want ok %v", err, c.ok)
			}
		})
	}
	if err := Check(&redisfailoverv1.RedisFailover{}, "", pods(0, "", l192)); err != nil {
		t.Errorf("without maxMemory: %v", err)
	}
}

func TestKeptBelowData(t *testing.T) {
	msg := "maxmemory kept at 144.0Mi: lowering it to 96.0Mi would not fit the memory in use under policy noeviction"
	if !KeptBelowData(msg, 96*mi) || KeptBelowData(msg, 120*mi) || KeptBelowData("maxmemory lowered to 96.0Mi, evicting down to it", 96*mi) {
		t.Error("KeptBelowData")
	}
}

func TestParseMemory(t *testing.T) {
	for s, want := range map[string]int64{"1000": 1000, "1k": 1000, "1kb": 1024, "100MB": 100 * mi, "2g": 2e9, "1gb": 1 << 30} {
		if got, err := ParseMemory(s); err != nil || got != want {
			t.Errorf("%s: %d %v, want %d", s, got, err, want)
		}
	}
	if _, err := ParseMemory("lots"); err == nil {
		t.Error("parsed lots")
	}
}
