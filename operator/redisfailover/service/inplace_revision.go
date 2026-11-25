package service

import (
	"encoding/json"
	"fmt"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// autoscalerRevision reports whether update is a step of the memory
// autoscaler from current. effectiveRF changes only the memory limit of the
// redis container, and a memory request equal to that limit. A step only
// raises the limit to an effective limit above the spec limit. A lower limit
// holds a change of the user, for example a lower memoryMax. A raise of the
// spec below the effective limit does not change the template, so a pod from
// before it keeps the step.
func autoscalerRevision(current, update *corev1.PodTemplateSpec, limits AutoscalerLimits) bool {
	if limits.Effective == 0 {
		return false
	}
	current = current.DeepCopy()
	c, u := redisContainer(current), redisContainer(update)
	if c == nil || u == nil {
		return false
	}
	cl, cok := c.Resources.Limits[corev1.ResourceMemory]
	ul, uok := u.Resources.Limits[corev1.ResourceMemory]
	if !cok || !uok || ul.Value() != limits.Effective || ul.Value() <= limits.Spec || cl.Value() >= ul.Value() {
		return false
	}
	cr, cok := c.Resources.Requests[corev1.ResourceMemory]
	ur, uok := u.Resources.Requests[corev1.ResourceMemory]
	if cok != uok || cok && !cr.Equal(ur) && (!cr.Equal(cl) || !ur.Equal(ul)) {
		return false
	}
	c.Resources.Limits[corev1.ResourceMemory] = ul
	if cok {
		c.Resources.Requests[corev1.ResourceMemory] = ur
	}
	return equality.Semantic.DeepEqual(current, update)
}

// stepRevision reports whether current differs from update only by a larger
// redis memory limit up to stepMax, as a revision of an unknown step.
func stepRevision(update, current *corev1.PodTemplateSpec, stepMax int64) bool {
	c := redisContainer(current)
	if stepMax == 0 || c == nil {
		return false
	}
	cl := c.Resources.Limits.Memory().Value()
	return cl <= stepMax && autoscalerRevision(update, current, AutoscalerLimits{Effective: cl})
}

func redisContainer(t *corev1.PodTemplateSpec) *corev1.Container {
	for i := range t.Spec.Containers {
		if t.Spec.Containers[i].Name == redisContainerName {
			return &t.Spec.Containers[i]
		}
	}
	return nil
}

// revisionTemplates returns the templates of the pod's revision and of the
// update revision, or the reason that one is not available.
func (r *RedisFailoverHealer) revisionTemplates(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod, updateRevision string) (current, update *corev1.PodTemplateSpec, reason string, err error) {
	current, err = r.revisionTemplate(rf, pod.Labels[appsv1.ControllerRevisionHashLabelKey])
	if current == nil || err != nil {
		return nil, nil, "the pod's revision is not available", err
	}
	update, err = r.revisionTemplate(rf, updateRevision)
	if update == nil || err != nil {
		return nil, nil, "the update revision is not available", err
	}
	return current, update, "", nil
}

func (r *RedisFailoverHealer) revisionTemplate(rf *redisfailoverv1.RedisFailover, revision string) (*corev1.PodTemplateSpec, error) {
	// The revision hash label and the update revision are the ControllerRevision's name.
	cr, err := r.k8sService.GetControllerRevision(rf.Namespace, revision)
	// Forbidden without the RBAC added for in-place resize: recreate instead.
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The StatefulSet controller stores the template as a patch of the
	// StatefulSet: {"spec":{"template":{...}}}.
	var data struct {
		Spec struct {
			Template corev1.PodTemplateSpec `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cr.Data.Raw, &data); err != nil {
		return nil, fmt.Errorf("decode controllerrevision %s/%s: %w", rf.Namespace, revision, err)
	}
	return &data.Spec.Template, nil
}

func withoutResources(t *corev1.PodTemplateSpec) *corev1.PodTemplateSpec {
	t = t.DeepCopy()
	for i := range t.Spec.Containers {
		t.Spec.Containers[i].Resources.Requests = nil
		t.Spec.Containers[i].Resources.Limits = nil
	}
	return t
}

// withDefaultRequests sets unset requests to their limits, as the API server
// does for pods but not for the templates they are created from.
func withDefaultRequests(r corev1.ResourceRequirements) corev1.ResourceRequirements {
	r = *r.DeepCopy()
	for name, q := range r.Limits {
		if _, ok := r.Requests[name]; !ok {
			if r.Requests == nil {
				r.Requests = corev1.ResourceList{}
			}
			r.Requests[name] = q
		}
	}
	return r
}

// keepsTemplate reports whether the pod spec and the applied resources have
// the resources of the template t, with a redis memory limit at or above
// the one of t.
func keepsTemplate(pod *corev1.Pod, t *corev1.PodTemplateSpec) bool {
	spec, applied := map[string]*corev1.ResourceRequirements{}, map[string]*corev1.ResourceRequirements{}
	for i := range pod.Spec.Containers {
		spec[pod.Spec.Containers[i].Name] = &pod.Spec.Containers[i].Resources
	}
	for _, cs := range pod.Status.ContainerStatuses {
		applied[cs.Name] = cs.Resources
	}
	for _, c := range t.Spec.Containers {
		for _, got := range []*corev1.ResourceRequirements{spec[c.Name], applied[c.Name]} {
			if !keepsResources(got, c) {
				return false
			}
		}
	}
	return true
}

// keepsResources reports whether got has the resources of the template
// container c, with a redis memory limit at or above the one of c.
func keepsResources(got *corev1.ResourceRequirements, c corev1.Container) bool {
	if got == nil {
		return false
	}
	want := withDefaultRequests(c.Resources)
	if q, ok := want.Limits[corev1.ResourceMemory]; ok && c.Name == redisContainerName {
		if got.Limits.Memory().Cmp(q) < 0 {
			return false
		}
		want.Limits[corev1.ResourceMemory] = got.Limits[corev1.ResourceMemory]
		if r, ok := got.Requests[corev1.ResourceMemory]; ok && want.Requests.Memory().Equal(q) {
			want.Requests[corev1.ResourceMemory] = r
		}
	}
	return sameResizable(*got, want)
}

func sameResizable(a, b corev1.ResourceRequirements) bool {
	for _, name := range resizableResources {
		for _, list := range [][2]corev1.ResourceList{{a.Requests, b.Requests}, {a.Limits, b.Limits}} {
			qa, oka := list[0][name]
			qb, okb := list[1][name]
			if oka != okb || !qa.Equal(qb) {
				return false
			}
		}
	}
	return true
}
