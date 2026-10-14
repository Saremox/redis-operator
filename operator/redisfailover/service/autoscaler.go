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
