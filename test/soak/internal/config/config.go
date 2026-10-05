// Package config loads the soak tester's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	Operator Operator `json:"operator"`
	Probe    Probe    `json:"probe"`
	Observer Observer `json:"observer"`
	Mutation Mutation `json:"mutation"`
	Chaos    Chaos    `json:"chaos"`
	// Versions and Edges are the server versions in the rotation and the
	// transition graph between them.
	Versions  []Version  `json:"versions"`
	Edges     []Edge     `json:"edges"`
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
	// ReplicaReadyWithoutData makes a Ready replica that never completed a
	// sync a finding, as other invariants are; default true. Off, it is
	// only reported, for operators without the readiness fix.
	ReplicaReadyWithoutData *bool `json:"replicaReadyWithoutData"`
}

// ReplicaReadyWithoutDataFinding reports whether replica_ready_without_data
// counts as a finding.
func (o Observer) ReplicaReadyWithoutDataFinding() bool {
	return o.ReplicaReadyWithoutData == nil || *o.ReplicaReadyWithoutData
}

// MinDwell is the minimum time that the convergence window of a mutation
// stays open. Without it, a change that the operator did not see yet can look
// converged.
const MinDwell = 15 * time.Second

// Mutation sets how often instances are mutated. A mutation's
// convergence timeout is observer.convergenceTimeout.
type Mutation struct {
	Interval metav1.Duration `json:"interval"`
	// Jitter is the most that is added at random to every interval.
	Jitter metav1.Duration `json:"jitter"`
	// Seed makes every pick reproducible. 0 picks a seed at startup.
	Seed int64 `json:"seed"`
	// StopAfter stops starting mutations this long after startup, for
	// runs of a fixed length. 0 never stops.
	StopAfter metav1.Duration `json:"stopAfter"`
	// Timeouts are the convergence timeouts of kinds that take longer than
	// observer.convergenceTimeout.
	Timeouts map[Kind]Timeout `json:"timeouts"`
}

// Timeout is a kind's convergence timeout, counted from applying the
// mutation: base plus perPod for every redis pod.
type Timeout struct {
	Base   metav1.Duration `json:"base"`
	PerPod metav1.Duration `json:"perPod"`
}

// DefaultNamespace holds the instances, apart from the tester, so that a run
// can start afresh with a new namespace.
const DefaultNamespace = "redis-soak-instances"

type Instance struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Mode      Mode   `json:"mode"`
	Port      int    `json:"port"`
	// MaxMemoryPolicy is the spec.redis.maxMemory.policy the instance is
	// created with, empty without maxMemory.
	MaxMemoryPolicy string `json:"maxMemoryPolicy"`
	// Template is a RedisFailover manifest, relative to the config file. The
	// tester creates the instance from it if the instance does not exist, and
	// recreates the instance from it on a reset.
	Template string `json:"template"`
	// Version and SentinelVersion set the redis and Sentinel image of an
	// instance made from its template, by version name. A chain starts on
	// its first start version.
	Version         string     `json:"version"`
	SentinelVersion string     `json:"sentinelVersion"`
	Chain           *Chain     `json:"chain"`
	Bootstrap       *Bootstrap `json:"bootstrap"`
	Data            *Data      `json:"data"`
	Mutations       Mutations  `json:"mutations"`
}

// Bootstrap configures an instance whose spec.bootstrapNode is the master of
// another configured instance, the source. It is read-only: its probes read a
// key that the probes of the source write, and its verification reads a sample
// of the source ledger from each pod.
type Bootstrap struct {
	Source string `json:"source"`
}

// AuthSecret is the Secret that auth_add creates, and that the tester names
// in spec.auth.secretPath of an instance made from a template.
func (in Instance) AuthSecret() string {
	return in.Name + "-auth"
}

// Load reads the config file; instance templates are relative to it.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, err
	}
	for i := range c.Instances {
		if t := c.Instances[i].Template; t != "" && !filepath.IsAbs(t) {
			c.Instances[i].Template = filepath.Join(filepath.Dir(path), t)
		}
	}
	c.Chaos.resolveCharts(filepath.Dir(path))
	return c, nil
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
	if c.Mutation.Interval.Duration == 0 {
		c.Mutation.Interval.Duration = 2 * time.Minute
	}
	if c.Mutation.Seed == 0 {
		c.Mutation.Seed = time.Now().UnixNano()
	}
	// A pod's start and sync per redis pod: image changes and resets
	// replace every pod, one at a time.
	for k, perPod := range map[Kind]time.Duration{ImageUpgrade: 2 * time.Minute, SentinelImageUpgrade: time.Minute, Reset: time.Minute} {
		if _, ok := c.Mutation.Timeouts[k]; !ok {
			if c.Mutation.Timeouts == nil {
				c.Mutation.Timeouts = map[Kind]Timeout{}
			}
			c.Mutation.Timeouts[k] = Timeout{PerPod: metav1.Duration{Duration: perPod}}
		}
	}
	if _, ok := c.Mutation.Timeouts[SentinelResetKillMaster]; !ok {
		if c.Mutation.Timeouts == nil {
			c.Mutation.Timeouts = map[Kind]Timeout{}
		}
		c.Mutation.Timeouts[SentinelResetKillMaster] = Timeout{Base: metav1.Duration{Duration: NoMasterTimeout}}
	}
	for k, t := range c.Mutation.Timeouts {
		if t.Base.Duration == 0 {
			t.Base = c.Observer.ConvergenceTimeout
			c.Mutation.Timeouts[k] = t
		}
	}
	c.Chaos.setDefaults(c.Observer)
	for i := range c.Versions {
		c.Versions[i].setDefaults()
	}
	for i := range c.Instances {
		if c.Instances[i].Namespace == "" {
			c.Instances[i].Namespace = DefaultNamespace
		}
		if c.Instances[i].Mode == "" {
			c.Instances[i].Mode = ModeOperator
		}
		if c.Instances[i].Port == 0 {
			c.Instances[i].Port = 6379
		}
		if c.Instances[i].Data != nil {
			c.Instances[i].Data.setDefaults()
		}
		if ch := c.Instances[i].Chain; ch != nil {
			if c.Instances[i].Version == "" && len(ch.Start) > 0 {
				c.Instances[i].Version = ch.Start[0]
			}
			if c.Instances[i].SentinelVersion == "" && ch.Sentinel != "" {
				c.Instances[i].SentinelVersion = c.Instances[i].Version
			}
		}
	}
}

func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("no instances configured")
	}
	if c.Mutation.Interval.Duration < 0 || c.Mutation.Jitter.Duration < 0 || c.Mutation.StopAfter.Duration < 0 {
		return fmt.Errorf("mutation: negative duration")
	}
	if MinDwell >= c.Observer.ConvergenceTimeout.Duration {
		return fmt.Errorf("observer.convergenceTimeout must be longer than %s", MinDwell)
	}
	for k, t := range c.Mutation.Timeouts {
		if !slices.Contains(kinds, k) || t.Base.Duration <= MinDwell || t.PerPod.Duration < 0 {
			return fmt.Errorf("mutation.timeouts: %s: a known kind, base longer than %s, perPod not negative", k, MinDwell)
		}
	}
	if err := c.validateVersions(); err != nil {
		return err
	}
	if err := c.Chaos.validate(); err != nil {
		return fmt.Errorf("chaos: %w", err)
	}
	seen := map[string]bool{}
	for _, in := range c.Instances {
		if in.Name == "" {
			return errors.New("instance: name is required")
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
		if err := in.validateData(); err != nil {
			return fmt.Errorf("instance %q: %w", in.Name, err)
		}
		if err := c.validateBootstrap(in); err != nil {
			return fmt.Errorf("instance %q: bootstrap: %w", in.Name, err)
		}
		if err := c.validateTemplate(in); err != nil {
			return fmt.Errorf("instance %q: %w", in.Name, err)
		}
		if err := in.Mutations.validate(in); err != nil {
			return fmt.Errorf("instance %q: mutations: %w", in.Name, err)
		}
		if err := c.validateFlip(in); err != nil {
			return fmt.Errorf("instance %q: mutations: %w", in.Name, err)
		}
		// Other config refers to an instance by its name alone, for example as
		// a source.
		if seen[in.Name] {
			return fmt.Errorf("instance %s is configured twice: names must be unique across namespaces", in.Name)
		}
		seen[in.Name] = true
	}
	return nil
}

// validateTemplate checks what needs a template: its versions, its chain,
// and a reset.
func (c *Config) validateTemplate(in Instance) error {
	_, reset := in.Mutations.Kinds[Reset]
	if in.Template == "" {
		if in.Chain != nil || in.Version != "" || in.SentinelVersion != "" || reset {
			return errors.New("chain, version, sentinelVersion and reset need a template")
		}
		return nil
	}
	for _, v := range []string{in.Version, in.SentinelVersion} {
		if _, ok := c.VersionNamed(v); v != "" && !ok {
			return fmt.Errorf("unknown version %s", v)
		}
	}
	if in.SentinelVersion != "" && in.Mode != ModeSentinel {
		return errors.New("sentinelVersion needs a sentinel instance")
	}
	if in.Chain == nil {
		return nil
	}
	if err := c.validateChain(in); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	if in.Version != in.Chain.Start[0] {
		return errors.New("the version of a chain instance is its first start")
	}
	return nil
}

// validateFlip checks that sentinel_image_flip has at least two known
// versions to flip between.
func (c *Config) validateFlip(in Instance) error {
	if _, ok := in.Mutations.Kinds[SentinelImageFlip]; !ok {
		return nil
	}
	seen := map[string]bool{}
	for _, v := range in.Mutations.SentinelImages {
		if _, ok := c.VersionNamed(v); !ok {
			return fmt.Errorf("sentinelImages: unknown version %s", v)
		}
		seen[v] = true
	}
	if len(seen) < 2 {
		return fmt.Errorf("%s needs at least two sentinelImages", SentinelImageFlip)
	}
	return nil
}

// Timeout returns a kind's convergence timeout for an instance with that
// many redis pods.
func (m Mutation) Timeout(kind Kind, observer Observer, redisPods int32) time.Duration {
	t, ok := m.Timeouts[kind]
	if !ok {
		return observer.ConvergenceTimeout.Duration
	}
	return t.Base.Duration + time.Duration(redisPods)*t.PerPod.Duration
}

// LongestTimeout bounds every convergence timeout for up to maxPods redis
// pods.
func (c *Config) LongestTimeout(maxPods int32) time.Duration {
	d := c.Observer.ConvergenceTimeout.Duration
	for k := range c.Mutation.Timeouts {
		d = max(d, c.Mutation.Timeout(k, c.Observer, maxPods))
	}
	return d
}

func (c *Config) validateBootstrap(in Instance) error {
	b := in.Bootstrap
	if b == nil {
		return nil
	}
	switch {
	case in.Mode != ModeOperator:
		return errors.New("a bootstrapping instance runs in operator mode, without Sentinels")
	case in.Data != nil:
		return errors.New("a bootstrapping instance is read-only and has no data")
	}
	i := slices.IndexFunc(c.Instances, func(s Instance) bool { return s.Name == b.Source })
	if i < 0 {
		return fmt.Errorf("source %q is not configured", b.Source)
	}
	if src := c.Instances[i]; src.Bootstrap != nil || src.Data == nil || src.Data.Ledger == nil {
		return fmt.Errorf("source %q must have a ledger and not bootstrap itself", b.Source)
	}
	return nil
}
