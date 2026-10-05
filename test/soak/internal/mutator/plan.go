package mutator

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
)

const (
	roleLabel     = "redisfailovers-role"
	roleMaster    = "master"
	roleReplica   = "slave"
	redisName     = "redis"
	resizeRequest = "redisfailovers.databases.spotahome.com/resize-requested-at"
	// secretChecksum is the checksum of the password on the redis pod
	// template, which rolls the pods onto a changed Secret.
	secretChecksum = "redisfailovers.databases.spotahome.com/secret-checksum"
	sentinelPort   = 26379
	mi             = 1 << 20
)

func stepRand(seed int64, in config.Instance, step int) *rand.Rand {
	return config.StepRand(seed, in.Name, step)
}

func pickKind(r *rand.Rand, m config.Mutations) config.Kind {
	return config.Pick(r, m.Kinds)
}

// pickOther returns a value of rg other than current.
func pickOther(r *rand.Rand, rg config.Range, current int64) int64 {
	if current < rg.Min || current > rg.Max {
		return rg.Min + r.Int64N(rg.Max-rg.Min+1)
	}
	v := rg.Min + r.Int64N(rg.Max-rg.Min)
	if v >= current {
		v++
	}
	return v
}

// plan is one picked mutation.
type plan struct {
	kind config.Kind
	// skip is why the mutator cannot apply the mutation now.
	skip   string
	params string
	// patch is a JSON merge patch of the RedisFailover.
	patch []byte
	// secret is a password to set in a Secret, before the patch.
	secret *secretChange
	// pod is the pod to delete, with its UID.
	pod   string
	uid   types.UID
	force bool
	// reset is a kill of an instance's only pod without a volume, or a
	// recreation of the instance, which loses the data by design.
	reset bool
	// edge is a version change along the graph; flip changes the image
	// outside it, and only its mixed window is measured.
	edge *transition
	flip *transition
	// action applies a mutation that is neither a patch nor a delete;
	// offline is scenario C.
	action  func(context.Context) error
	offline *offline
	// converged is the mutation's own convergence signal: nil once the
	// change is complete, given what fetch reads. probe, if set, must also
	// return nil.
	converged func(state) error
	fetch     fetchOpts
	probe     func(context.Context) error
}

type secretChange struct {
	name     string
	password string
}

// newPassword returns a random password. Logs never show it, so a replayed
// step uses a different password. This has no effect on the test.
func newPassword() string {
	return crand.Text()
}

func skipped(kind config.Kind, format string, a ...any) plan {
	return plan{kind: kind, skip: fmt.Sprintf(format, a...)}
}

func newPlan(r *rand.Rand, kind config.Kind, m config.Mutations, s state, master string, d Data) plan {
	switch kind {
	case config.RedisReplicas:
		want := int32(pickOther(r, m.RedisReplicas, int64(s.rf.Spec.Redis.Replicas)))
		return plan{
			kind:      kind,
			params:    fmt.Sprintf("redis.replicas %d -> %d", s.rf.Spec.Redis.Replicas, want),
			patch:     mergePatch(map[string]any{"redis": map[string]any{"replicas": want}}),
			converged: redisReplicasConverged(want),
		}
	case config.SentinelReplicas:
		want := int32(pickOther(r, m.SentinelReplicas, int64(s.rf.Spec.Sentinel.Replicas)))
		return plan{
			kind:      kind,
			params:    fmt.Sprintf("sentinel.replicas %d -> %d", s.rf.Spec.Sentinel.Replicas, want),
			patch:     mergePatch(map[string]any{"sentinel": map[string]any{"replicas": want}}),
			converged: sentinelReplicasConverged(want),
		}
	case config.RedisResources:
		return planResources(r, m.Resources, s)
	case config.KillMaster, config.KillMasterForce:
		pod, why := labelledMaster(s, master)
		if why != "" {
			return skipped(kind, "%s", why)
		}
		p := planKill(kind, pod, kind == config.KillMasterForce, redisPodReplaced)
		p.reset = s.rf.Spec.Redis.Replicas == 1 && s.rf.Spec.Redis.Storage.PersistentVolumeClaim == nil
		return p
	case config.KillReplica:
		replicas := withRole(s.redis, roleReplica)
		if len(replicas) == 0 {
			return skipped(kind, "no replica")
		}
		pod := replicas[r.IntN(len(replicas))]
		return planKill(kind, pod, r.Float64() < m.ForceDeleteProbability, redisPodReplaced)
	case config.KillSentinel:
		if len(s.sentinels) == 0 {
			return skipped(kind, "no sentinel pod")
		}
		want := s.rf.Spec.Sentinel.Replicas
		pod := s.sentinels[r.IntN(len(s.sentinels))]
		return planKill(kind, pod, r.Float64() < m.ForceDeleteProbability, func(name string, uid types.UID) func(state) error {
			return sentinelPodReplaced(uid, want)
		})
	case config.PasswordRotate, config.AuthAdd, config.AuthRemove, config.PasswordRotateOffline:
		return planAuth(kind, s)
	case config.SentinelToggle:
		want := !s.rf.SentinelEnabled()
		return plan{
			kind:      kind,
			params:    fmt.Sprintf("sentinel.enabled %t -> %t", !want, want),
			patch:     mergePatch(map[string]any{"sentinel": map[string]any{"enabled": want}}),
			fetch:     fetchOpts{sentinelObjects: true},
			converged: toggleConverged(want, s.rf.Spec.Sentinel.Replicas, s.rf.Spec.Redis.Replicas),
		}
	}
	mm := s.rf.Spec.Redis.MaxMemory
	if mm == nil {
		return skipped(kind, "the RedisFailover has no maxMemory")
	}
	switch kind {
	case config.RedisMemory:
		return planMemory(r, m.RedisMemory, s)
	case config.MaxMemoryPolicy:
		var choices []string
		for _, p := range m.MaxMemoryPolicies {
			if p != mm.Policy {
				choices = append(choices, p)
			}
		}
		want := choices[r.IntN(len(choices))]
		return plan{
			kind:   kind,
			params: fmt.Sprintf("redis.maxMemory.policy %s -> %s", mm.Policy, want),
			patch:  mergePatch(map[string]any{"redis": map[string]any{"maxMemory": map[string]any{"policy": want}}}),
			converged: maxMemoryConverged(s.rf.Spec.Redis.Replicas, func(mm *redisfailoverv1.MaxMemorySettings) bool {
				return mm.Policy == want
			}),
		}
	case config.MaxMemoryPercent:
		want := int32(pickOther(r, m.MaxMemoryPercent, int64(mm.Percent)))
		return plan{
			kind:   kind,
			params: fmt.Sprintf("redis.maxMemory.percent %d -> %d", mm.Percent, want),
			patch:  mergePatch(map[string]any{"redis": map[string]any{"maxMemory": map[string]any{"percent": want}}}),
			converged: maxMemoryConverged(s.rf.Spec.Redis.Replicas, func(mm *redisfailoverv1.MaxMemorySettings) bool {
				return mm.Percent == want
			}),
		}
	case config.FillBurst:
		if mm.Policy != "noeviction" {
			return skipped(kind, "policy %s", mm.Policy)
		}
		const hold = 10 * time.Second
		replicas := s.rf.Spec.Redis.Replicas
		return plan{
			kind:   kind,
			params: fmt.Sprintf("write past maxmemory, hold %s", hold),
			action: func(ctx context.Context) error {
				skip, err := d.Burst(ctx, hold)
				if skip != "" {
					return fmt.Errorf("burst not started: %s", skip)
				}
				return err
			},
			probe: d.Writable,
			converged: func(s state) error {
				return podsReady("redis", s.redis, replicas)
			},
		}
	}
	return skipped(kind, "unknown kind")
}

// planMemory changes the memory limit within rg, and a memory request in
// proportion, so the QoS class stays the same.
func planMemory(r *rand.Rand, rg config.Range, s state) plan {
	const kind = config.RedisMemory
	cur := s.rf.Spec.Redis.Resources
	lim, ok := cur.Limits[corev1.ResourceMemory]
	if !ok {
		return skipped(kind, "no memory limit")
	}
	want := resource.NewQuantity(pickOther(r, rg, lim.Value()/mi)*mi, resource.BinarySI)
	next := *cur.DeepCopy()
	next.Limits[corev1.ResourceMemory] = *want
	limits := map[string]string{"memory": want.String()}
	patch := map[string]any{"limits": limits}
	params := []string{fmt.Sprintf("limits.memory %s -> %s", lim.String(), want.String())}
	if req, ok := cur.Requests[corev1.ResourceMemory]; ok {
		nreq := want
		if !req.Equal(lim) {
			nreq = resource.NewQuantity(max(1, req.Value()*want.Value()/lim.Value()/mi)*mi, resource.BinarySI)
		}
		next.Requests[corev1.ResourceMemory] = *nreq
		patch["requests"] = map[string]string{"memory": nreq.String()}
		params = append(params, fmt.Sprintf("requests.memory %s -> %s", req.String(), nreq.String()))
	}
	if from, to := qosClass(cur), qosClass(next); from != to {
		return skipped(kind, "%s would change the QoS class from %s to %s", strings.Join(params, ", "), from, to)
	}
	before := map[string]int64{}
	for i := range s.redis {
		before[s.redis[i].Name] = maxmem.RedisLimit(&s.redis[i])
	}
	target := s.rf.MaxMemoryFor(want.Value())
	evicts := strings.HasPrefix(s.rf.Spec.Redis.MaxMemory.Policy, "allkeys-")
	return plan{
		kind:      kind,
		params:    fmt.Sprintf("%s (maxmemory %s)", strings.Join(params, ", "), maxmem.FormatBytes(target)),
		patch:     mergePatch(map[string]any{"redis": map[string]any{"resources": patch}}),
		converged: memoryConverged(next, s.rf.Spec.Redis.Replicas, before, target, evicts),
	}
}

// labelledMaster returns the one pod that is labelled master. The observer
// must agree, or the label is stale. why says what is wrong if it is not.
func labelledMaster(s state, master string) (pod corev1.Pod, why string) {
	masters := withRole(s.redis, roleMaster)
	switch {
	case len(masters) != 1:
		return pod, fmt.Sprintf("%d pods are labelled master", len(masters))
	case masters[0].Name != master:
		return pod, fmt.Sprintf("%s is labelled master, the observer saw %q", masters[0].Name, master)
	}
	return masters[0], ""
}

func planKill(kind config.Kind, p corev1.Pod, force bool, converged func(string, types.UID) func(state) error) plan {
	how := "graceful"
	if force {
		how = "force"
	}
	return plan{
		kind:      kind,
		params:    fmt.Sprintf("delete pod %s (%s)", p.Name, how),
		pod:       p.Name,
		uid:       p.UID,
		force:     force,
		converged: converged(p.Name, p.UID),
	}
}

// planAuth changes the password, adds auth or removes auth. Each converges as
// authConverged says.
func planAuth(kind config.Kind, s state) plan {
	path := s.rf.Spec.Auth.SecretPath
	p := plan{kind: kind}
	password := newPassword()
	switch kind {
	case config.AuthAdd:
		if path != "" {
			return skipped(kind, "auth is on, with secret %s", path)
		}
		p.params = "add auth with secret " + s.authSecret
		p.secret = &secretChange{name: s.authSecret, password: password}
		p.patch = mergePatch(map[string]any{"auth": map[string]any{"secretPath": s.authSecret}})
	case config.AuthRemove:
		if path == "" {
			return skipped(kind, "auth is off")
		}
		p.params = "remove auth with secret " + path
		password = ""
		p.patch = mergePatch(map[string]any{"auth": map[string]any{"secretPath": nil}})
	case config.PasswordRotate:
		if path == "" {
			return skipped(kind, "auth is off")
		}
		p.params = "rotate the password in secret " + path
		p.secret = &secretChange{name: path, password: password}
	case config.PasswordRotateOffline:
		if path == "" {
			return skipped(kind, "auth is off")
		}
		p.params = "rotate the password in secret " + path + " while the operator is stopped, put it back, rotate again"
		p.offline = &offline{secret: path, previous: s.password, first: newPassword(), second: password}
	}
	p.fetch = fetchOpts{password: &password, sentinelMaster: s.rf.SentinelEnabled()}
	p.converged = authConverged(templateChecksum(s), s.rf.Spec.Redis.Replicas)
	return p
}

func templateChecksum(s state) string {
	if s.sts == nil {
		return ""
	}
	return s.sts.Spec.Template.Annotations[secretChecksum]
}

func withRole(pods []corev1.Pod, role string) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Labels[roleLabel] == role {
			out = append(out, p)
		}
	}
	return out
}

func mergePatch(spec map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"spec": spec})
	return b
}

// planResources changes cpu, memory or both: every request and limit of
// them that the RedisFailover sets and the config bounds.
func planResources(r *rand.Rand, b config.Resources, s state) plan {
	const kind = config.RedisResources
	cur := s.rf.Spec.Redis.Resources
	type entry struct {
		list  string
		name  corev1.ResourceName
		rg    config.Range
		quant corev1.ResourceList
	}
	entries := map[corev1.ResourceName][]entry{}
	for _, e := range []entry{
		{"requests", corev1.ResourceCPU, b.Requests.CPU, cur.Requests},
		{"limits", corev1.ResourceCPU, b.Limits.CPU, cur.Limits},
		{"requests", corev1.ResourceMemory, b.Requests.Memory, cur.Requests},
		{"limits", corev1.ResourceMemory, b.Limits.Memory, cur.Limits},
	} {
		if _, ok := e.quant[e.name]; ok && e.rg.Set() {
			entries[e.name] = append(entries[e.name], e)
		}
	}
	names := slices.Sorted(maps.Keys(entries))
	switch len(names) {
	case 0:
		return skipped(kind, "the RedisFailover sets none of the configured requests and limits")
	case 2:
		// cpu, memory or both.
		if i := r.IntN(3); i < 2 {
			names = names[i : i+1]
		}
	}

	next := *cur.DeepCopy()
	patch := map[string]map[string]string{}
	var params []string
	for _, n := range names {
		for _, e := range entries[n] {
			q := e.quant[e.name]
			var nq *resource.Quantity
			if n == corev1.ResourceCPU {
				nq = resource.NewMilliQuantity(pickOther(r, e.rg, q.MilliValue()), resource.DecimalSI)
			} else {
				nq = resource.NewQuantity(pickOther(r, e.rg, q.Value()/mi)*mi, resource.BinarySI)
			}
			list := next.Requests
			if e.list == "limits" {
				list = next.Limits
			}
			list[e.name] = *nq
			if patch[e.list] == nil {
				patch[e.list] = map[string]string{}
			}
			patch[e.list][string(e.name)] = nq.String()
			params = append(params, fmt.Sprintf("%s.%s %s -> %s", e.list, e.name, q.String(), nq.String()))
		}
	}
	for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		req, okr := next.Requests[n]
		lim, okl := next.Limits[n]
		if okr && okl && req.Cmp(lim) > 0 {
			return skipped(kind, "requests.%s %s would exceed limits.%s %s", n, req.String(), n, lim.String())
		}
	}
	if from, to := qosClass(cur), qosClass(next); from != to {
		return skipped(kind, "%s would change the QoS class from %s to %s", strings.Join(params, ", "), from, to)
	}
	return plan{
		kind:      kind,
		params:    strings.Join(params, ", "),
		patch:     mergePatch(map[string]any{"redis": map[string]any{"resources": patch}}),
		converged: resourcesConverged(next, s.rf.Spec.Redis.Replicas),
	}
}

// qosClass is the QoS class of a pod with only this container.
func qosClass(r corev1.ResourceRequirements) corev1.PodQOSClass {
	if len(r.Requests) == 0 && len(r.Limits) == 0 {
		return corev1.PodQOSBestEffort
	}
	for _, n := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		lim, ok := r.Limits[n]
		if !ok {
			return corev1.PodQOSBurstable
		}
		// An unset request defaults to the limit.
		if req, ok := r.Requests[n]; ok && !req.Equal(lim) {
			return corev1.PodQOSBurstable
		}
	}
	return corev1.PodQOSGuaranteed
}
