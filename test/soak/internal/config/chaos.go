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

// Chaos configures the chaos lane: one action at a time on all instances, one
// for each interval plus up to jitter. It is off without kinds, and starts no
// actions after mutation.stopAfter.
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

// Upgrade is how operator_upgrade runs helm on the release named as the
// operator Deployment.
type Upgrade struct {
	// Set are --set values added to every upgrade, after the release's
	// own, which it reuses.
	Set      []string          `json:"set"`
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

// Repository and Tag split the image for the chart, which joins them with a
// ':'. A digest stays with the tag.
func (v OperatorVersion) Repository() string {
	repo, _ := splitImage(v.Image)
	return repo
}

func (v OperatorVersion) Tag() string {
	_, tag := splitImage(v.Image)
	if _, digest, ok := strings.Cut(v.Image, "@"); ok {
		tag += "@" + digest
	}
	return tag
}

// Drain is how node_drain drains a node.
type Drain struct {
	// Hold is how long the drained node stays cordoned.
	Hold metav1.Duration `json:"hold"`
	// Timeout bounds the evictions, which PodDisruptionBudgets may block.
	Timeout metav1.Duration `json:"timeout"`
}

func hasTag(image string) bool {
	_, tag := splitImage(image)
	return tag != ""
}

// ChaosOn reports whether the chaos lane runs.
func (c *Config) ChaosOn() bool {
	return len(c.Chaos.Kinds) > 0
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
	d := &ch.Drain
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
		ch.Drain.Hold.Duration < 0 || ch.Drain.Timeout.Duration < 0 {
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
		case !hasTag(v.Image):
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
