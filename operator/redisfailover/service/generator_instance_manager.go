package service

import (
	"fmt"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// The instance manager is opt-in. Set spec.redis.instanceManager.enabled and
// each Redis pod gets an init container that copies the redis-instance
// binary out of the operator image onto an emptyDir volume. The Redis
// container then runs that copy as PID 1, with redis-server as its child.
//
// The manager serves the health endpoints that the probes use, so the probes
// start no process in the Redis container. On a node that is short of memory
// an exec probe can fail to fork while Redis is healthy; an HTTP probe
// cannot.
//
// Without the field nothing below runs and the pods are unchanged.
const (
	instanceManagerVolumeName  = "instance-manager"
	instanceManagerMountPath   = "/controller"
	instanceManagerBinaryName  = "redis-instance"
	instanceManagerHealthzPath = "/healthz"
	instanceManagerReadyzPath  = "/readyz"
	// operatorImageVar names the image that holds the redis-instance binary.
	// The operator Deployment carries it, as OPERATOR_IMAGE_NAME does for
	// CloudNativePG. The operator writes the flags of the binary, so the
	// operator must also choose the build that reads them.
	operatorImageVar = "OPERATOR_IMAGE_NAME"
)

// instanceManagerEnabled reports whether the Redis pods run the manager.
func instanceManagerEnabled(rf *redisfailoverv1.RedisFailover) bool {
	return rf.Spec.Redis.InstanceManager.Enabled
}

// operatorImage returns the image of the operator, or an empty string when
// the operator Deployment does not name it.
func operatorImage() string {
	return os.Getenv(operatorImageVar)
}

// checkInstanceManagerImage reports that the operator cannot name its own
// image. The init container has to copy the binary out of an image, and only
// the operator Deployment knows which image holds it.
func checkInstanceManagerImage(rf *redisfailoverv1.RedisFailover) error {
	if instanceManagerEnabled(rf) && operatorImage() == "" {
		return fmt.Errorf("redis.instanceManager.enabled needs the %s variable on the operator Deployment", operatorImageVar)
	}
	return nil
}

// instanceManagerHealthPort is the port of the health server.
func instanceManagerHealthPort(rf *redisfailoverv1.RedisFailover) int32 {
	if p := rf.Spec.Redis.InstanceManager.Port; p != 0 {
		return p
	}
	return redisfailoverv1.DefaultInstanceManagerPort
}

// instanceManagerPort is the container port of the health server.
func instanceManagerPort(rf *redisfailoverv1.RedisFailover) corev1.ContainerPort {
	return corev1.ContainerPort{
		Name:          "health",
		ContainerPort: instanceManagerHealthPort(rf),
		Protocol:      corev1.ProtocolTCP,
	}
}

// instanceManagerLivenessProbe asks the health server whether Redis answers.
func instanceManagerLivenessProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		FailureThreshold:    6,
		PeriodSeconds:       15,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: instanceManagerHealthzPath,
				Port: intstr.FromInt32(instanceManagerHealthPort(rf)),
			},
		},
	}
}

// instanceManagerReadinessProbe asks the health server whether Redis can take
// the traffic: it does not load its dataset, it does not sync, and a replica
// has its link to the master.
func instanceManagerReadinessProbe(rf *redisfailoverv1.RedisFailover) *corev1.Probe {
	return &corev1.Probe{
		InitialDelaySeconds: graceTime,
		TimeoutSeconds:      5,
		PeriodSeconds:       10,
		FailureThreshold:    3,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: instanceManagerReadyzPath,
				Port: intstr.FromInt32(instanceManagerHealthPort(rf)),
			},
		},
	}
}

// instanceManagerCommand starts the manager. The password is in no ConfigMap
// and in no pod command: the manager reads REDIS_PASSWORD from its
// environment and gives requirepass and masterauth to redis-server.
//
// The flags carry what ready.sh and the probes of the pod would otherwise
// hold: the Redis port, the port of the health server, and the window in
// which a replica stays ready after it lost its master.
//
// The manager gets no shutdown timeout, so it sends no SIGKILL of its own.
// The kubelet ends the pod at terminationGracePeriodSeconds, and a stop-time
// save then keeps its whole window.
func instanceManagerCommand(rf *redisfailoverv1.RedisFailover) []string {
	return []string{
		fmt.Sprintf("%s/%s", instanceManagerMountPath, instanceManagerBinaryName),
		"run",
		"--redis-conf", fmt.Sprintf("/redis/%s", redisConfigFileName),
		"--redis-port", strconv.Itoa(int(rf.Spec.Redis.Port)),
		"--health-port", strconv.Itoa(int(instanceManagerHealthPort(rf))),
		"--max-link-down", fmt.Sprintf("%ds", redisReadinessMaxLinkDownSeconds(rf)),
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
		Image:           operatorImage(),
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.ImagePullPolicy),
		// The same context as the rdb-tempfile-cleanup container beside it.
		// The copy goes to the mounted volume and not to the root
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
