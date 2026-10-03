package observer

import (
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// evictions sums the evicted_keys deltas of every redis pod. A pod's
// counter restarts with its process, which a new run_id tells; a pod that
// is new after the first round counts from 0.
type evictions struct {
	started bool
	pods    map[string]evictionCount
}

type evictionCount struct {
	runID string
	n     int64
}

// update takes the counters of the pods that answered INFO, and the names
// of every pod, and returns the keys evicted since the last update.
func (e *evictions) update(counts map[string]evictionCount, pods []string) int64 {
	if e.pods == nil {
		e.pods = map[string]evictionCount{}
	}
	var delta int64
	for name, c := range counts {
		last, ok := e.pods[name]
		switch {
		case ok && last.runID == c.runID:
			delta += max(0, c.n-last.n)
		case ok || e.started:
			delta += c.n
		}
		e.pods[name] = c
	}
	for name := range e.pods {
		if !slices.Contains(pods, name) {
			delete(e.pods, name)
		}
	}
	e.started = true
	return delta
}

// oomKills identifies the OOM kills a pod's containers report, by the
// pod's UID, the container and when it was killed.
func oomKills(p *corev1.Pod) []string {
	var kills []string
	statuses := append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
			if t == nil || t.Reason != "OOMKilled" {
				continue
			}
			k := string(p.UID) + "/" + cs.Name + "@" + t.FinishedAt.UTC().Format(time.RFC3339)
			if !slices.Contains(kills, k) {
				kills = append(kills, k)
			}
		}
	}
	return kills
}

// observeOOMKills counts every OOM kill not seen before as a finding. It
// ignores the kills before the start of the tester.
func (o *Observer) observeOOMKills(s snapshot) {
	pods := make([]pod, 0, len(s.redis)+len(s.sentinels))
	for _, p := range s.redis {
		pods = append(pods, p.pod)
	}
	for _, p := range s.sentinels {
		pods = append(pods, p.pod)
	}
	seen := map[string]bool{}
	for _, p := range pods {
		for _, k := range p.OOMKills {
			container, at, _ := strings.Cut(k[strings.Index(k, "/")+1:], "@")
			if t, err := time.Parse(time.RFC3339, at); err == nil && t.Before(o.started) {
				continue
			}
			seen[k] = true
			if o.oomSeen[k] {
				continue
			}
			o.findings.WithLabelValues(invOOMKilled).Inc()
			o.log.Warn("invariant violated", "invariant", invOOMKilled, "finding", true,
				"reason", "OOM-killed", "pod", p.Name, "container", container, "at", at)
		}
	}
	o.ok.WithLabelValues(invOOMKilled).Set(gauge(len(seen) == 0))
	o.oomSeen = seen
}

// observeData exports the master's dataset and memory, and the keys every
// pod evicted.
func (o *Observer) observeData(s snapshot) {
	counts := map[string]evictionCount{}
	names := make([]string, 0, len(s.redis))
	for _, p := range s.redis {
		names = append(names, p.Name)
		if p.info != nil {
			counts[p.Name] = evictionCount{runID: p.info["run_id"], n: p.info.int("evicted_keys")}
		}
	}
	o.evicted.Add(float64(o.evictions.update(counts, names)))
	master, err := s.master()
	if err != nil {
		return
	}
	var keys int64
	for _, k := range slices.Sorted(maps.Keys(master.info)) {
		if strings.HasPrefix(k, "db") {
			keys += keyspaceKeys(master.info[k])
		}
	}
	o.datasetKeys.Set(float64(keys))
	o.usedMemory.Set(float64(master.info.int("used_memory")))
	o.maxMemory.Set(float64(master.info.int("maxmemory")))
}

// keyspaceKeys parses the keys of an INFO keyspace line's value, like
// keys=1,expires=0,avg_ttl=0.
func keyspaceKeys(v string) int64 {
	for f := range strings.SplitSeq(v, ",") {
		if n, ok := strings.CutPrefix(f, "keys="); ok {
			return info{"n": n}.int("n")
		}
	}
	return 0
}
