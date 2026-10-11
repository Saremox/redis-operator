package mutator

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	corev1 "k8s.io/api/core/v1"
)

// Redis 6.2 accepts one section in INFO, and answers a list of sections with a
// syntax error. The mutator reads the version and the replication fields of a
// redis pod with one INFO.
func TestServersReadsOneInfo(t *testing.T) {
	srv := miniredis.RunT(t)
	srv.Server().SetPreHook(func(c *miniserver.Peer, cmd string, args ...string) bool {
		if !strings.EqualFold(cmd, "INFO") {
			return false
		}
		if len(args) > 1 {
			c.WriteError("ERR syntax error")
			return true
		}
		c.WriteBulk("# Server\r\nredis_version:6.2.24\r\n# Replication\r\nrole:master\r\nmaster_replid:abc\r\n")
		return true
	})
	port, err := strconv.Atoi(srv.Port())
	if err != nil {
		t.Fatal(err)
	}
	pod := corev1.Pod{}
	pod.Name = "rfr-x-0"
	pod.Status.PodIP = "127.0.0.1"
	m := &Mutator{timeout: time.Second}

	got := m.servers(context.Background(), []corev1.Pod{pod}, nil, port)["rfr-x-0"]
	if got.err != nil {
		t.Fatalf("INFO: %v", got.err)
	}
	if got.name != "redis" || got.version != "6.2.24" || got.fields["role"] != "master" || got.fields["master_replid"] != "abc" {
		t.Errorf("server %+v", got)
	}
}
