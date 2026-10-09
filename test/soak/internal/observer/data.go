package observer

import (
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

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
