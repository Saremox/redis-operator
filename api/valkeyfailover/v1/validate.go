package v1

import (
	"errors"
	"fmt"
	"strconv"
)

const (
	maxNameLength = 48
)

// Validate set the values by default if not defined and checks if the values given are valid
func (r *ValkeyFailover) Validate() error {
	if len(r.Name) > maxNameLength {
		return fmt.Errorf("name length can't be higher than %d", maxNameLength)
	}

	if r.Bootstrapping() {
		if r.Spec.BootstrapNode.Host == "" {
			return errors.New("BootstrapNode must include a host when provided")
		}

		if r.Spec.BootstrapNode.Port == "" {
			r.Spec.BootstrapNode.Port = strconv.Itoa(defaultValkeyPort)
		}
		r.Spec.Valkey.CustomConfig = deduplicateStr(append(bootstrappingValkeyCustomConfig, r.Spec.Valkey.CustomConfig...))
	} else {
		r.Spec.Valkey.CustomConfig = deduplicateStr(append(defaultValkeyCustomConfig, r.Spec.Valkey.CustomConfig...))
	}

	if r.Spec.Valkey.Image == "" {
		r.Spec.Valkey.Image = defaultImage
	}

	if r.Spec.Sentinel.Image == "" {
		r.Spec.Sentinel.Image = defaultImage
	}

	if r.Spec.Valkey.Replicas <= 0 {
		r.Spec.Valkey.Replicas = defaultValkeyNumber
	}

	if r.Spec.Valkey.Port <= 0 {
		r.Spec.Valkey.Port = defaultValkeyPort
	}

	if r.Spec.Sentinel.Replicas <= 0 {
		r.Spec.Sentinel.Replicas = defaultSentinelNumber
	}

	if r.Spec.Valkey.Exporter.Image == "" {
		r.Spec.Valkey.Exporter.Image = defaultExporterImage
	}

	if r.Spec.Sentinel.Exporter.Image == "" {
		r.Spec.Sentinel.Exporter.Image = defaultSentinelExporterImage
	}

	if len(r.Spec.Sentinel.CustomConfig) == 0 {
		r.Spec.Sentinel.CustomConfig = defaultSentinelCustomConfig
	}

	r.Status = ValkeyFailoverStatus{
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
