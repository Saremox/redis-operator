package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

func generateRedisStatefulSet(rf *redisfailoverv1.RedisFailover, labels map[string]string, ownerRefs []metav1.OwnerReference, password string) *appsv1.StatefulSet {
	name := GetRedisName(rf)
	namespace := rf.Namespace

	selectorLabels := generateSelectorLabels(redisRoleName, rf.Name)
	labels = util.MergeLabels(labels, selectorLabels)
	// The operator changes the role label of the master pod, so a pod
	// anti-affinity that matches on it does not count the master.
	affinityLabels := labels
	labels = util.MergeLabels(labels, generateRedisDefaultRoleLabel())

	podAnnotations := util.MergeAnnotations(rf.Spec.Redis.PodAnnotations, map[string]string{
		redisAuthSecretChecksumAnnotation: redisAuthSecretChecksum(rf, password),
	})

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Annotations:     rf.Annotations,
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: ownerRefs,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &rf.Spec.Redis.Replicas,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations,
				},
				Spec: generateRedisPodSpec(rf, affinityLabels),
			},
			VolumeClaimTemplates: redisVolumeClaimTemplates(rf, ownerRefs),
		},
	}
}

func generateRedisPodSpec(rf *redisfailoverv1.RedisFailover, affinityLabels map[string]string) corev1.PodSpec {
	terminationGracePeriodSeconds := getTerminationGracePeriodSeconds(rf)

	spec := corev1.PodSpec{
		Affinity:                      getAffinity(rf.Spec.Redis.Affinity, affinityLabels),
		Tolerations:                   rf.Spec.Redis.Tolerations,
		TopologySpreadConstraints:     rf.Spec.Redis.TopologySpreadConstraints,
		NodeSelector:                  rf.Spec.Redis.NodeSelector,
		SecurityContext:               getSecurityContext(rf.Spec.Redis.SecurityContext),
		HostNetwork:                   rf.Spec.Redis.HostNetwork,
		DNSPolicy:                     getDnsPolicy(rf.Spec.Redis.DNSPolicy),
		ImagePullSecrets:              rf.Spec.Redis.ImagePullSecrets,
		PriorityClassName:             rf.Spec.Redis.PriorityClassName,
		ServiceAccountName:            rf.Spec.Redis.ServiceAccountName,
		AutomountServiceAccountToken:  automountServiceAccountToken(rf.Spec.Redis.ServiceAccountName),
		EnableServiceLinks:            new(false),
		TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
		Containers:                    []corev1.Container{generateRedisContainer(rf)},
		Volumes:                       getRedisVolumes(rf),
	}

	if rf.Spec.Redis.Exporter.Enabled {
		exporter := createRedisExporterContainer(rf)
		spec.Containers = append(spec.Containers, exporter)
	}

	// Runs before any user-supplied init container, so a restore or migration
	// step starts from a directory with no stale tempfiles in it.
	spec.InitContainers = append(
		[]corev1.Container{createRDBTempfileCleanupContainer(rf)},
		spec.InitContainers...,
	)

	if rf.Spec.Redis.InitContainers != nil {
		initContainers := getInitContainersWithRedisEnv(rf)
		spec.InitContainers = append(spec.InitContainers, initContainers...)
	}

	if rf.Spec.Redis.ExtraContainers != nil {
		extraContainers := getExtraContainersWithRedisEnv(rf)
		spec.Containers = append(spec.Containers, extraContainers...)
	}

	// The env of the user comes before the env of the operator. On a duplicate
	// name the last one wins, so the user cannot replace REDIS_ADDR, REDIS_PORT,
	// REDIS_USER, and REDIS_PASSWORD (only set with auth.secretPath).
	redisEnv := getRedisEnv(rf)
	mainEnv := append(spec.Containers[0].Env, rf.Spec.Redis.Env...)
	spec.Containers[0].Env = append(mainEnv, redisEnv...)

	return spec
}

func redisAuthSecretChecksum(rf *redisfailoverv1.RedisFailover, password string) string {
	mac := hmac.New(sha256.New, []byte(rf.Namespace+"/"+rf.Name))
	_, _ = mac.Write([]byte(password))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// generateRedisContainer returns the Redis container without its env.
func generateRedisContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	return corev1.Container{
		Name:            redisContainerName,
		Image:           rf.Spec.Redis.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Redis.ContainerSecurityContext),
		Ports: []corev1.ContainerPort{
			{
				Name:          "redis",
				ContainerPort: rf.Spec.Redis.Port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: getRedisVolumeMounts(rf),
		Command:      getRedisCommand(rf),
		Resources:    rf.Spec.Redis.Resources,
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "/redis-shutdown/shutdown.sh"},
				},
			},
		},
		LivenessProbe:  redisLivenessProbe(rf),
		ReadinessProbe: redisReadinessProbe(rf),
		StartupProbe:   redisStartupProbe(rf),
	}
}

func redisVolumeClaimTemplates(rf *redisfailoverv1.RedisFailover, ownerRefs []metav1.OwnerReference) []corev1.PersistentVolumeClaim {
	if rf.Spec.Redis.Storage.PersistentVolumeClaim == nil {
		return nil
	}
	pvc := corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              rf.Spec.Redis.Storage.PersistentVolumeClaim.Name,
			Labels:            rf.Spec.Redis.Storage.PersistentVolumeClaim.Labels,
			Annotations:       rf.Spec.Redis.Storage.PersistentVolumeClaim.Annotations,
			CreationTimestamp: metav1.Time{},
		},
		Spec:   rf.Spec.Redis.Storage.PersistentVolumeClaim.Spec,
		Status: rf.Spec.Redis.Storage.PersistentVolumeClaim.Status,
	}
	if !rf.Spec.Redis.Storage.KeepAfterDeletion {
		// The owner reference makes Kubernetes delete the PVCs with the
		// RedisFailover.
		pvc.OwnerReferences = ownerRefs
	}
	return []corev1.PersistentVolumeClaim{
		pvc,
	}
}

func redisLivenessProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	if rf.Spec.Redis.CustomLivenessProbe != nil {
		return rf.Spec.Redis.CustomLivenessProbe
	}
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		FailureThreshold:    6,
		PeriodSeconds:       15,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{
					"sh",
					"-c",
					// Bounded like ready.sh: a frozen server never answers.
					// redis-cli exits 0 on an error reply. With a Redis password,
					// a refused login gets NOAUTH, and the grep fails the probe.
					// A server that loads its dataset answers LOADING and is
					// alive: a restart starts the load again.
					fmt.Sprintf("t=; command -v timeout >/dev/null 2>&1 && t=\"timeout 2\"; $t redis-cli -h $(hostname) -p %[1]v --user pinger --pass pingpass --no-auth-warning ping | grep -qE '^(PONG|LOADING)'", rf.Spec.Redis.Port),
				},
			},
		},
	}
}

func redisReadinessProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	if rf.Spec.Redis.CustomReadinessProbe != nil {
		return rf.Spec.Redis.CustomReadinessProbe
	}
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/sh", "/redis-readiness/ready.sh"},
			},
		},
	}
}

func redisStartupProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	if rf.Spec.Redis.CustomStartupProbe != nil {
		return rf.Spec.Redis.CustomStartupProbe
	}
	if rf.Spec.Redis.StartupConfigMap == "" {
		return nil
	}
	return startupScriptProbe("/redis-startup/startup.sh")
}

// createRDBTempfileCleanupContainer removes old RDB tempfiles before Redis
// starts.
//
// During a BGSAVE, Redis writes temp-<pid>.rdb and then renames it to the
// dump file. When the process stops during the save, for example after an
// OOM kill, the tempfile stays. Redis removes only the tempfile of its own
// child process. The files collect over restarts until the volume is full,
// and then each BGSAVE fails.
//
// rdb.c sets the temp-<pid>.rdb name, and dbfilename does not change it, so
// the pattern cannot match the dump file. An init container runs before
// Redis, so no BGSAVE runs during the delete.
func createRDBTempfileCleanupContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	return corev1.Container{
		Name:            "rdb-tempfile-cleanup",
		Image:           rf.Spec.Redis.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Redis.ContainerSecurityContext),
		Command: []string{
			"sh", "-c",
			"find /data -maxdepth 1 -type f -name 'temp-*.rdb' -delete",
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      getRedisDataVolumeName(rf),
				MountPath: "/data",
			},
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

func getRedisVolumeMounts(rf *redisfailoverv1.RedisFailover) []corev1.VolumeMount {
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      redisConfigurationVolumeName,
			MountPath: "/redis",
		},
		{
			Name:      redisShutdownConfigurationVolumeName,
			MountPath: "/redis-shutdown",
		},
		{
			Name:      redisReadinessVolumeName,
			MountPath: "/redis-readiness",
		},
		{
			Name:      getRedisDataVolumeName(rf),
			MountPath: "/data",
		},
	}

	if rf.Spec.Redis.StartupConfigMap != "" {
		startupVolumeMount := corev1.VolumeMount{
			Name:      redisStartupConfigurationVolumeName,
			MountPath: "/redis-startup",
		}

		volumeMounts = append(volumeMounts, startupVolumeMount)
	}

	if rf.Spec.Redis.ExtraVolumeMounts != nil {
		volumeMounts = append(volumeMounts, rf.Spec.Redis.ExtraVolumeMounts...)
	}

	return volumeMounts
}

func getRedisVolumes(rf *redisfailoverv1.RedisFailover) []corev1.Volume {
	configMapName := GetRedisName(rf)
	shutdownConfigMapName := GetRedisShutdownConfigMapName(rf)
	readinessConfigMapName := GetRedisReadinessName(rf)

	executeMode := int32(0744)
	volumes := []corev1.Volume{
		{
			Name: redisConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName,
					},
				},
			},
		},
		{
			Name: redisShutdownConfigurationVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: shutdownConfigMapName,
					},
					DefaultMode: &executeMode,
				},
			},
		},
		{
			Name: redisReadinessVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: readinessConfigMapName,
					},
					DefaultMode: &executeMode,
				},
			},
		},
	}

	if rf.Spec.Redis.StartupConfigMap != "" {
		startupVolumeName := rf.Spec.Redis.StartupConfigMap
		startupVolume := corev1.Volume{
			Name: redisStartupConfigurationVolumeName,
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

	if rf.Spec.Redis.ExtraVolumes != nil {
		volumes = append(volumes, rf.Spec.Redis.ExtraVolumes...)
	}

	dataVolume := getRedisDataVolume(rf)
	if dataVolume != nil {
		volumes = append(volumes, *dataVolume)
	}

	return volumes
}

func getRedisDataVolume(rf *redisfailoverv1.RedisFailover) *corev1.Volume {
	// A PVC comes from the volumeClaimTemplates, so it needs no volume here.
	// Without a storage setting, the data volume is an EmptyDir.
	switch {
	case rf.Spec.Redis.Storage.PersistentVolumeClaim != nil:
		return nil
	case rf.Spec.Redis.Storage.EmptyDir != nil:
		return &corev1.Volume{
			Name: redisStorageVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: rf.Spec.Redis.Storage.EmptyDir,
			},
		}
	default:
		return &corev1.Volume{
			Name: redisStorageVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}
	}
}

func getRedisDataVolumeName(rf *redisfailoverv1.RedisFailover) string {
	switch {
	case rf.Spec.Redis.Storage.PersistentVolumeClaim != nil:
		return rf.Spec.Redis.Storage.PersistentVolumeClaim.Name
	case rf.Spec.Redis.Storage.EmptyDir != nil:
		return redisStorageVolumeName
	default:
		return redisStorageVolumeName
	}
}

func getRedisCommand(rf *redisfailoverv1.RedisFailover) []string {
	if len(rf.Spec.Redis.Command) > 0 {
		return rf.Spec.Redis.Command
	}
	// A custom redis.command gets no password arguments. With auth, the
	// command gives requirepass and masterauth to redis-server from the
	// REDIS_PASSWORD env var, a SecretKeyRef. Thus the password is not in a
	// ConfigMap or in the pod spec. The shell expands the env var, and exec
	// keeps redis-server as PID 1, so it gets SIGTERM.
	if rf.Spec.Auth.SecretPath != "" {
		return []string{
			"sh", "-c",
			fmt.Sprintf(`exec redis-server /redis/%s --requirepass "$REDIS_PASSWORD" --masterauth "$REDIS_PASSWORD"`, redisConfigFileName),
		}
	}
	return []string{
		"redis-server",
		fmt.Sprintf("/redis/%s", redisConfigFileName),
	}
}

func getTerminationGracePeriodSeconds(rf *redisfailoverv1.RedisFailover) int64 {
	if rf.Spec.Redis.TerminationGracePeriodSeconds > 0 {
		return rf.Spec.Redis.TerminationGracePeriodSeconds
	}
	return 30
}

func getExtraContainersWithRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.Container {
	env := getRedisEnv(rf)
	extraContainers := getContainersWithRedisEnv(rf.Spec.Redis.ExtraContainers, env)

	return extraContainers
}

func getInitContainersWithRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.Container {
	env := getRedisEnv(rf)
	initContainers := getContainersWithRedisEnv(rf.Spec.Redis.InitContainers, env)

	return initContainers
}

func getContainersWithRedisEnv(cs []corev1.Container, e []corev1.EnvVar) []corev1.Container {
	var containers []corev1.Container
	for _, c := range cs {
		c.Env = append(c.Env, e...)
		containers = append(containers, c)
	}

	return containers
}

func getRedisEnv(rf *redisfailoverv1.RedisFailover) []corev1.EnvVar {
	var env []corev1.EnvVar

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_ADDR",
		Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_PORT",
		Value: fmt.Sprintf("%[1]v", rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_USER",
		Value: "default",
	})

	if rf.Spec.Auth.SecretPath != "" {
		env = append(env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rf.Spec.Auth.SecretPath,
					},
					Key: "password",
				},
			},
		})
	}

	return env
}
