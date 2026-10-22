package service

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// The instance manager is opt-in. Set spec.redis.instanceManagerImage to the
// operator image and each Redis pod gets an init container that copies the
// redis-instance binary out of it onto an emptyDir volume. The Redis
// container then runs that copy as PID 1, with redis-server as its child.
//
// The manager removes the old RDB tempfiles before Redis starts, so the pod
// needs no rdb-tempfile-cleanup init container, and it serves the health
// endpoints that the probes use, so the probes start no process in the Redis
// container. On a node that is short of memory an exec probe can fail to fork
// while Redis is healthy; an HTTP probe cannot.
//
// Without the field nothing below runs and the pods are unchanged.
const (
	instanceManagerVolumeName  = "instance-manager"
	instanceManagerMountPath   = "/controller"
	instanceManagerBinaryName  = "redis-instance"
	instanceManagerHealthPort  = 8080
	instanceManagerHealthzPath = "/healthz"
	instanceManagerReadyzPath  = "/readyz"
)

// instanceManagerEnabled reports whether the Redis pods run the manager. The
// image is the switch: the init container has to pull the binary from
// somewhere, and the operator cannot name its own image unless it is told.
func instanceManagerEnabled(rf *redisfailoverv1.RedisFailover) bool {
	return rf.Spec.Redis.InstanceManagerImage != ""
}

// instanceManagerPort is the container port of the health server.
func instanceManagerPort() corev1.ContainerPort {
	return corev1.ContainerPort{
		Name:          "health",
		ContainerPort: instanceManagerHealthPort,
		Protocol:      corev1.ProtocolTCP,
	}
}

// instanceManagerLivenessProbe asks the health server whether Redis answers.
func instanceManagerLivenessProbe() *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		FailureThreshold:    6,
		PeriodSeconds:       15,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: instanceManagerHealthzPath,
				Port: intstr.FromInt32(instanceManagerHealthPort),
			},
		},
	}
}

// instanceManagerReadinessProbe asks the health server whether Redis can take
// the traffic: it does not load its dataset, it does not sync, and a replica
// has its link to the master.
func instanceManagerReadinessProbe() *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		PeriodSeconds:       10,
		FailureThreshold:    3,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: instanceManagerReadyzPath,
				Port: intstr.FromInt32(instanceManagerHealthPort),
			},
		},
	}
}

// instanceManagerCommand starts the manager. The password is in no ConfigMap
// and in no pod command: the manager reads REDIS_PASSWORD from its
// environment and gives requirepass and masterauth to redis-server.
func instanceManagerCommand() []string {
	return []string{
		fmt.Sprintf("%s/%s", instanceManagerMountPath, instanceManagerBinaryName),
		"run",
		"--redis-conf", fmt.Sprintf("/redis/%s", redisConfigFileName),
	}
}

// instanceManagerVolume is the emptyDir volume that holds the binary between
// the init container and the Redis container.
func instanceManagerVolume() corev1.Volume {
	return corev1.Volume{
		Name: instanceManagerVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

// instanceManagerVolumeMount mounts that volume in a container.
func instanceManagerVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      instanceManagerVolumeName,
		MountPath: instanceManagerMountPath,
	}
}

// createInstanceManagerInitContainer puts the redis-instance binary on the
// shared volume. The binary copies itself, with its own bootstrap subcommand,
// so the image needs no shell and no cp and can stay distroless.
func createInstanceManagerInitContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	return corev1.Container{
		Name:            "instance-manager-init",
		Image:           rf.Spec.Redis.InstanceManagerImage,
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.ImagePullPolicy),
		// The same context as the rdb-tempfile-cleanup container that this one
		// replaces. The copy goes to the mounted volume and not to the root
		// filesystem, so readOnlyRootFilesystem holds.
		SecurityContext: getContainerSecurityContext(rf.Spec.Redis.ContainerSecurityContext),
		Command: []string{
			fmt.Sprintf("/usr/local/bin/%s", instanceManagerBinaryName),
			"bootstrap",
			fmt.Sprintf("%s/%s", instanceManagerMountPath, instanceManagerBinaryName),
		},
		VolumeMounts: []corev1.VolumeMount{instanceManagerVolumeMount()},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
}
