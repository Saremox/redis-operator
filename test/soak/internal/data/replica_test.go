package data

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

func TestCaughtUp(t *testing.T) {
	at := position{replID: "new", offset: 1000}
	cases := []struct {
		name string
		info map[string]string
		want bool
	}{
		{"caught up", map[string]string{"master_replid": "new", "slave_repl_offset": "1000", "master_link_status": "up"}, true},
		{"ahead", map[string]string{"master_replid": "new", "slave_repl_offset": "1200", "master_link_status": "up"}, true},
		{"behind", map[string]string{"master_replid": "new", "slave_repl_offset": "999", "master_link_status": "up"}, false},
		// The source's master was replaced and the pod hasn't synced with it
		// yet: far ahead, but in the old stream.
		{"old stream", map[string]string{"master_replid": "old", "slave_repl_offset": "900000", "master_link_status": "up"}, false},
		{"link down", map[string]string{"master_replid": "new", "slave_repl_offset": "1000", "master_link_status": "down"}, false},
		{"no offset", map[string]string{"master_replid": "new", "master_link_status": "up"}, false},
	}
	for _, c := range cases {
		if got := caughtUp(c.info, at); got != c.want {
			t.Errorf("%s: caughtUp = %v", c.name, got)
		}
	}
}

type fakePods []observer.PodAddr

func (f fakePods) RedisAddrs() []observer.PodAddr { return f }

// The source deletes the keys of a sample at its second verification after
// the sample. If it does so while the bootstrap check waits for a pod, the
// pod receives the deletes. They are no lost writes.
func TestReplicaSourceAgedOut(t *testing.T) {
	src, m, fi, mt := newTestData(t, true)
	ctx := context.Background()
	const replication = "# Replication\r\nmaster_replid:r1\r\nmaster_repl_offset:100\r\nslave_repl_offset:100\r\nmaster_link_status:up\r\n"
	m.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if strings.EqualFold(cmd, "INFO") && len(args) == 1 && args[0] == "replication" {
			c.WriteBulk(replication)
			return true
		}
		return fi.hook(c, cmd, args...)
	})
	for range 10 {
		seq := src.ledger.begin()
		key := LedgerKey("x", seq)
		src.ledger.end(seq, src.client.Set(ctx, key, Value(key, 64), 0).Err() == nil)
	}
	pod := miniredis.RunT(t)
	for _, k := range m.Keys() {
		v, _ := m.Get(k)
		_ = pod.Set(k, v)
	}
	var once sync.Once
	pod.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if !strings.EqualFold(cmd, "INFO") {
			return false
		}
		// The source verifies twice while the check waits, and the pod
		// replicates its deletes.
		once.Do(func() {
			for range 2 {
				if _, err := src.verify(ctx, config.EventPeriodic, 0); err != nil {
					t.Error(err)
				}
			}
			for _, k := range pod.Keys() {
				if !m.Exists(k) {
					pod.Del(k)
				}
			}
		})
		c.WriteBulk(replication)
		return true
	})
	r := &Replica{
		in:       config.Instance{Name: "b", Namespace: "ns", Mode: config.ModeOperator},
		cfg:      config.Bootstrap{SampleKeys: 100},
		source:   src,
		pods:     fakePods{{Name: "rfr-b-0", Addr: pod.Addr()}},
		rnd:      rand.New(rand.NewPCG(1, 2)),
		log:      slog.New(slog.DiscardHandler),
		lost:     mt.LostWrites.MustCurryWith(prometheus.Labels{"rf": "b", "namespace": "ns", "mode": "operator"}),
		verified: mt.LedgerVerified.MustCurryWith(prometheus.Labels{"rf": "b", "namespace": "ns", "mode": "operator"}),
	}
	lost, err := r.verify(ctx, config.EventPeriodic, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lost != 0 {
		t.Errorf("lost %d writes that the source deleted", lost)
	}
}
