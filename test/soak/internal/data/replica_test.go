package data

import "testing"

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
