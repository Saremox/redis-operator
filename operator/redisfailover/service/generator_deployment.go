package service

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

func generateSentinelDeployment(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference) *appsv1.Deployment {
	name := GetSentinelName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(sentinelRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rf.Spec.Sentinel.Replicas,
			Strategy: rf.Spec.Sentinel.Strategy,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: rf.Spec.Sentinel.PodAnnotations,
				},
				Spec: generateSentinelPodSpec(rf, labels),
			},
		},
	}
}

func generateSentinelPodSpec(rf *redisfailoverv1.RedisFailover, labels map[string]string) corev1.PodSpec {
	configMapName := GetSentinelName(rf)

	serviceAccountName := rf.Spec.Sentinel.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = GetSentinelServiceAccountName(rf)
	}

	spec := corev1.PodSpec{
		Affinity:                     getAffinity(rf.Spec.Sentinel.Affinity, labels),
		Tolerations:                  rf.Spec.Sentinel.Tolerations,
		TopologySpreadConstraints:    rf.Spec.Sentinel.TopologySpreadConstraints,
		NodeSelector:                 rf.Spec.Sentinel.NodeSelector,
		SecurityContext:              getSecurityContext(rf.Spec.Sentinel.SecurityContext),
		HostNetwork:                  rf.Spec.Sentinel.HostNetwork,
		DNSPolicy:                    getDNSPolicy(rf.Spec.Sentinel.DNSPolicy),
		ImagePullSecrets:             rf.Spec.Sentinel.ImagePullSecrets,
		PriorityClassName:            rf.Spec.Sentinel.PriorityClassName,
		ServiceAccountName:           serviceAccountName,
		AutomountServiceAccountToken: automountServiceAccountToken(rf.Spec.Sentinel.ServiceAccountName),
		EnableServiceLinks:           new(false),
		InitContainers:               []corev1.Container{generateSentinelConfigCopyContainer(rf)},
		Containers:                   []corev1.Container{generateSentinelContainer(rf)},
		Volumes:                      getSentinelVolumes(rf, configMapName),
	}

	if rf.Spec.Sentinel.Exporter.Enabled {
		exporter := createSentinelExporterContainer(rf)
		spec.Containers = append(spec.Containers, exporter)
	}
	if rf.Spec.Sentinel.InitContainers != nil {
		spec.InitContainers = append(spec.InitContainers, rf.Spec.Sentinel.InitContainers...)
	}

	if rf.Spec.Sentinel.ExtraContainers != nil {
		spec.Containers = append(spec.Containers, rf.Spec.Sentinel.ExtraContainers...)
	}

	return spec
}

func generateSentinelConfigCopyContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	return corev1.Container{
		Name:            "sentinel-config-copy",
		Image:           rf.Spec.Sentinel.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.ConfigCopy.ContainerSecurityContext),
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "sentinel-config",
				MountPath: "/redis",
			},
			{
				Name:      "sentinel-config-writable",
				MountPath: "/redis-writable",
			},
		},
		Command: []string{
			"cp",
			fmt.Sprintf("/redis/%s", sentinelConfigFileName),
			fmt.Sprintf("/redis-writable/%s", sentinelConfigFileName),
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
	}
}

func generateSentinelContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	return corev1.Container{
		Name:            "sentinel",
		Image:           rf.Spec.Sentinel.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.ContainerSecurityContext),
		Env:             rf.Spec.Sentinel.Env,
		Ports: []corev1.ContainerPort{
			{
				Name:          "sentinel",
				ContainerPort: 26379,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts:   getSentinelVolumeMounts(rf),
		Command:        getSentinelCommand(rf),
		Resources:      rf.Spec.Sentinel.Resources,
		LivenessProbe:  sentinelLivenessProbe(rf),
		ReadinessProbe: sentinelReadinessProbe(rf),
		StartupProbe:   sentinelStartupProbe(rf),
	}
}

func sentinelLivenessProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	if rf.Spec.Sentinel.CustomLivenessProbe != nil {
		return rf.Spec.Sentinel.CustomLivenessProbe
	}
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh",
					"-c",
					"t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p 26379 ping",
				},
			},
		},
	}
}

func sentinelReadinessProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	if rf.Spec.Sentinel.CustomReadinessProbe != nil {
		return rf.Spec.Sentinel.CustomReadinessProbe
	}
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				// The first check keeps the Sentinel not ready until it
				// monitors a real master. SENTINEL CKQUORUM keeps it not
				// ready while it cannot reach enough Sentinels for a
				// failover, for example in a network partition. Without
				// CKQUORUM, an isolated Sentinel stays ready with an old
				// master address. See
				// https://github.com/spotahome/redis-operator/issues/663.
				Command: []string{
					"sh",
					"-c",
					fmt.Sprintf("t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name %[1]s | head -n 1 | grep -vq '127.0.0.1' && $t redis-cli -h $(hostname) -p 26379 sentinel ckquorum %[1]s | grep -q '^OK'", SentinelMasterName(rf)),
				},
			},
		},
	}
}

func sentinelStartupProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	if rf.Spec.Sentinel.CustomStartupProbe != nil {
		return rf.Spec.Sentinel.CustomStartupProbe
	}
	if rf.Spec.Sentinel.StartupConfigMap == "" {
		return nil
	}
	return startupScriptProbe("/sentinel-startup/startup.sh")
}

func getSentinelVolumeMounts(rf *redisfailoverv1.RedisFailover) []corev1.VolumeMount {
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "sentinel-config-writable",
			MountPath: "/redis",
		},
	}

	if rf.Spec.Sentinel.StartupConfigMap != "" {
		startupVolumeMount := corev1.VolumeMount{
			Name:      "sentinel-startup-config",
			MountPath: "/sentinel-startup",
		}
		volumeMounts = append(volumeMounts, startupVolumeMount)
	}
	if rf.Spec.Sentinel.ExtraVolumeMounts != nil {
		volumeMounts = append(volumeMounts, rf.Spec.Sentinel.ExtraVolumeMounts...)
	}

	return volumeMounts
}

func getSentinelVolumes(rf *redisfailoverv1.RedisFailover, configMapName string) []corev1.Volume {
	executeMode := int32(0744)

	volumes := []corev1.Volume{
		{
			Name: "sentinel-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
		{
			Name: "sentinel-config-writable",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if rf.Spec.Sentinel.StartupConfigMap != "" {
		startupVolumeName := rf.Spec.Sentinel.StartupConfigMap
		startupVolume := corev1.Volume{
			Name: sentinelStartupConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: startupVolumeName,
					},
					DefaultMode: &executeMode,
				},
			},
		}
		volumes = append(volumes, startupVolume)
	}

	if rf.Spec.Sentinel.ExtraVolumes != nil {
		volumes = append(volumes, rf.Spec.Sentinel.ExtraVolumes...)
	}

	return volumes
}

func getSentinelCommand(rf *redisfailoverv1.RedisFailover) []string {
	if len(rf.Spec.Sentinel.Command) > 0 {
		return rf.Spec.Sentinel.Command
	}
	return []string{
		"redis-server",
		fmt.Sprintf("/redis/%s", sentinelConfigFileName),
		"--sentinel",
	}
}
