// Package customconfig tells whether the redis pods and the Sentinels run a
// RedisFailover's customConfig, which the operator applies with CONFIG SET
// and SENTINEL SET mymaster.
package customconfig

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
)

// unmanaged are the redis keys that the operator sets itself, whatever
// customConfig says: the passwords, and aclfile, which it cannot set and loads
// with ACL LOAD.
var unmanaged = []string{"requirepass", "masterauth", "aclfile"}

// maxMemoryKeys are left to the maxmem check with spec.redis.maxMemory: it
// gives customConfig's maxmemory and maxmemory-policy precedence itself,
// and the operator sets replica-ignore-maxmemory to yes, rejecting no.
var maxMemoryKeys = []string{"maxmemory", "maxmemory-policy", "replica-ignore-maxmemory", "slave-ignore-maxmemory"}

type entry struct{ key, value string }

// parse splits entries like the operator does: the key, and the rest as the
// value, where `""` is the empty value. A later entry for a key wins.
func parse(configs []string) []entry {
	var out []entry
	for _, c := range configs {
		f := strings.Split(c, " ")
		if len(f) < 2 || strings.TrimSpace(f[0]) == "" {
			continue
		}
		e := entry{key: strings.ToLower(f[0]), value: strings.Join(f[1:], " ")}
		if len(f) == 2 && f[1] == `""` {
			e.value = ""
		}
		out = slices.DeleteFunc(out, func(o entry) bool { return o.key == e.key })
		out = append(out, e)
	}
	return out
}

func redisEntries(rf *redisfailoverv1.RedisFailover) []entry {
	return slices.DeleteFunc(parse(rf.Spec.Redis.CustomConfig), func(e entry) bool {
		return slices.Contains(unmanaged, e.key) || rf.Spec.Redis.MaxMemory != nil && slices.Contains(maxMemoryKeys, e.key)
	})
}

// RedisKeys returns the keys to CONFIG GET from every redis pod.
func RedisKeys(rf *redisfailoverv1.RedisFailover) []string {
	var keys []string
	for _, e := range redisEntries(rf) {
		keys = append(keys, e.key)
	}
	return keys
}

// Redis compares rf, which has its defaults applied, with a pod's CONFIG
// GET reply for RedisKeys.
func Redis(rf *redisfailoverv1.RedisFailover, got map[string]string) error {
	var errs []error
	for _, e := range redisEntries(rf) {
		v, ok := got[e.key]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s not reported", e.key))
		case !same(v, e.value):
			errs = append(errs, fmt.Errorf("%s is %q, customConfig sets %q", e.key, v, e.value))
		}
	}
	return errors.Join(errs...)
}

// Sentinel compares the Sentinel customConfig of rf with the SENTINEL MASTER
// mymaster fields of a Sentinel, which use the names of SENTINEL SET. It
// cannot check keys that SENTINEL MASTER does not report, for example
// auth-pass.
func Sentinel(rf *redisfailoverv1.RedisFailover, fields map[string]string) error {
	var errs []error
	for _, e := range parse(rf.Spec.Sentinel.CustomConfig) {
		if v, ok := fields[e.key]; ok && !same(v, e.value) {
			errs = append(errs, fmt.Errorf("%s is %q, customConfig sets %q", e.key, v, e.value))
		}
	}
	return errors.Join(errs...)
}

// same compares values as redis normalises them: case-insensitively, and
// memory sizes by their bytes.
func same(got, want string) bool {
	got, want = strings.TrimSpace(got), strings.TrimSpace(want)
	if strings.EqualFold(got, want) {
		return true
	}
	g, err1 := maxmem.ParseMemory(got)
	w, err2 := maxmem.ParseMemory(want)
	return err1 == nil && err2 == nil && g == w
}
