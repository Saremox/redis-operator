package config

import (
	"cmp"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Version is a server version in the rotation, by a name that stays the
// same while its image is updated deliberately.
type Version struct {
	Name string `json:"name"`
	// Image is an exact patch tag, never a floating one.
	Image string `json:"image"`
	// Server and Release are what INFO server reports for it, from the
	// image: redis or valkey, and the tag up to its first '-'.
	Server  string `json:"-"`
	Release string `json:"-"`
}

// WaitsForReplicas reports whether the server, on SIGTERM, waits until its
// replicas have the writes, up to shutdown-timeout. Redis 7.0 and later and
// Valkey do. Redis 6.2 only flushes the replica buffers once, and then exits.
func (v Version) WaitsForReplicas() bool {
	return v.Server != "redis" || compareReleases(v.Release, "7.0.0") >= 0
}

// WaitsForReplicas reports whether the redis pods that run image wait for
// their replicas on SIGTERM. An image that is no configured version does.
func (c *Config) WaitsForReplicas(image string) bool {
	v, ok := c.VersionOf(image)
	return !ok || v.WaitsForReplicas()
}

// Expectations of an edge, the values of the expect label.
const (
	ExpectOK      = "ok"
	ExpectFail    = "fail"
	ExpectUnknown = "unknown"
)

// Edge is a version change that the tester can make. A downgrade never
// expects ok, because an older server may not load a newer RDB.
type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Expect string `json:"expect"`
	// Group keeps the edge out of every chain whose group differs. Edges
	// without a group form the main graph.
	Group string `json:"group"`
}

func (e Edge) String() string { return e.From + " -> " + e.To }

// How a chain instance's Sentinels change image.
const (
	// SentinelFollow changes the Sentinel image with the data image.
	SentinelFollow = "follow"
	// SentinelSeparate changes it by sentinel_image_upgrade, behind the
	// data image.
	SentinelSeparate = "separate"
)

// Chain is the part of the transition graph an instance moves through.
type Chain struct {
	// Start are the versions a chain starts on, in turn per reset.
	Start []string `json:"start"`
	// Versions are the versions the instance may run; only edges between
	// them are taken.
	Versions []string `json:"versions"`
	// Expect, if set, takes only edges of these expectations.
	Expect []string `json:"expect"`
	// Group takes only the edges of that group, none of the main graph. Empty
	// takes the main graph.
	Group string `json:"group"`
	// Sentinel is follow, separate, or empty for the Sentinel image the
	// instance is created with.
	Sentinel string `json:"sentinel"`
}

// VersionNamed returns the version of that name.
func (c *Config) VersionNamed(name string) (Version, bool) {
	i := slices.IndexFunc(c.Versions, func(v Version) bool { return v.Name == name })
	if i < 0 {
		return Version{}, false
	}
	return c.Versions[i], true
}

// VersionOf returns the version whose image is image.
func (c *Config) VersionOf(image string) (Version, bool) {
	i := slices.IndexFunc(c.Versions, func(v Version) bool { return v.Image == image })
	if i < 0 {
		return Version{}, false
	}
	return c.Versions[i], true
}

// EdgesFrom returns the edges from a version that the chain may take, in
// config order.
func (c *Config) EdgesFrom(from string, ch *Chain) []Edge {
	var out []Edge
	for _, e := range c.Edges {
		if e.From == from && ch.allows(e) {
			out = append(out, e)
		}
	}
	return out
}

// Edge returns the edge from one version to another, if there is one.
func (c *Config) Edge(from, to string) (Edge, bool) {
	i := slices.IndexFunc(c.Edges, func(e Edge) bool { return e.From == from && e.To == to })
	if i < 0 {
		return Edge{}, false
	}
	return c.Edges[i], true
}

func (ch *Chain) allows(e Edge) bool {
	return e.Group == ch.Group && slices.Contains(ch.Versions, e.From) && slices.Contains(ch.Versions, e.To) &&
		(len(ch.Expect) == 0 || slices.Contains(ch.Expect, e.Expect))
}

// Reaches reports whether the chain leads from one version to another,
// or they are the same.
func (c *Config) Reaches(ch *Chain, from, to string) bool {
	seen := map[string]bool{from: true}
	next := []string{from}
	for len(next) > 0 {
		v := next[0]
		next = next[1:]
		for _, e := range c.EdgesFrom(v, ch) {
			if !seen[e.To] {
				seen[e.To] = true
				next = append(next, e.To)
			}
		}
	}
	return seen[to]
}

func (v *Version) setDefaults() {
	repo, tag := splitImage(v.Image)
	v.Server = path.Base(repo)
	v.Release, _, _ = strings.Cut(tag, "-")
}

// splitImage returns an image's repository and tag, without its digest. A
// registry's port is part of the repository.
func splitImage(image string) (repo, tag string) {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, ""
}

// validateVersions checks the catalogue and the graph: unique names, exact
// tags, edges between known versions that change the version, and no
// downgrade that expects ok.
func (c *Config) validateVersions() error {
	seen := map[string]bool{}
	images := map[string]bool{}
	for _, v := range c.Versions {
		switch {
		case v.Name == "" || v.Image == "":
			return errors.New("versions: name and image are required")
		case seen[v.Name]:
			return fmt.Errorf("versions: %s is configured twice", v.Name)
		case images[v.Image]:
			return fmt.Errorf("versions: image %s is configured twice", v.Image)
		case strings.Count(v.Release, ".") < 2:
			return fmt.Errorf("versions: %s: %s is not an exact patch tag", v.Name, v.Image)
		}
		seen[v.Name], images[v.Image] = true, true
	}
	edges := map[string]bool{}
	for _, e := range c.Edges {
		switch {
		case !seen[e.From] || !seen[e.To]:
			return fmt.Errorf("edges: %s: unknown version", e)
		case e.From == e.To:
			return fmt.Errorf("edges: %s: an edge must change the version", e)
		case e.Expect != ExpectOK && e.Expect != ExpectFail && e.Expect != ExpectUnknown:
			return fmt.Errorf("edges: %s: expect must be %s, %s or %s", e, ExpectOK, ExpectFail, ExpectUnknown)
		case edges[e.String()]:
			return fmt.Errorf("edges: %s is configured twice", e)
		}
		from, _ := c.VersionNamed(e.From)
		to, _ := c.VersionNamed(e.To)
		if from.Server == to.Server && compareReleases(to.Release, from.Release) < 0 && e.Expect == ExpectOK {
			return fmt.Errorf("edges: %s is a downgrade, so it cannot expect %s", e, ExpectOK)
		}
		edges[e.String()] = true
	}
	return nil
}

// validateChain checks that an instance's chain starts on known versions
// with an edge to take, and that every version it may run is reachable
// from a start.
func (c *Config) validateChain(in Instance) error {
	ch := in.Chain
	if len(ch.Start) == 0 || len(ch.Versions) == 0 {
		return errors.New("start and versions are required")
	}
	for _, v := range ch.Versions {
		if _, ok := c.VersionNamed(v); !ok {
			return fmt.Errorf("unknown version %s", v)
		}
	}
	for _, x := range ch.Expect {
		if x != ExpectOK && x != ExpectFail && x != ExpectUnknown {
			return fmt.Errorf("expect: %q must be %s, %s or %s", x, ExpectOK, ExpectFail, ExpectUnknown)
		}
	}
	for _, s := range ch.Start {
		if !slices.Contains(ch.Versions, s) {
			return fmt.Errorf("start %s is not one of its versions", s)
		}
		if len(c.EdgesFrom(s, ch)) == 0 {
			return fmt.Errorf("start %s has no edge to take", s)
		}
	}
	for _, v := range ch.Versions {
		if !slices.ContainsFunc(ch.Start, func(s string) bool { return c.Reaches(ch, s, v) }) {
			return fmt.Errorf("version %s is not reachable from a start", v)
		}
	}
	switch ch.Sentinel {
	case "":
	case SentinelFollow, SentinelSeparate:
		if in.Mode != ModeSentinel {
			return fmt.Errorf("sentinel: %s needs a sentinel instance", ch.Sentinel)
		}
	default:
		return fmt.Errorf("sentinel must be %q, %q or empty", SentinelFollow, SentinelSeparate)
	}
	return nil
}

// compareReleases compares two x.y.z releases numerically.
func compareReleases(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}
