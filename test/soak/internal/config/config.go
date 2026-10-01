// Package config loads the soak tester's YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

type Mode string

const (
	ModeOperator Mode = "operator"
	ModeSentinel Mode = "sentinel"
)

// maxNameLength is the operator's limit on RedisFailover names.
const maxNameLength = 48

type Config struct {
	Operator  Operator   `json:"operator"`
	Probe     Probe      `json:"probe"`
	Observer  Observer   `json:"observer"`
	Instances []Instance `json:"instances"`
}

// Operator locates the operator Deployment whose image tag is reported as
// the operator version.
type Operator struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
}

type Probe struct {
	Interval metav1.Duration `json:"interval"`
	Timeout  metav1.Duration `json:"timeout"`
}

// Observer sets how often the invariants are checked, and how long an
// instance may take to converge after a change before its violations count
// as findings.
type Observer struct {
	Interval           metav1.Duration `json:"interval"`
	ConvergenceTimeout metav1.Duration `json:"convergenceTimeout"`
}

type Instance struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Mode      Mode   `json:"mode"`
	Port      int    `json:"port"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, err
	}
	c.setDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.Operator.Namespace == "" {
		c.Operator.Namespace = "redis-operator"
	}
	if c.Operator.Deployment == "" {
		c.Operator.Deployment = "redis-operator"
	}
	if c.Probe.Interval.Duration == 0 {
		c.Probe.Interval.Duration = time.Second
	}
	if c.Probe.Timeout.Duration == 0 {
		c.Probe.Timeout.Duration = time.Second
	}
	if c.Observer.Interval.Duration == 0 {
		c.Observer.Interval.Duration = 5 * time.Second
	}
	if c.Observer.ConvergenceTimeout.Duration == 0 {
		c.Observer.ConvergenceTimeout.Duration = 10 * time.Minute
	}
	for i := range c.Instances {
		if c.Instances[i].Mode == "" {
			c.Instances[i].Mode = ModeOperator
		}
		if c.Instances[i].Port == 0 {
			c.Instances[i].Port = 6379
		}
	}
}

func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("no instances configured")
	}
	seen := map[string]bool{}
	for _, in := range c.Instances {
		if in.Name == "" || in.Namespace == "" {
			return fmt.Errorf("instance %q: name and namespace are required", in.Name)
		}
		if len(in.Name) > maxNameLength {
			return fmt.Errorf("instance %q: name is longer than %d characters", in.Name, maxNameLength)
		}
		if in.Mode != ModeOperator && in.Mode != ModeSentinel {
			return fmt.Errorf("instance %q: mode must be %q or %q", in.Name, ModeOperator, ModeSentinel)
		}
		if in.Port < 1 || in.Port > 65535 {
			return fmt.Errorf("instance %q: invalid port %d", in.Name, in.Port)
		}
		key := in.Namespace + "/" + in.Name
		if seen[key] {
			return fmt.Errorf("instance %s is configured twice", key)
		}
		seen[key] = true
	}
	return nil
}
