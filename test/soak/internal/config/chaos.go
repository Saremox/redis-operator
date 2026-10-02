package config

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ChaosKind is a kind of cluster-level action, the value of the chaos
// metrics' kind label and of the event label of what it costs the
// instances.
type ChaosKind string

const (
	// OperatorRestart deletes the operator pod.
	OperatorRestart ChaosKind = "operator_restart"
	// OperatorUpgrade upgrades the operator release with helm to every
	// other configured version in turn, and back.
	OperatorUpgrade ChaosKind = "operator_upgrade"
	// NodeDrain cordons and drains a node, holds it, and uncordons it.
	NodeDrain ChaosKind = "node_drain"
)

var chaosKinds = []ChaosKind{OperatorRestart, OperatorUpgrade, NodeDrain}

// Chaos is the global chaos lane: at most one action at a time, for every
// instance at once, one per interval plus up to jitter. Every mutator
// pauses while an action runs, and every instance is in a convergence
// window. It is off without kinds, or with mutation.enabled false, and
// stops starting actions after mutation.stopAfter.
type Chaos struct {
	// Kinds are the enabled kinds and their weights.
	Kinds    map[ChaosKind]int `json:"kinds"`
	Interval metav1.Duration   `json:"interval"`
	Jitter   metav1.Duration   `json:"jitter"`
	// Timeout bounds each wait of an action: the instances to be quiet
	// before it, the operator to lead again, and the instances to converge
	// after it. By default observer.convergenceTimeout.
	Timeout metav1.Duration `json:"timeout"`
	Upgrade Upgrade         `json:"upgrade"`
	Drain   Drain           `json:"drain"`
}

// Upgrade is how operator_upgrade runs helm.
type Upgrade struct {
	// Release is the operator's helm release, in operator.namespace.
	Release string `json:"release"`
	// Helm is the helm binary.
	Helm string `json:"helm"`
	// Set are --set values added to every upgrade, after the release's
	// own, which it reuses.
	Set []string `json:"set"`
	// HookJob is the chart's CRD upgrade hook Job, by default
	// <release>-crds-upgrade.
	HookJob string `json:"hookJob"`
	// CRD is the CustomResourceDefinition the hook applies.
	CRD string `json:"crd"`
	// Timeout is helm's --timeout.
	Timeout  metav1.Duration   `json:"timeout"`
	Versions []OperatorVersion `json:"versions"`
}

// OperatorVersion is an operator release to upgrade to.
type OperatorVersion struct {
	Name string `json:"name"`
	// Chart is an OCI reference, or a chart archive or directory relative
	// to the config file.
	Chart string `json:"chart"`
	// Version is the chart version, for an OCI chart.
	Version string `json:"version"`
	// Image is the operator image, repository:tag.
	Image string `json:"image"`
}

// Repository and Tag split the image.
func (v OperatorVersion) Repository() string {
	i := strings.LastIndex(v.Image, ":")
	return v.Image[:i]
}

func (v OperatorVersion) Tag() string {
	return v.Image[strings.LastIndex(v.Image, ":")+1:]
}

// Drain is how node_drain picks and drains a node.
type Drain struct {
	// NodeSelector is a label selector of the nodes that may be drained.
	// The tester's own node never is.
	NodeSelector string `json:"nodeSelector"`
	// Hold is how long the drained node stays cordoned.
	Hold metav1.Duration `json:"hold"`
	// Timeout bounds the evictions, which PodDisruptionBudgets may block.
	Timeout metav1.Duration `json:"timeout"`
}

// ChaosOn reports whether the chaos lane runs.
func (c *Config) ChaosOn() bool {
	return c.Mutation.On() && len(c.Chaos.Kinds) > 0
}

// Sorted returns the enabled chaos kinds in a fixed order.
func (ch Chaos) Sorted() []ChaosKind {
	return slices.Sorted(maps.Keys(ch.Kinds))
}

func (ch *Chaos) setDefaults(observer Observer) {
	if ch.Interval.Duration == 0 {
		ch.Interval.Duration = 10 * time.Minute
	}
	if ch.Timeout.Duration == 0 {
		ch.Timeout = observer.ConvergenceTimeout
	}
	u := &ch.Upgrade
	if u.Release == "" {
		u.Release = "redis-operator"
	}
	if u.Helm == "" {
		u.Helm = "helm"
	}
	if u.HookJob == "" {
		u.HookJob = u.Release + "-crds-upgrade"
	}
	if u.CRD == "" {
		u.CRD = "redisfailovers.databases.spotahome.com"
	}
	if u.Timeout.Duration == 0 {
		u.Timeout.Duration = 10 * time.Minute
	}
	d := &ch.Drain
	if d.NodeSelector == "" {
		d.NodeSelector = "!node-role.kubernetes.io/control-plane"
	}
	if d.Hold.Duration == 0 {
		d.Hold.Duration = time.Minute
	}
	if d.Timeout.Duration == 0 {
		d.Timeout.Duration = 5 * time.Minute
	}
}

func (ch Chaos) validate() error {
	for k, w := range ch.Kinds {
		if !slices.Contains(chaosKinds, k) {
			return fmt.Errorf("unknown kind %q", k)
		}
		if w < 1 {
			return fmt.Errorf("%s: weight must be at least 1", k)
		}
	}
	if ch.Interval.Duration < 0 || ch.Jitter.Duration < 0 || ch.Timeout.Duration < 0 ||
		ch.Drain.Hold.Duration < 0 || ch.Drain.Timeout.Duration < 0 || ch.Upgrade.Timeout.Duration < 0 {
		return errors.New("negative duration")
	}
	if _, ok := ch.Kinds[OperatorUpgrade]; !ok {
		return nil
	}
	if len(ch.Upgrade.Versions) < 2 {
		return errors.New("upgrade: at least two versions")
	}
	names, images := map[string]bool{}, map[string]bool{}
	for _, v := range ch.Upgrade.Versions {
		switch {
		case v.Name == "" || v.Chart == "" || v.Image == "":
			return errors.New("upgrade: every version needs a name, chart and image")
		case !strings.Contains(v.Image[strings.LastIndex(v.Image, "/")+1:], ":"):
			return fmt.Errorf("upgrade: %s: image %s has no tag", v.Name, v.Image)
		case names[v.Name] || images[v.Image]:
			return fmt.Errorf("upgrade: %s or its image is configured twice", v.Name)
		}
		names[v.Name], images[v.Image] = true, true
	}
	return nil
}

// resolveCharts makes local charts relative to the config file's
// directory.
func (ch *Chaos) resolveCharts(dir string) {
	for i, v := range ch.Upgrade.Versions {
		if !strings.Contains(v.Chart, "://") && !filepath.IsAbs(v.Chart) {
			ch.Upgrade.Versions[i].Chart = filepath.Join(dir, v.Chart)
		}
	}
}
