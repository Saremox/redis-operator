package v1

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	maxNameLength = 48
)

// validCommandRenamePattern restricts rename-command "from"/"to" values to
// safe, non-empty Redis command name characters. This prevents the quotes,
// spaces and newlines used in redisConfigTemplate's
// `rename-command "{{.From}}" "{{.To}}"` line from being broken out of,
// which would otherwise let a crafted CustomCommandRenames value inject
// arbitrary directives into redis.conf.
var validCommandRenamePattern = regexp.MustCompile(`^[A-Za-z_]+$`)

// operatorRedisCommands are the Redis commands that the operator, the pod
// scripts and the replicas send. A rename of one of them makes each reconcile,
// a probe or the replication fail.
var operatorRedisCommands = []string{"AUTH", "CLIENT", "CONFIG", "INFO", "PING", "PSYNC", "REPLCONF", "REPLICAOF", "SLAVEOF"}

// sentinelRedisCommands are the other Redis commands that Sentinel sends. A
// rename of one of them stops the Sentinel discovery or the failover.
var sentinelRedisCommands = []string{"EXEC", "MULTI", "PUBLISH", "SUBSCRIBE"}

// Validate set the values by default if not defined and checks if the values given are valid
func (r *RedisFailover) Validate() error {
	if len(r.Name) > maxNameLength {
		return fmt.Errorf("name length can't be higher than %d", maxNameLength)
	}

	for _, rename := range r.Spec.Redis.CustomCommandRenames {
		if !validCommandRenamePattern.MatchString(rename.From) {
			return fmt.Errorf("customCommandRenames: invalid \"from\" command name %q, must match %s", rename.From, validCommandRenamePattern.String())
		}
		if err := r.validateCommandRename(rename.From); err != nil {
			return err
		}
		// "to" may be empty to disable the command entirely.
		if rename.To != "" && !validCommandRenamePattern.MatchString(rename.To) {
			return fmt.Errorf("customCommandRenames: invalid \"to\" command name %q, must match %s", rename.To, validCommandRenamePattern.String())
		}
	}

	for name, port := range map[string]int32{"redis": r.Spec.Redis.Exporter.Port, "sentinel": r.Spec.Sentinel.Exporter.Port} {
		if port < 0 || port > 65535 {
			return fmt.Errorf("%s.exporter.port %d must be between 1 and 65535, or 0 for the default", name, port)
		}
	}

	if err := r.validateMaxMemory(); err != nil {
		return err
	}

	if r.Bootstrapping() {
		if r.Spec.BootstrapNode.Host == "" {
			return errors.New("BootstrapNode must include a host when provided")
		}

		if r.Spec.BootstrapNode.Port == "" {
			r.Spec.BootstrapNode.Port = strconv.Itoa(defaultRedisPort)
		}
		r.Spec.Redis.CustomConfig = deduplicateStr(append(bootstrappingRedisCustomConfig, r.Spec.Redis.CustomConfig...))
	} else {
		r.Spec.Redis.CustomConfig = deduplicateStr(append(defaultRedisCustomConfig, r.Spec.Redis.CustomConfig...))
	}

	if r.Spec.Redis.Image == "" {
		r.Spec.Redis.Image = defaultImage
	}

	if r.Spec.Sentinel.Image == "" {
		r.Spec.Sentinel.Image = defaultImage
	}

	if r.Spec.Redis.Replicas <= 0 {
		r.Spec.Redis.Replicas = defaultRedisNumber
	}

	if r.Spec.Redis.Port <= 0 {
		r.Spec.Redis.Port = defaultRedisPort
	}

	if r.Spec.Sentinel.Replicas <= 0 {
		r.Spec.Sentinel.Replicas = defaultSentinelNumber
	}

	if r.Spec.Redis.Exporter.Image == "" {
		r.Spec.Redis.Exporter.Image = defaultExporterImage
	}

	if r.Spec.Sentinel.Exporter.Image == "" {
		r.Spec.Sentinel.Exporter.Image = defaultSentinelExporterImage
	}

	r.Spec.Sentinel.CustomConfig = addSentinelDefaults(r.Spec.Sentinel.CustomConfig)

	return nil
}

// validateCommandRename rejects a rename of a command that the RedisFailover
// needs. ACL is necessary only to load an aclfile, and the Sentinel commands
// only when Sentinels run.
func (r *RedisFailover) validateCommandRename(from string) error {
	command := strings.ToUpper(from)
	switch {
	case slices.Contains(operatorRedisCommands, command):
		return fmt.Errorf("customCommandRenames: %q cannot be renamed, because the operator, the pod scripts or the replicas send it (%s)", from, strings.Join(operatorRedisCommands, ", "))
	case command == "ACL" && r.CustomConfigSets("aclfile"):
		return fmt.Errorf("customCommandRenames: %q cannot be renamed, because the operator sends ACL LOAD to apply the aclfile of customConfig", from)
	case slices.Contains(sentinelRedisCommands, command) && r.SentinelsAllowed():
		return fmt.Errorf("customCommandRenames: %q cannot be renamed, because Sentinel sends it (%s)", from, strings.Join(sentinelRedisCommands, ", "))
	}
	return nil
}

// addSentinelDefaults puts each default in front of configs, unless configs
// sets that parameter. Otherwise a Sentinel uses a different value after a
// restart or a new monitor, and the Sentinels then disagree on the timeouts.
// The parameter names are case-insensitive, as in SENTINEL SET.
func addSentinelDefaults(configs []string) []string {
	set := map[string]bool{}
	for _, c := range configs {
		set[strings.ToLower(strings.SplitN(c, " ", 2)[0])] = true
	}
	merged := []string{}
	for _, d := range defaultSentinelCustomConfig {
		if !set[strings.SplitN(d, " ", 2)[0]] {
			merged = append(merged, d)
		}
	}
	return append(merged, configs...)
}

func deduplicateStr(strSlice []string) []string {
	allKeys := make(map[string]bool)
	list := []string{}
	for _, item := range strSlice {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}
