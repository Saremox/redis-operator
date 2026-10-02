package observer

import (
	"errors"
	"maps"
	"slices"
	"testing"
)

func replicationInfo(role, masterHost, link string, offset string) info {
	i := info{"role": role, "master_repl_offset": offset}
	if role == "slave" {
		i["master_host"], i["master_port"], i["master_link_status"] = masterHost, "6379", link
		i["slave_repl_offset"] = offset
	}
	return i
}

// healthy returns a converged three-pod instance whose master is rfr-x-0.
func healthy(sentinel bool) snapshot {
	s := snapshot{
		sentinel:         sentinel,
		redisReplicas:    3,
		sentinelReplicas: 3,
		port:             6379,
		state:            "Healthy",
		endpoints:        []string{"10.0.0.10"},
	}
	s.redis = []redisPod{
		{pod: pod{Name: "rfr-x-0", UID: "u0", IP: "10.0.0.10", Ready: true}, info: replicationInfo("master", "", "", "1000")},
		{pod: pod{Name: "rfr-x-1", UID: "u1", IP: "10.0.0.11", Ready: true}, info: replicationInfo("slave", "10.0.0.10", "up", "990")},
		{pod: pod{Name: "rfr-x-2", UID: "u2", IP: "10.0.0.12", Ready: true}, info: replicationInfo("slave", "10.0.0.10", "up", "1000")},
	}
	if sentinel {
		for _, n := range []string{"a", "b", "c"} {
			s.sentinels = append(s.sentinels, sentinelPod{pod: pod{Name: "rfs-x-" + n, Ready: true}, master: "10.0.0.10:6379"})
		}
	}
	return s
}

func violated(checks []check) []string {
	var v []string
	for _, c := range checks {
		if c.err != nil {
			v = append(v, c.invariant)
		}
	}
	return v
}

func TestEvaluate(t *testing.T) {
	noMaster := []string{invOneMaster, invMasterService, invReplication}
	cases := []struct {
		name     string
		sentinel bool
		change   func(*snapshot)
		want     []string
	}{
		{"healthy", false, func(*snapshot) {}, nil},
		{"healthy sentinel", true, func(*snapshot) {}, nil},
		{"pod missing", false, func(s *snapshot) {
			s.redis = s.redis[:2]
		}, []string{invPods}},
		{"pod not ready", false, func(s *snapshot) {
			s.redis[2].Ready = false
		}, []string{invPods}},
		{"sentinel missing", true, func(s *snapshot) {
			s.sentinels = s.sentinels[:2]
		}, []string{invPods}},
		{"sentinel not ready", true, func(s *snapshot) {
			s.sentinels[0].Ready = false
		}, []string{invPods}},
		{"master unreachable", false, func(s *snapshot) {
			s.redis[0].info, s.redis[0].err = nil, errors.New("connection refused")
		}, noMaster},
		{"two masters", false, func(s *snapshot) {
			s.redis[1].info = replicationInfo("master", "", "", "990")
		}, noMaster},
		{"two masters sentinel", true, func(s *snapshot) {
			s.redis[1].info = replicationInfo("master", "", "", "990")
		}, append(noMaster, invSentinelAgreement)},
		{"valkey roles", false, func(s *snapshot) {
			s.redis[0].info["role"] = "primary"
			s.redis[1].info["role"] = "replica"
		}, nil},
		{"no endpoint", false, func(s *snapshot) {
			s.endpoints = nil
		}, []string{invMasterService}},
		{"two endpoints", false, func(s *snapshot) {
			s.endpoints = append(s.endpoints, "10.0.0.11")
		}, []string{invMasterService}},
		{"endpoint on a replica", false, func(s *snapshot) {
			s.endpoints = []string{"10.0.0.11"}
		}, []string{invMasterService}},
		{"link down", false, func(s *snapshot) {
			s.redis[1].info["master_link_status"] = "down"
		}, []string{invReplication}},
		{"replicates from the old master", false, func(s *snapshot) {
			s.redis[2].info["master_host"] = "10.0.0.9"
		}, []string{invReplication}},
		{"replicates from the wrong port", false, func(s *snapshot) {
			s.redis[2].info["master_port"] = "6380"
		}, []string{invReplication}},
		{"replica unreachable", false, func(s *snapshot) {
			s.redis[2].info, s.redis[2].err = nil, errNoIP
		}, []string{invReplication}},
		{"sentinel disagrees", true, func(s *snapshot) {
			s.sentinels[1].master = "10.0.0.11:6379"
		}, []string{invSentinelAgreement}},
		{"sentinel unreachable", true, func(s *snapshot) {
			s.sentinels[2].err = errors.New("i/o timeout")
		}, []string{invSentinelAgreement}},
		{"ready replica that never synced", false, func(s *snapshot) {
			s.redis[1].info["master_link_status"] = "down"
			s.redis[1].info["master_link_down_since_seconds"] = "-1"
		}, []string{invReplication, invReplicaReadyWithoutData}},
		{"replica that never synced, not ready", false, func(s *snapshot) {
			s.redis[1].Ready = false
			s.redis[1].info["master_link_status"] = "down"
			s.redis[1].info["master_link_down_since_seconds"] = "-1"
		}, []string{invPods, invReplication}},
		{"ready replica whose link just dropped", false, func(s *snapshot) {
			s.redis[1].info["master_link_status"] = "down"
			s.redis[1].info["master_link_down_since_seconds"] = "8"
		}, []string{invReplication}},
		{"not healthy", false, func(s *snapshot) {
			s.state, s.message = "NotHealthy", "unable to apply the configured password"
		}, []string{invHealthy}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := healthy(c.sentinel)
			c.change(&s)
			checks := evaluate(s)
			if got := violated(checks); !slices.Equal(got, c.want) {
				for _, ch := range checks {
					t.Logf("%s: %v", ch.invariant, ch.err)
				}
				t.Errorf("violated %v, want %v", got, c.want)
			}
			hasSentinel := slices.ContainsFunc(checks, func(ch check) bool { return ch.invariant == invSentinelAgreement })
			if hasSentinel != c.sentinel {
				t.Errorf("sentinel_agreement checked: %v", hasSentinel)
			}
		})
	}
}

// A real Redis 7.2 and Valkey 8 master/replica pair, as INFO reports it.
func TestEvaluateRealInfo(t *testing.T) {
	for _, c := range []struct{ prefix, masterIP string }{
		{"redis-7.2.12", "172.19.0.2"},
		{"valkey-8.1.10", "172.19.0.4"},
	} {
		t.Run(c.prefix, func(t *testing.T) {
			s := snapshot{redisReplicas: 2, port: 6379, state: "Healthy", endpoints: []string{c.masterIP}}
			s.redis = []redisPod{
				{pod: pod{Name: "rfr-x-0", IP: c.masterIP, Ready: true}, info: testInfo(t, c.prefix+"-master-replication")},
				{pod: pod{Name: "rfr-x-1", IP: "172.19.0.99", Ready: true}, info: testInfo(t, c.prefix+"-replica-replication")},
			}
			if v := violated(evaluate(s)); v != nil {
				t.Errorf("violated: %v", v)
			}
		})
	}
}

func TestLags(t *testing.T) {
	s := healthy(false)
	s.redis[2].info["slave_repl_offset"] = "1010"
	s.redis = append(s.redis, redisPod{
		pod:  pod{Name: "rfr-x-3", IP: "10.0.0.13"},
		info: replicationInfo("slave", "10.0.0.9", "down", "0"),
	})
	master, err := s.master()
	if err != nil {
		t.Fatal(err)
	}
	got := s.lags(master)
	want := map[string]int64{"rfr-x-1": 10, "rfr-x-2": 0}
	if !maps.Equal(got, want) {
		t.Errorf("lags = %v, want %v", got, want)
	}
}

// A Valkey 9 replica that can't load a Redis 8 RDB retries its full sync
// forever; between attempts INFO reports its link down since it started.
func TestReplicaReadyWithoutDataRealInfo(t *testing.T) {
	s := snapshot{redisReplicas: 2, port: 6379, state: "Healthy", endpoints: []string{"10.0.0.10"}}
	replica := parseInfo("# Replication\r\nrole:slave\r\nmaster_host:10.0.0.10\r\nmaster_port:6379\r\n" +
		"master_link_status:down\r\nmaster_last_io_seconds_ago:-1\r\nmaster_sync_in_progress:0\r\n" +
		"slave_read_repl_offset:0\r\nslave_repl_offset:0\r\nmaster_link_down_since_seconds:-1\r\n")
	s.redis = []redisPod{
		{pod: pod{Name: "rfr-x-0", IP: "10.0.0.10", Ready: true}, info: replicationInfo("master", "", "", "1000")},
		{pod: pod{Name: "rfr-x-1", IP: "10.0.0.11", Ready: true}, info: replica},
	}
	if got := violated(evaluate(s)); !slices.Equal(got, []string{invReplication, invReplicaReadyWithoutData}) {
		t.Errorf("violated %v", got)
	}
	s.redis[1].Ready = false
	if got := violated(evaluate(s)); !slices.Equal(got, []string{invPods, invReplication}) {
		t.Errorf("not ready: violated %v", got)
	}
}
