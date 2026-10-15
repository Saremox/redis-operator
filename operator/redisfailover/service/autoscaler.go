package service

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

// AutoscalerStateKey is the key of the state in the autoscaler ConfigMap.
const AutoscalerStateKey = "state"

// SpecMemoryLimitAnnotation holds the spec memory limit of the redis
// container at the last Ensure of the redis StatefulSet. Only a change of the
// spec limit lets the autoscaler lower the template.
const SpecMemoryLimitAnnotation = "redisfailovers.databases.spotahome.com/spec-memory-limit"

// IsRedisPod reports whether pod is a redis pod of rf, also without its
// StatefulSet.
func IsRedisPod(rf *redisfailoverv1.RedisFailover, pod *corev1.Pod) bool {
	for k, v := range generateSelectorLabels(redisRoleName, rf.Name) {
		if pod.Labels[k] != v {
			return false
		}
	}
	return true
}

// GenerateAutoscalerStateConfigMap returns the ConfigMap that holds the state
// of the memory autoscaler. With the owner references, Kubernetes deletes it
// together with the RedisFailover.
func GenerateAutoscalerStateConfigMap(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, state string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            GetAutoscalerStateName(rf),
			Namespace:       rf.Namespace,
			Labels:          util.MergeLabels(labels, generateSelectorLabels(autoscalerRoleName, rf.Name)),
			OwnerReferences: ownerRefs,
		},
		Data: map[string]string{AutoscalerStateKey: state},
	}
}

// TemplateRedisMemoryLimit returns the memory limit of the redis container in
// the pod template of ss, or 0 without one.
func TemplateRedisMemoryLimit(ss *appsv1.StatefulSet) int64 {
	for _, c := range ss.Spec.Template.Spec.Containers {
		if c.Name == redisContainerName {
			return c.Resources.Limits.Memory().Value()
		}
	}
	return 0
}

// RequestedRedisMemoryLimit returns the memory limit of the redis container in
// the pod spec, or 0 without one.
func RequestedRedisMemoryLimit(pod *corev1.Pod) int64 {
	for _, c := range pod.Spec.Containers {
		if c.Name == redisContainerName {
			return c.Resources.Limits.Memory().Value()
		}
	}
	return 0
}

// AppliedRedisMemoryLimit returns the memory limit of the redis container
// that the kubelet reports as applied. A kubelet without in-place resize
// reports none, and then a running container has the limit of the pod spec.
// Without a report, it returns 0 for a container that does not run.
func AppliedRedisMemoryLimit(pod *corev1.Pod) int64 {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != redisContainerName {
			continue
		}
		if cs.Resources != nil {
			return cs.Resources.Limits.Memory().Value()
		}
		if cs.State.Running == nil {
			return 0
		}
		for _, c := range pod.Spec.Containers {
			if c.Name == redisContainerName {
				return c.Resources.Limits.Memory().Value()
			}
		}
	}
	return 0
}
