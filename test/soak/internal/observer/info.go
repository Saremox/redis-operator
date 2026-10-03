package observer

import (
	"strconv"
	"strings"
)

const (
	roleMaster  = "master"
	roleReplica = "replica"

	serverRedis  = "redis"
	serverValkey = "valkey"
)

// info is the key:value body of an INFO reply.
type info map[string]string

// ParseInfo parses the key:value lines of an INFO reply.
func ParseInfo(s string) map[string]string {
	i := map[string]string{}
	for line := range strings.Lines(s) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			i[k] = v
		}
	}
	return i
}

// role normalises the replication role, which Valkey may report as
// primary or replica.
func (i info) role() string {
	switch i["role"] {
	case "master", "primary":
		return roleMaster
	case "slave", "replica":
		return roleReplica
	}
	return ""
}

func (i info) server() (name, version string) {
	if v := i["valkey_version"]; v != "" {
		return serverValkey, v
	}
	if i["server_name"] == serverValkey {
		return serverValkey, i["redis_version"]
	}
	return serverRedis, i["redis_version"]
}

func (i info) int(key string) int64 {
	n, _ := strconv.ParseInt(i[key], 10, 64)
	return n
}

// ServerOf returns the server and version an INFO server reply reports,
// and its replication fields if it has them.
func ServerOf(reply string) (name, version string, fields map[string]string) {
	i := info(ParseInfo(reply))
	name, version = i.server()
	return name, version, i
}
