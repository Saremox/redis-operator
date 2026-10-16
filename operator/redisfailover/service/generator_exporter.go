package service

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

var exporterDefaultResourceRequirements = corev1.ResourceRequirements{
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(exporterDefaultLimitCPU),
		corev1.ResourceMemory: resource.MustParse(exporterDefaultLimitMemory),
	},
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(exporterDefaultRequestCPU),
		corev1.ResourceMemory: resource.MustParse(exporterDefaultRequestMemory),
	},
}

// redisExporterListenPort returns the port the redis exporter listens on,
// falling back to the built-in default when the spec leaves it unset.
func redisExporterListenPort(rf *redisfailoverv1.RedisFailover) int32 {
	if p := rf.Spec.Redis.Exporter.Port; p != 0 {
		return p
	}
	return exporterPort
}

// sentinelExporterListenPort returns the port the sentinel exporter listens on,
// falling back to the built-in default when the spec leaves it unset.
func sentinelExporterListenPort(rf *redisfailoverv1.RedisFailover) int32 {
	if p := rf.Spec.Sentinel.Exporter.Port; p != 0 {
		return p
	}
	return sentinelExporterPort
}

func createRedisExporterContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	resources := exporterDefaultResourceRequirements
	if rf.Spec.Redis.Exporter.Resources != nil {
		resources = *rf.Spec.Redis.Exporter.Resources
	}
	container := corev1.Container{
		Name:            exporterContainerName,
		Image:           rf.Spec.Redis.Exporter.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Redis.Exporter.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Redis.Exporter.ContainerSecurityContext),
		Args:            rf.Spec.Redis.Exporter.Args,
		Env: append(rf.Spec.Redis.Exporter.Env, corev1.EnvVar{
			Name: "REDIS_ALIAS",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		),
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: redisExporterListenPort(rf),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: resources,
	}

	redisEnv := getRedisExporterEnv(rf)
	container.Env = append(container.Env, redisEnv...)
	// Only for a custom port, so default pod templates stay unchanged.
	if rf.Spec.Redis.Exporter.Port != 0 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "REDIS_EXPORTER_WEB_LISTEN_ADDRESS",
			Value: fmt.Sprintf("0.0.0.0:%d", rf.Spec.Redis.Exporter.Port),
		})
	}

	return container
}

func createSentinelExporterContainer(rf *redisfailoverv1.RedisFailover) corev1.Container {
	resources := exporterDefaultResourceRequirements
	if rf.Spec.Sentinel.Exporter.Resources != nil {
		resources = *rf.Spec.Sentinel.Exporter.Resources
	}
	listenPort := sentinelExporterListenPort(rf)
	container := corev1.Container{
		Name:            sentinelExporterContainerName,
		Image:           rf.Spec.Sentinel.Exporter.Image,
		ImagePullPolicy: pullPolicy(rf.Spec.Sentinel.Exporter.ImagePullPolicy),
		SecurityContext: getContainerSecurityContext(rf.Spec.Sentinel.Exporter.ContainerSecurityContext),
		Args:            rf.Spec.Sentinel.Exporter.Args,
		Env: append(rf.Spec.Sentinel.Exporter.Env, corev1.EnvVar{
			Name: "REDIS_ALIAS",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		}, corev1.EnvVar{
			Name:  "REDIS_EXPORTER_WEB_LISTEN_ADDRESS",
			Value: fmt.Sprintf("0.0.0.0:%[1]v", listenPort),
		}, corev1.EnvVar{
			Name:  "REDIS_ADDR",
			Value: "redis://127.0.0.1:26379",
		},
		),
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: listenPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: resources,
	}

	return container
}

func getRedisExporterEnv(rf *redisfailoverv1.RedisFailover) []corev1.EnvVar {
	var env []corev1.EnvVar

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_ADDR",
		Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", rf.Spec.Redis.Port),
	})

	env = append(env, corev1.EnvVar{
		Name:  "REDIS_PORT",
		Value: fmt.Sprintf("%[1]v", rf.Spec.Redis.Port),
	})

	if !envExists(rf.Spec.Redis.Exporter.Env, "REDIS_USER") {
		env = append(env, corev1.EnvVar{
			Name:  "REDIS_USER",
			Value: "default",
		})
	}

	if !envExists(rf.Spec.Redis.Exporter.Env, "REDIS_PASSWORD") {
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
	}
	return env
}

func envExists(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}
