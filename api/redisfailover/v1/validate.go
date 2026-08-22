package v1

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
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

// Validate set the values by default if not defined and checks if the values given are valid
func (r *RedisFailover) Validate() error {
	if len(r.Name) > maxNameLength {
		return fmt.Errorf("name length can't be higher than %d", maxNameLength)
	}

	for _, rename := range r.Spec.Redis.CustomCommandRenames {
		if !validCommandRenamePattern.MatchString(rename.From) {
			return fmt.Errorf("customCommandRenames: invalid \"from\" command name %q, must match %s", rename.From, validCommandRenamePattern.String())
		}
		// "to" may be empty to disable the command entirely.
		if rename.To != "" && !validCommandRenamePattern.MatchString(rename.To) {
			return fmt.Errorf("customCommandRenames: invalid \"to\" command name %q, must match %s", rename.To, validCommandRenamePattern.String())
		}
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

	if len(r.Spec.Sentinel.CustomConfig) == 0 {
		r.Spec.Sentinel.CustomConfig = defaultSentinelCustomConfig
	}

	r.Status = RedisFailoverStatus{
		State: HealthyState,
	}

	return nil
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
