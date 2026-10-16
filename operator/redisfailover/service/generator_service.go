package service

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

func generateSentinelService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetSentinelName(rf)
	namespace := rf.Namespace

	sentinelTargetPort := intstr.FromInt32(26379)
	selectorLabels := generateSelectorLabels(sentinelRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Sentinel.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels,
			Ports: []corev1.ServicePort{
				{
					Name:       "sentinel",
					Port:       26379,
					TargetPort: sentinelTargetPort,
					Protocol:   "TCP",
				},
			},
		},
	}

	// The sentinel exporter sidecar listens on sentinelExporterListenPort, but
	// without a matching service port there is no way to scrape it through the
	// service.
	if rf.Spec.Sentinel.Exporter.Enabled {
		port := sentinelExporterListenPort(rf)
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       "metrics",
			Port:       port,
			TargetPort: intstr.FromInt(int(port)),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	return svc
}

func generateRedisService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	name := GetRedisName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)
	defaultAnnotations := map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   strconv.Itoa(int(redisExporterListenPort(rf))),
		"prometheus.io/path":   "/metrics",
	}
	annotations := util.MergeLabels(defaultAnnotations, rf.Spec.Redis.ServiceAnnotations)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{
				{
					Port:     redisExporterListenPort(rf),
					Protocol: corev1.ProtocolTCP,
					Name:     exporterPortName,
				},
			},
			Selector: selectorLabels,
		},
	}
}

func generateRedisMasterService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	return generateRedisRoleService(rf, GetRedisMasterName(rf), redisRoleLabelMaster, labels, ownerRefs)
}

func generateRedisSlaveService(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	return generateRedisRoleService(rf, GetRedisSlaveName(rf), redisRoleLabelSlave, labels, ownerRefs)
}

// generateRedisRoleService returns the Service that selects the Redis pods
// with the role label role.
func generateRedisRoleService(rf *redisfailoverv1.RedisFailover, name, role string, labels map[string]string, ownerRefs []metav1.OwnerReference) *corev1.Service {
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	selectorLabels = util.MergeLabels(selectorLabels, map[string]string{
		redisRoleLabelKey: role,
	})
	labels = util.MergeLabels(labels, selectorLabels)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
			Annotations:     rf.Spec.Redis.ServiceAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       rf.Spec.Redis.Port,
					TargetPort: intstr.FromString("redis"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Selector: selectorLabels,
		},
	}
}
