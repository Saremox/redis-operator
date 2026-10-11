package observer

import (
	"os"
	"testing"
)

// testInfo parses INFO replies captured from real servers. The replica
// replicates from the master of the same file set.
func testInfo(t *testing.T, files ...string) info {
	t.Helper()
	s := ""
	for _, f := range files {
		b, err := os.ReadFile("testdata/" + f + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		s += string(b)
	}
	return info(ParseInfo(s))
}

func TestParseInfo(t *testing.T) {
	cases := []struct {
		server, version, masterIP string
		offset                    int64
	}{
		{"redis", "7.2.12", "172.19.0.2", 128},
		{"valkey", "9.1.2", "172.19.0.2", 78},
	}
	for _, c := range cases {
		t.Run(c.server+"-"+c.version, func(t *testing.T) {
			prefix := c.server + "-" + c.version
			master := testInfo(t, prefix+"-master-replication", prefix+"-master-server")
			if r := master.role(); r != roleMaster {
				t.Errorf("master role = %q", r)
			}
			if name, version := master.server(); name != c.server || version != c.version {
				t.Errorf("server = %s %s, want %s %s", name, version, c.server, c.version)
			}
			if o := master.int("master_repl_offset"); o != c.offset {
				t.Errorf("master_repl_offset = %d", o)
			}
			if _, ok := master["slave0"]; !ok {
				t.Error("the line with a ':' inside its value is missing")
			}

			replica := testInfo(t, prefix+"-replica-replication")
			if r := replica.role(); r != roleReplica {
				t.Errorf("replica role = %q", r)
			}
			if replica["master_host"] != c.masterIP || replica["master_port"] != "6379" ||
				replica["master_link_status"] != "up" || replica.int("slave_repl_offset") != c.offset {
				t.Errorf("replica fields: %v", replica)
			}
		})
	}
}

func TestRole(t *testing.T) {
	cases := map[string]string{
		"master":   roleMaster,
		"primary":  roleMaster,
		"slave":    roleReplica,
		"replica":  roleReplica,
		"":         "",
		"sentinel": "",
	}
	for role, want := range cases {
		if got := info(ParseInfo("# Replication\r\nrole:" + role + "\r\n")).role(); got != want {
			t.Errorf("role %q = %q, want %q", role, got, want)
		}
	}
	var unreachable info
	if unreachable.role() != "" {
		t.Error("nil info has a role")
	}
}

func TestServer(t *testing.T) {
	cases := []struct {
		info          string
		name, version string
	}{
		{"redis_version:8.2.1", "redis", "8.2.1"},
		{"redis_version:7.2.4\nserver_name:valkey\nvalkey_version:9.0.6", "valkey", "9.0.6"},
		{"redis_version:7.2.4\nserver_name:valkey", "valkey", "7.2.4"},
	}
	for _, c := range cases {
		if name, version := info(ParseInfo(c.info)).server(); name != c.name || version != c.version {
			t.Errorf("%q: got %s %s, want %s %s", c.info, name, version, c.name, c.version)
		}
	}
}
