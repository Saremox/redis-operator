package service_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestRedisStatefulSetStorageGeneration(t *testing.T) {
	configMapName := rfservice.GetRedisName(generateRF())
	shutdownConfigMapName := rfservice.GetRedisShutdownConfigMapName(generateRF())
	readinesConfigMapName := rfservice.GetRedisReadinessName(generateRF())
	executeMode := int32(0744)
	tests := []struct {
		name           string
		ownerRefs      []metav1.OwnerReference
		expectedSS     appsv1.StatefulSet
		rfRedisStorage redisfailoverv1.RedisStorage
	}{
		{
			name: "Default values",
			expectedSS: appsv1.StatefulSet{
				Spec: appsv1.StatefulSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "redis-config",
											MountPath: "/redis",
										},
										{
											Name:      "redis-shutdown-config",
											MountPath: "/redis-shutdown",
										},
										{
											Name:      "redis-readiness-config",
											MountPath: "/redis-readiness",
										},
										{
											Name:      "redis-data",
											MountPath: "/data",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "redis-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: configMapName,
											},
										},
									},
								},
								{
									Name: "redis-shutdown-config",
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
									Name: "redis-readiness-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: readinesConfigMapName,
											},
											DefaultMode: &executeMode,
										},
									},
								},
								{
									Name: "redis-data",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{},
									},
								},
							},
						},
					},
				},
			},
			rfRedisStorage: redisfailoverv1.RedisStorage{},
		},
		{
			name: "Defined an emptydir with storage on memory",
			expectedSS: appsv1.StatefulSet{
				Spec: appsv1.StatefulSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "redis-config",
											MountPath: "/redis",
										},
										{
											Name:      "redis-shutdown-config",
											MountPath: "/redis-shutdown",
										},
										{
											Name:      "redis-readiness-config",
											MountPath: "/redis-readiness",
										},
										{
											Name:      "redis-data",
											MountPath: "/data",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "redis-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: configMapName,
											},
										},
									},
								},
								{
									Name: "redis-shutdown-config",
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
									Name: "redis-readiness-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: readinesConfigMapName,
											},
											DefaultMode: &executeMode,
										},
									},
								},
								{
									Name: "redis-data",
									VolumeSource: corev1.VolumeSource{
										EmptyDir: &corev1.EmptyDirVolumeSource{
											Medium: corev1.StorageMediumMemory,
										},
									},
								},
							},
						},
					},
				},
			},
			rfRedisStorage: redisfailoverv1.RedisStorage{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory,
				},
			},
		},
		{
			name: "Defined an persistentvolumeclaim",
			expectedSS: appsv1.StatefulSet{
				Spec: appsv1.StatefulSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "redis-config",
											MountPath: "/redis",
										},
										{
											Name:      "redis-shutdown-config",
											MountPath: "/redis-shutdown",
										},
										{
											Name:      "redis-readiness-config",
											MountPath: "/redis-readiness",
										},
										{
											Name:      "pvc-data",
											MountPath: "/data",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "redis-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: configMapName,
											},
										},
									},
								},
								{
									Name: "redis-shutdown-config",
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
									Name: "redis-readiness-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: readinesConfigMapName,
											},
											DefaultMode: &executeMode,
										},
									},
								},
							},
						},
					},
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{
							TypeMeta: metav1.TypeMeta{
								Kind:       "PersistentVolumeClaim",
								APIVersion: "v1",
							},
							ObjectMeta: metav1.ObjectMeta{
								Name: "pvc-data",
							},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{
									"ReadWriteOnce",
								},
								Resources: corev1.VolumeResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							},
						},
					},
				},
			},
			rfRedisStorage: redisfailoverv1.RedisStorage{
				PersistentVolumeClaim: &redisfailoverv1.EmbeddedPersistentVolumeClaim{
					EmbeddedObjectMetadata: redisfailoverv1.EmbeddedObjectMetadata{
						Name: "pvc-data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							"ReadWriteOnce",
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
		{
			name: "Defined an persistentvolumeclaim with ownerRefs",
			ownerRefs: []metav1.OwnerReference{
				{
					Name: "testing",
				},
			},
			expectedSS: appsv1.StatefulSet{
				Spec: appsv1.StatefulSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "redis-config",
											MountPath: "/redis",
										},
										{
											Name:      "redis-shutdown-config",
											MountPath: "/redis-shutdown",
										},
										{
											Name:      "redis-readiness-config",
											MountPath: "/redis-readiness",
										},
										{
											Name:      "pvc-data",
											MountPath: "/data",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "redis-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: configMapName,
											},
										},
									},
								},
								{
									Name: "redis-shutdown-config",
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
									Name: "redis-readiness-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: readinesConfigMapName,
											},
											DefaultMode: &executeMode,
										},
									},
								},
							},
						},
					},
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{
							TypeMeta: metav1.TypeMeta{
								Kind:       "PersistentVolumeClaim",
								APIVersion: "v1",
							},
							ObjectMeta: metav1.ObjectMeta{
								Name: "pvc-data",
								OwnerReferences: []metav1.OwnerReference{
									{
										Name: "testing",
									},
								},
							},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{
									"ReadWriteOnce",
								},
								Resources: corev1.VolumeResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							},
						},
					},
				},
			},
			rfRedisStorage: redisfailoverv1.RedisStorage{
				PersistentVolumeClaim: &redisfailoverv1.EmbeddedPersistentVolumeClaim{
					EmbeddedObjectMetadata: redisfailoverv1.EmbeddedObjectMetadata{
						Name: "pvc-data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							"ReadWriteOnce",
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
		{
			name: "Defined an persistentvolumeclaim with ownerRefs keeping the pvc",
			ownerRefs: []metav1.OwnerReference{
				{
					Name: "testing",
				},
			},
			expectedSS: appsv1.StatefulSet{
				Spec: appsv1.StatefulSetSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "redis-config",
											MountPath: "/redis",
										},
										{
											Name:      "redis-shutdown-config",
											MountPath: "/redis-shutdown",
										},
										{
											Name:      "redis-readiness-config",
											MountPath: "/redis-readiness",
										},
										{
											Name:      "pvc-data",
											MountPath: "/data",
										},
									},
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "redis-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: configMapName,
											},
										},
									},
								},
								{
									Name: "redis-shutdown-config",
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
									Name: "redis-readiness-config",
									VolumeSource: corev1.VolumeSource{
										ConfigMap: &corev1.ConfigMapVolumeSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: readinesConfigMapName,
											},
											DefaultMode: &executeMode,
										},
									},
								},
							},
						},
					},
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{
							TypeMeta: metav1.TypeMeta{
								Kind:       "PersistentVolumeClaim",
								APIVersion: "v1",
							},
							ObjectMeta: metav1.ObjectMeta{
								Name: "pvc-data",
							},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{
									"ReadWriteOnce",
								},
								Resources: corev1.VolumeResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							},
						},
					},
				},
			},
			rfRedisStorage: redisfailoverv1.RedisStorage{
				KeepAfterDeletion: true,
				PersistentVolumeClaim: &redisfailoverv1.EmbeddedPersistentVolumeClaim{
					EmbeddedObjectMetadata: redisfailoverv1.EmbeddedObjectMetadata{
						Name: "pvc-data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							"ReadWriteOnce",
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		// Generate a default RedisFailover and attaching the required storage
		rf := generateRF()
		rf.Spec.Redis.Storage = test.rfRedisStorage

		generatedStatefulSet := appsv1.StatefulSet{}

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			generatedStatefulSet = *ss
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, test.ownerRefs)

		// Check that the storage-related fields are as expected
		assert.Equal(test.expectedSS.Spec.Template.Spec.Volumes, generatedStatefulSet.Spec.Template.Spec.Volumes)
		assert.Equal(test.expectedSS.Spec.Template.Spec.Containers[0].VolumeMounts, generatedStatefulSet.Spec.Template.Spec.Containers[0].VolumeMounts)
		assert.Equal(test.expectedSS.Spec.VolumeClaimTemplates, generatedStatefulSet.Spec.VolumeClaimTemplates)
		assert.NoError(err)
	}
}

func TestRedisStatefulSetCommands(t *testing.T) {
	tests := []struct {
		name             string
		givenCommands    []string
		expectedCommands []string
	}{
		{
			name:          "Default values",
			givenCommands: []string{},
			expectedCommands: []string{
				"redis-server",
				"/redis/redis.conf",
			},
		},
		{
			name: "Given commands should be used in redis container",
			givenCommands: []string{
				"test",
				"command",
			},
			expectedCommands: []string{
				"test",
				"command",
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		// Generate a default RedisFailover and attaching the required storage
		rf := generateRF()
		rf.Spec.Redis.Command = test.givenCommands

		gotCommands := []string{}

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			gotCommands = ss.Spec.Template.Spec.Containers[0].Command
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.Equal(test.expectedCommands, gotCommands)
		assert.NoError(err)
	}
}

func TestSentinelDeploymentCommands(t *testing.T) {
	tests := []struct {
		name             string
		givenCommands    []string
		expectedCommands []string
	}{
		{
			name:          "Default values",
			givenCommands: []string{},
			expectedCommands: []string{
				"redis-server",
				"/redis/sentinel.conf",
				"--sentinel",
			},
		},
		{
			name: "Given commands should be used in sentinel container",
			givenCommands: []string{
				"test",
				"command",
			},
			expectedCommands: []string{
				"test",
				"command",
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		// Generate a default RedisFailover and attaching the required storage
		rf := generateRF()
		rf.Spec.Sentinel.Command = test.givenCommands

		gotCommands := []string{}

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			gotCommands = d.Spec.Template.Spec.Containers[0].Command
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.Equal(test.expectedCommands, gotCommands)
		assert.NoError(err)
	}
}

func TestRedisStatefulSetPodAnnotations(t *testing.T) {
	tests := []struct {
		name                   string
		givenPodAnnotations    map[string]string
		expectedPodAnnotations map[string]string
	}{
		{
			name:                   "PodAnnotations was not defined",
			givenPodAnnotations:    nil,
			expectedPodAnnotations: nil,
		},
		{
			name: "PodAnnotations is defined",
			givenPodAnnotations: map[string]string{
				"some":               "annotation",
				"path/to/annotation": "here",
			},
			expectedPodAnnotations: map[string]string{
				"some":               "annotation",
				"path/to/annotation": "here",
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		// Generate a default RedisFailover and attaching the required annotations
		rf := generateRF()
		rf.Spec.Redis.PodAnnotations = test.givenPodAnnotations

		gotPodAnnotations := map[string]string{}

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			gotPodAnnotations = ss.Spec.Template.ObjectMeta.Annotations
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		// The Redis StatefulSet's pod template always carries an auth-secret
		// checksum annotation (see redisAuthSecretChecksumAnnotation) so that
		// pods roll when the referenced Secret's password value changes.
		// Assert its presence/format separately, then compare the rest.
		assert.Contains(gotPodAnnotations, "redisfailovers.databases.spotahome.com/secret-checksum")
		assert.Len(gotPodAnnotations["redisfailovers.databases.spotahome.com/secret-checksum"], 64)
		delete(gotPodAnnotations, "redisfailovers.databases.spotahome.com/secret-checksum")
		if len(gotPodAnnotations) == 0 {
			gotPodAnnotations = nil
		}

		assert.Equal(test.expectedPodAnnotations, gotPodAnnotations)
		assert.NoError(err)
	}
}

// TestRedisStatefulSetAuthSecretChecksumChangesWithPassword is a regression test
// for spotahome/redis-operator#658: rotating the auth Secret's password value
// (same Secret name, new data) must change the Redis pod template's checksum
// annotation, so the operator's existing revision-based rolling update logic
// actually replaces the running pods instead of leaving them stuck on the old
// password while the operator itself starts using the new one.
func TestRedisStatefulSetAuthSecretChecksumChangesWithPassword(t *testing.T) {
	assert := assert.New(t)

	generateSS := func(password string) map[string]string {
		rf := generateRF()
		rf.Spec.Auth.SecretPath = "redis-secret"

		var gotAnnotations map[string]string
		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("GetSecret", namespace, "redis-secret").Once().Return(&corev1.Secret{
			Data: map[string][]byte{"password": []byte(password)},
		}, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			gotAnnotations = ss.Spec.Template.ObjectMeta.Annotations
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})
		assert.NoError(err)

		return gotAnnotations
	}

	const checksumKey = "redisfailovers.databases.spotahome.com/secret-checksum"

	annotationsV1 := generateSS("password-v1")
	annotationsV1Again := generateSS("password-v1")
	annotationsV2 := generateSS("password-v2")

	assert.NotEmpty(annotationsV1[checksumKey])
	assert.Equal(annotationsV1[checksumKey], annotationsV1Again[checksumKey], "same password must produce the same checksum")
	assert.NotEqual(annotationsV1[checksumKey], annotationsV2[checksumKey], "a rotated password must produce a different checksum, so the pod template (and its revision hash) changes")
}

func TestSentinelDeploymentPodAnnotations(t *testing.T) {
	tests := []struct {
		name                   string
		givenPodAnnotations    map[string]string
		expectedPodAnnotations map[string]string
	}{
		{
			name:                   "PodAnnotations was not defined",
			givenPodAnnotations:    nil,
			expectedPodAnnotations: nil,
		},
		{
			name: "PodAnnotations is defined",
			givenPodAnnotations: map[string]string{
				"some":               "annotation",
				"path/to/annotation": "here",
			},
			expectedPodAnnotations: map[string]string{
				"some":               "annotation",
				"path/to/annotation": "here",
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		// Generate a default RedisFailover and attaching the required annotations
		rf := generateRF()
		rf.Spec.Sentinel.PodAnnotations = test.givenPodAnnotations

		gotPodAnnotations := map[string]string{}

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			gotPodAnnotations = d.Spec.Template.ObjectMeta.Annotations
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.Equal(test.expectedPodAnnotations, gotPodAnnotations)
		assert.NoError(err)
	}
}

func TestRedisStatefulSetServiceAccountName(t *testing.T) {
	tests := []struct {
		name                       string
		givenServiceAccountName    string
		expectedServiceAccountName string
	}{
		{
			name:                       "ServiceAccountName was not defined",
			givenServiceAccountName:    "",
			expectedServiceAccountName: "",
		},
		{
			name:                       "ServiceAccountName is defined",
			givenServiceAccountName:    "redis-sa",
			expectedServiceAccountName: "redis-sa",
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		// Generate a default RedisFailover and attaching the required Service Account
		rf := generateRF()
		rf.Spec.Redis.ServiceAccountName = test.givenServiceAccountName

		gotServiceAccountName := ""

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			gotServiceAccountName = ss.Spec.Template.Spec.ServiceAccountName
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.Equal(test.expectedServiceAccountName, gotServiceAccountName)
		assert.NoError(err)
	}
}

func TestSentinelDeploymentServiceAccountName(t *testing.T) {
	tests := []struct {
		name                        string
		givenServiceAccountName     string
		expectServiceAccountCreated bool
	}{
		{
			name:                        "ServiceAccountName was not defined: the operator auto-provisions one",
			givenServiceAccountName:     "",
			expectServiceAccountCreated: true,
		},
		{
			name:                        "ServiceAccountName is defined: the user's own ServiceAccount is used as-is",
			givenServiceAccountName:     "sentinel-sa",
			expectServiceAccountCreated: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Generate a default RedisFailover and attaching the required Service Account
			rf := generateRF()
			rf.Spec.Sentinel.ServiceAccountName = test.givenServiceAccountName

			expectedServiceAccountName := test.givenServiceAccountName
			if expectedServiceAccountName == "" {
				expectedServiceAccountName = rfservice.GetSentinelServiceAccountName(rf)
			}

			gotServiceAccountName := ""

			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			if test.expectServiceAccountCreated {
				ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
			}
			ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				d := args.Get(1).(*appsv1.Deployment)
				gotServiceAccountName = d.Spec.Template.Spec.ServiceAccountName
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

			assert.Equal(expectedServiceAccountName, gotServiceAccountName)
			assert.NoError(err)

			if test.expectServiceAccountCreated {
				ms.AssertCalled(t, "CreateOrUpdateServiceAccount", namespace, mock.Anything)
			} else {
				ms.AssertNotCalled(t, "CreateOrUpdateServiceAccount", mock.Anything, mock.Anything)
			}
		})
	}
}

func TestSentinelService(t *testing.T) {
	tests := []struct {
		name            string
		rfName          string
		rfNamespace     string
		rfLabels        map[string]string
		rfAnnotations   map[string]string
		expectedService corev1.Service
	}{
		{
			name: "with defaults",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sentinelName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "sentinel",
							Port:       26379,
							TargetPort: intstr.FromInt(26379),
							Protocol:   "TCP",
						},
					},
				},
			},
		},
		{
			name:   "with Name provided",
			rfName: "custom-name",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rfs-custom-name",
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "sentinel",
							Port:       26379,
							TargetPort: intstr.FromInt(26379),
							Protocol:   "TCP",
						},
					},
				},
			},
		},
		{
			name:        "with Namespace provided",
			rfNamespace: "custom-namespace",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sentinelName,
					Namespace: "custom-namespace",
					Labels: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "sentinel",
							Port:       26379,
							TargetPort: intstr.FromInt(26379),
							Protocol:   "TCP",
						},
					},
				},
			},
		},
		{
			name:     "with Labels provided",
			rfLabels: map[string]string{"some": "label"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sentinelName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"some":                        "label",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "sentinel",
							Port:       26379,
							TargetPort: intstr.FromInt(26379),
							Protocol:   "TCP",
						},
					},
				},
			},
		},
		{
			name:          "with Annotations provided",
			rfAnnotations: map[string]string{"some": "annotation"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sentinelName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Annotations: map[string]string{"some": "annotation"},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"app.kubernetes.io/component": "sentinel",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "sentinel",
							Port:       26379,
							TargetPort: intstr.FromInt(26379),
							Protocol:   "TCP",
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Generate a default RedisFailover and attaching the required annotations
			rf := generateRF()
			if test.rfName != "" {
				rf.Name = test.rfName
			}
			if test.rfNamespace != "" {
				rf.Namespace = test.rfNamespace
			}
			rf.Spec.Sentinel.ServiceAnnotations = test.rfAnnotations

			generatedService := corev1.Service{}

			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateService", rf.Namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				s := args.Get(1).(*corev1.Service)
				generatedService = *s
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureSentinelService(rf, test.rfLabels, []metav1.OwnerReference{{Name: "testing"}})

			assert.Equal(test.expectedService, generatedService)
			assert.NoError(err)
		})
	}
}

func TestRedisService(t *testing.T) {
	tests := []struct {
		name            string
		rfName          string
		rfNamespace     string
		rfLabels        map[string]string
		rfAnnotations   map[string]string
		expectedService corev1.Service
	}{
		{
			name: "with defaults",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      redisName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/path":   "/metrics",
						"prometheus.io/port":   "http",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: corev1.ClusterIPNone,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:     "http-metrics",
							Port:     9121,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
		},
		{
			name:   "with Name provided",
			rfName: "custom-name",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rfr-custom-name",
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/path":   "/metrics",
						"prometheus.io/port":   "http",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: corev1.ClusterIPNone,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:     "http-metrics",
							Port:     9121,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
		},
		{
			name:        "with Namespace provided",
			rfNamespace: "custom-namespace",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      redisName,
					Namespace: "custom-namespace",
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/path":   "/metrics",
						"prometheus.io/port":   "http",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: corev1.ClusterIPNone,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:     "http-metrics",
							Port:     9121,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
		},
		{
			name:     "with Labels provided",
			rfLabels: map[string]string{"some": "label"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      redisName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"some":                        "label",
					},
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/path":   "/metrics",
						"prometheus.io/port":   "http",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: corev1.ClusterIPNone,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:     "http-metrics",
							Port:     9121,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
		},
		{
			name:          "with Annotations provided",
			rfAnnotations: map[string]string{"some": "annotation"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      redisName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Annotations: map[string]string{
						"prometheus.io/scrape": "true",
						"prometheus.io/path":   "/metrics",
						"prometheus.io/port":   "http",
						"some":                 "annotation",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type:      corev1.ServiceTypeClusterIP,
					ClusterIP: corev1.ClusterIPNone,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
					},
					Ports: []corev1.ServicePort{
						{
							Name:     "http-metrics",
							Port:     9121,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Generate a default RedisFailover and attaching the required annotations
			rf := generateRF()
			if test.rfName != "" {
				rf.Name = test.rfName
			}
			if test.rfNamespace != "" {
				rf.Namespace = test.rfNamespace
			}
			rf.Spec.Redis.ServiceAnnotations = test.rfAnnotations

			generatedService := corev1.Service{}

			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateService", rf.Namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				s := args.Get(1).(*corev1.Service)
				generatedService = *s
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisService(rf, test.rfLabels, []metav1.OwnerReference{{Name: "testing"}})

			assert.Equal(test.expectedService, generatedService)
			assert.NoError(err)
		})
	}
}

func TestRedisMasterService(t *testing.T) {
	tests := []struct {
		name            string
		rfName          string
		rfNamespace     string
		rfLabels        map[string]string
		rfAnnotations   map[string]string
		expectedService corev1.Service
	}{
		{
			name: "with defaults",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      masterName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:   "with Name provided",
			rfName: "custom-name",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rfrm-custom-name",
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:        "with Namespace provided",
			rfNamespace: "custom-namespace",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      masterName,
					Namespace: "custom-namespace",
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:     "with Labels provided",
			rfLabels: map[string]string{"some": "label"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      masterName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
						"some":                        "label",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:          "with Annotations provided",
			rfAnnotations: map[string]string{"some": "annotation"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      masterName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Annotations: map[string]string{
						"some": "annotation",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "master",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Generate a default RedisFailover and attaching the required annotations
			rf := generateRF()
			if test.rfName != "" {
				rf.Name = test.rfName
			}
			if test.rfNamespace != "" {
				rf.Namespace = test.rfNamespace
			}
			rf.Spec.Redis.Port = 6379
			rf.Spec.Redis.ServiceAnnotations = test.rfAnnotations

			generatedMasterService := corev1.Service{}

			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateService", rf.Namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				s := args.Get(1).(*corev1.Service)
				generatedMasterService = *s
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisMasterService(rf, test.rfLabels, []metav1.OwnerReference{{Name: "testing"}})

			assert.Equal(test.expectedService, generatedMasterService)
			assert.NoError(err)
		})
	}
}

func TestRedisSlaveService(t *testing.T) {
	tests := []struct {
		name            string
		rfName          string
		rfNamespace     string
		rfLabels        map[string]string
		rfAnnotations   map[string]string
		expectedService corev1.Service
	}{
		{
			name: "with defaults",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      slaveName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:   "with Name provided",
			rfName: "custom-name",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rfrs-custom-name",
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      "custom-name",
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:        "with Namespace provided",
			rfNamespace: "custom-namespace",
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      slaveName,
					Namespace: "custom-namespace",
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:     "with Labels provided",
			rfLabels: map[string]string{"some": "label"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      slaveName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
						"some":                        "label",
					},
					Annotations: nil,
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
		{
			name:          "with Annotations provided",
			rfAnnotations: map[string]string{"some": "annotation"},
			expectedService: corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      slaveName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Annotations: map[string]string{
						"some": "annotation",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							Name: "testing",
						},
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Selector: map[string]string{
						"app.kubernetes.io/component": "redis",
						"app.kubernetes.io/name":      name,
						"app.kubernetes.io/part-of":   "redis-failover",
						"redisfailovers-role":         "slave",
					},
					Ports: []corev1.ServicePort{
						{
							Name:       "redis",
							Port:       6379,
							Protocol:   corev1.ProtocolTCP,
							TargetPort: intstr.FromString("redis"),
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			// Generate a default RedisFailover and attaching the required annotations
			rf := generateRF()
			if test.rfName != "" {
				rf.Name = test.rfName
			}
			if test.rfNamespace != "" {
				rf.Namespace = test.rfNamespace
			}
			rf.Spec.Redis.Port = 6379
			rf.Spec.Redis.ServiceAnnotations = test.rfAnnotations

			generatedSlaveService := corev1.Service{}

			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateService", rf.Namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				s := args.Get(1).(*corev1.Service)
				generatedSlaveService = *s
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisSlaveService(rf, test.rfLabels, []metav1.OwnerReference{{Name: "testing"}})

			assert.Equal(test.expectedService, generatedSlaveService)
			assert.NoError(err)
		})
	}
}

func TestRedisHostNetworkAndDnsPolicy(t *testing.T) {
	tests := []struct {
		name                string
		hostNetwork         bool
		expectedHostNetwork bool
		dnsPolicy           corev1.DNSPolicy
		expectedDnsPolicy   corev1.DNSPolicy
	}{
		{
			name:                "Default",
			expectedHostNetwork: false,
			expectedDnsPolicy:   corev1.DNSClusterFirst,
		},
		{
			name:                "Custom",
			hostNetwork:         true,
			expectedHostNetwork: true,
			dnsPolicy:           corev1.DNSClusterFirstWithHostNet,
			expectedDnsPolicy:   corev1.DNSClusterFirstWithHostNet,
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		rf := generateRF()
		rf.Spec.Redis.HostNetwork = test.hostNetwork
		rf.Spec.Redis.DNSPolicy = test.dnsPolicy

		var actualHostNetwork bool
		var actualDnsPolicy corev1.DNSPolicy

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			actualHostNetwork = ss.Spec.Template.Spec.HostNetwork
			actualDnsPolicy = ss.Spec.Template.Spec.DNSPolicy
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})
		assert.NoError(err)

		assert.Equal(test.expectedHostNetwork, actualHostNetwork)
		assert.Equal(test.expectedDnsPolicy, actualDnsPolicy)
	}
}

func TestSentinelHostNetworkAndDnsPolicy(t *testing.T) {
	tests := []struct {
		name                string
		hostNetwork         bool
		expectedHostNetwork bool
		dnsPolicy           corev1.DNSPolicy
		expectedDnsPolicy   corev1.DNSPolicy
	}{
		{
			name:                "Default",
			expectedHostNetwork: false,
			expectedDnsPolicy:   corev1.DNSClusterFirst,
		},
		{
			name:                "Custom",
			hostNetwork:         true,
			expectedHostNetwork: true,
			dnsPolicy:           corev1.DNSClusterFirstWithHostNet,
			expectedDnsPolicy:   corev1.DNSClusterFirstWithHostNet,
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		rf := generateRF()
		rf.Spec.Sentinel.HostNetwork = test.hostNetwork
		rf.Spec.Sentinel.DNSPolicy = test.dnsPolicy

		var actualHostNetwork bool
		var actualDnsPolicy corev1.DNSPolicy

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			actualHostNetwork = d.Spec.Template.Spec.HostNetwork
			actualDnsPolicy = d.Spec.Template.Spec.DNSPolicy
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})
		assert.NoError(err)

		assert.Equal(test.expectedHostNetwork, actualHostNetwork)
		assert.Equal(test.expectedDnsPolicy, actualDnsPolicy)
	}
}

func TestRedisImagePullPolicy(t *testing.T) {
	tests := []struct {
		name                   string
		policy                 corev1.PullPolicy
		exporterPolicy         corev1.PullPolicy
		expectedPolicy         corev1.PullPolicy
		expectedExporterPolicy corev1.PullPolicy
	}{
		{
			name:                   "Default",
			expectedPolicy:         corev1.PullAlways,
			expectedExporterPolicy: corev1.PullAlways,
		},
		{
			name:                   "Custom",
			policy:                 corev1.PullIfNotPresent,
			exporterPolicy:         corev1.PullNever,
			expectedPolicy:         corev1.PullIfNotPresent,
			expectedExporterPolicy: corev1.PullNever,
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var policy corev1.PullPolicy
		var exporterPolicy corev1.PullPolicy

		rf := generateRF()
		rf.Spec.Redis.ImagePullPolicy = test.policy
		rf.Spec.Redis.Exporter.Enabled = true
		rf.Spec.Redis.Exporter.ImagePullPolicy = test.expectedExporterPolicy

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			policy = ss.Spec.Template.Spec.Containers[0].ImagePullPolicy
			exporterPolicy = ss.Spec.Template.Spec.Containers[1].ImagePullPolicy
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(string(test.expectedPolicy), string(policy))
		assert.Equal(string(test.expectedExporterPolicy), string(exporterPolicy))
	}
}

func TestSentinelImagePullPolicy(t *testing.T) {
	tests := []struct {
		name                 string
		policy               corev1.PullPolicy
		expectedPolicy       corev1.PullPolicy
		expectedConfigPolicy corev1.PullPolicy
	}{
		{
			name:                 "Default",
			expectedPolicy:       corev1.PullAlways,
			expectedConfigPolicy: corev1.PullAlways,
		},
		{
			name:                 "Custom",
			policy:               corev1.PullIfNotPresent,
			expectedPolicy:       corev1.PullIfNotPresent,
			expectedConfigPolicy: corev1.PullIfNotPresent,
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var policy corev1.PullPolicy
		var configPolicy corev1.PullPolicy

		rf := generateRF()
		rf.Spec.Sentinel.ImagePullPolicy = test.policy

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			policy = d.Spec.Template.Spec.Containers[0].ImagePullPolicy
			configPolicy = d.Spec.Template.Spec.InitContainers[0].ImagePullPolicy
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(string(test.expectedPolicy), string(policy))
		assert.Equal(string(test.expectedConfigPolicy), string(configPolicy))
	}
}

func TestRedisExtraVolumeMounts(t *testing.T) {
	mode := int32(755)
	tests := []struct {
		name                 string
		expectedVolumes      []corev1.Volume
		expectedVolumeMounts []corev1.VolumeMount
	}{
		{
			name: "EmptyDir",
			expectedVolumes: []corev1.Volume{
				{
					Name: "foo",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			expectedVolumeMounts: []corev1.VolumeMount{
				{
					Name:      "foo",
					MountPath: "/mnt/foo",
				},
			},
		},
		{
			name: "ConfigMap",
			expectedVolumes: []corev1.Volume{
				{
					Name: "bar",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "bar-cm",
							},
							DefaultMode: &mode,
						},
					},
				},
			},
			expectedVolumeMounts: []corev1.VolumeMount{
				{
					Name:      "bar",
					MountPath: "/mnt/scripts",
				},
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var extraVolume corev1.Volume
		var extraVolumeMount corev1.VolumeMount

		rf := generateRF()
		rf.Spec.Redis.ExtraVolumes = test.expectedVolumes
		rf.Spec.Redis.ExtraVolumeMounts = test.expectedVolumeMounts

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			s := args.Get(1).(*appsv1.StatefulSet)
			extraVolume = s.Spec.Template.Spec.Volumes[3]
			extraVolumeMount = s.Spec.Template.Spec.Containers[0].VolumeMounts[4]
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedVolumes[0], extraVolume)
		assert.Equal(test.expectedVolumeMounts[0], extraVolumeMount)
	}
}

func TestSentinelExtraVolumeMounts(t *testing.T) {
	mode := int32(755)
	tests := []struct {
		name                 string
		expectedVolumes      []corev1.Volume
		expectedVolumeMounts []corev1.VolumeMount
	}{
		{
			name: "EmptyDir",
			expectedVolumes: []corev1.Volume{
				{
					Name: "foo",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			expectedVolumeMounts: []corev1.VolumeMount{
				{
					Name:      "foo",
					MountPath: "/mnt/foo",
				},
			},
		},
		{
			name: "ConfigMap",
			expectedVolumes: []corev1.Volume{
				{
					Name: "bar",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "bar-cm",
							},
							DefaultMode: &mode,
						},
					},
				},
			},
			expectedVolumeMounts: []corev1.VolumeMount{
				{
					Name:      "bar",
					MountPath: "/mnt/scripts",
				},
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var extraVolume corev1.Volume
		var extraVolumeMount corev1.VolumeMount

		rf := generateRF()
		rf.Spec.Sentinel.ExtraVolumes = test.expectedVolumes
		rf.Spec.Sentinel.ExtraVolumeMounts = test.expectedVolumeMounts

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			extraVolume = d.Spec.Template.Spec.Volumes[2]
			extraVolumeMount = d.Spec.Template.Spec.Containers[0].VolumeMounts[1]
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedVolumes[0], extraVolume)
		assert.Equal(test.expectedVolumeMounts[0], extraVolumeMount)
	}
}

func TestCustomPort(t *testing.T) {
	default_port := int32(6379)
	custom_port := int32(12345)
	tests := []struct {
		name                  string
		port                  int32
		expectedContainerPort []corev1.ContainerPort
	}{
		{
			name: "Default port",
			port: default_port,
			expectedContainerPort: []corev1.ContainerPort{
				{
					Name:          "redis",
					ContainerPort: default_port,
					Protocol:      corev1.ProtocolTCP,
				},
			},
		},
		{
			name: "Custom port",
			port: custom_port,
			expectedContainerPort: []corev1.ContainerPort{
				{
					Name:          "redis",
					ContainerPort: custom_port,
					Protocol:      corev1.ProtocolTCP,
				},
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var port corev1.ContainerPort

		rf := generateRF()
		rf.Spec.Redis.Port = test.port

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			s := args.Get(1).(*appsv1.StatefulSet)
			port = s.Spec.Template.Spec.Containers[0].Ports[0]
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedContainerPort[0], port)
	}
}

func TestRedisEnv(t *testing.T) {
	default_port := int32(6379)
	tests := []struct {
		name             string
		auth             string
		expectedRedisEnv []corev1.EnvVar
	}{
		{
			name: "without auth",
			auth: "",
			expectedRedisEnv: []corev1.EnvVar{
				{
					Name:  "REDIS_ADDR",
					Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", default_port),
				},
				{
					Name:  "REDIS_PORT",
					Value: fmt.Sprintf("%[1]v", default_port),
				},
				{
					Name:  "REDIS_USER",
					Value: "default",
				},
			},
		},
		{
			name: "with auth",
			auth: "redis-secret",
			expectedRedisEnv: []corev1.EnvVar{
				{
					Name:  "REDIS_ADDR",
					Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", default_port),
				},
				{
					Name:  "REDIS_PORT",
					Value: fmt.Sprintf("%[1]v", default_port),
				},
				{
					Name:  "REDIS_USER",
					Value: "default",
				},
				{
					Name: "REDIS_PASSWORD",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: "redis-secret",
							},
							Key: "password",
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var env []corev1.EnvVar

		rf := generateRF()
		rf.Spec.Redis.Port = default_port
		if test.auth != "" {
			rf.Spec.Auth.SecretPath = test.auth
		}

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		if test.auth != "" {
			ms.On("GetSecret", namespace, test.auth).Once().Return(&corev1.Secret{
				Data: map[string][]byte{"password": []byte("s3cr3t")},
			}, nil)
		}
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			s := args.Get(1).(*appsv1.StatefulSet)
			env = s.Spec.Template.Spec.Containers[0].Env
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedRedisEnv, env)
	}
}

func TestRedisStartupProbe(t *testing.T) {
	mode := int32(0744)
	tests := []struct {
		name                string
		expectedVolume      corev1.Volume
		expectedVolumeMount corev1.VolumeMount
	}{
		{
			name: "startup_config",
			expectedVolume: corev1.Volume{
				Name: "redis-startup-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "startup_config",
						},
						DefaultMode: &mode,
					},
				},
			},
			expectedVolumeMount: corev1.VolumeMount{
				Name:      "redis-startup-config",
				MountPath: "/redis-startup",
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var startupVolumes []corev1.Volume
		var startupVolumeMounts []corev1.VolumeMount

		rf := generateRF()
		rf.Spec.Redis.StartupConfigMap = test.name

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			s := args.Get(1).(*appsv1.StatefulSet)
			startupVolumes = s.Spec.Template.Spec.Volumes
			startupVolumeMounts = s.Spec.Template.Spec.Containers[0].VolumeMounts
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Contains(startupVolumes, test.expectedVolume)
		assert.Contains(startupVolumeMounts, test.expectedVolumeMount)
	}
}

func TestSentinelStartupProbe(t *testing.T) {
	mode := int32(0744)
	tests := []struct {
		name                string
		expectedVolume      corev1.Volume
		expectedVolumeMount corev1.VolumeMount
	}{
		{
			name: "startup_config",
			expectedVolume: corev1.Volume{
				Name: "sentinel-startup-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "startup_config",
						},
						DefaultMode: &mode,
					},
				},
			},
			expectedVolumeMount: corev1.VolumeMount{
				Name:      "sentinel-startup-config",
				MountPath: "/sentinel-startup",
			},
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		var startupVolumes []corev1.Volume
		var startupVolumeMounts []corev1.VolumeMount

		rf := generateRF()
		rf.Spec.Sentinel.StartupConfigMap = test.name

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			startupVolumes = d.Spec.Template.Spec.Volumes
			startupVolumeMounts = d.Spec.Template.Spec.Containers[0].VolumeMounts
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Contains(startupVolumes, test.expectedVolume)
		assert.Contains(startupVolumeMounts, test.expectedVolumeMount)
	}
}

func TestRedisCustomLivenessProbe(t *testing.T) {
	tests := []struct {
		name                  string
		customLivenessProbe   *corev1.Probe
		expectedLivenessProbe *corev1.Probe
	}{
		{
			name: "liveness_probe",
			customLivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p ${REDIS_PORT} --user pinger --pass pingpass --no-auth-warning ping | grep PONG",
						},
					},
				},
			},
			expectedLivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p ${REDIS_PORT} --user pinger --pass pingpass --no-auth-warning ping | grep PONG",
						},
					},
				},
			},
		},
		{
			name:                "liveness_probe_nil",
			customLivenessProbe: nil,
			expectedLivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      5,
				FailureThreshold:    6,
				PeriodSeconds:       15,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h $(hostname) -p 6379 --user pinger --pass pingpass --no-auth-warning ping | grep PONG",
						},
					},
				},
			},
		},
	}
	for _, test := range tests {
		assert := assert.New(t)

		var livenessProbe *corev1.Probe
		rf := generateRF()
		rf.Spec.Redis.CustomLivenessProbe = test.customLivenessProbe
		rf.Spec.Redis.Port = 6379

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			s := args.Get(1).(*appsv1.StatefulSet)
			livenessProbe = s.Spec.Template.Spec.Containers[0].LivenessProbe
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedLivenessProbe, livenessProbe)
	}
}

func TestSentinelCustomLivenessProbe(t *testing.T) {
	tests := []struct {
		name                  string
		customLivenessProbe   *corev1.Probe
		expectedLivenessProbe *corev1.Probe
	}{
		{
			name: "liveness_probe",
			customLivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p 26379 ping",
						},
					},
				},
			},
			expectedLivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p 26379 ping",
						},
					},
				},
			},
		},
		{
			name:                "liveness_probe_nil",
			customLivenessProbe: nil,
			expectedLivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      5,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h $(hostname) -p 26379 ping",
						},
					},
				},
			},
		},
	}
	for _, test := range tests {
		assert := assert.New(t)

		var livenessProbe *corev1.Probe
		rf := generateRF()
		rf.Spec.Sentinel.CustomLivenessProbe = test.customLivenessProbe

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			livenessProbe = d.Spec.Template.Spec.Containers[0].LivenessProbe
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedLivenessProbe, livenessProbe)
	}
}

func TestRedisCustomReadinessProbe(t *testing.T) {
	tests := []struct {
		name                   string
		customReadinessProbe   *corev1.Probe
		expectedReadinessProbe *corev1.Probe
	}{
		{
			name: "readiness_probe",
			customReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/sh", "/redis-readiness/readiness.sh"},
					},
				},
			},
			expectedReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/sh", "/redis-readiness/readiness.sh"},
					},
				},
			},
		},
		{
			name:                 "readiness_probe_nil",
			customReadinessProbe: nil,
			expectedReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      5,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/sh", "/redis-readiness/ready.sh"},
					},
				},
			},
		},
	}
	for _, test := range tests {
		assert := assert.New(t)

		var readinessProbe *corev1.Probe
		rf := generateRF()
		rf.Spec.Redis.CustomReadinessProbe = test.customReadinessProbe

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			s := args.Get(1).(*appsv1.StatefulSet)
			readinessProbe = s.Spec.Template.Spec.Containers[0].ReadinessProbe
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedReadinessProbe, readinessProbe)
	}
}

func TestSentinelCustomReadinessProbe(t *testing.T) {
	tests := []struct {
		name                   string
		customReadinessProbe   *corev1.Probe
		expectedReadinessProbe *corev1.Probe
	}{
		{
			name: "liveness_probe",
			customReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p 26379 ping",
						},
					},
				},
			},
			expectedReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p 26379 ping",
						},
					},
				},
			},
		},
		{
			name:                 "liveness_probe_nil",
			customReadinessProbe: nil,
			expectedReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      5,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h $(hostname) -p 26379 sentinel get-master-addr-by-name mymaster | head -n 1 | grep -vq '127.0.0.1' && redis-cli -h $(hostname) -p 26379 sentinel ckquorum mymaster | grep -q '^OK'",
						},
					},
				},
			},
		},
	}
	for _, test := range tests {
		assert := assert.New(t)

		var readinessProbe *corev1.Probe
		rf := generateRF()
		rf.Spec.Sentinel.CustomReadinessProbe = test.customReadinessProbe

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			readinessProbe = d.Spec.Template.Spec.Containers[0].ReadinessProbe
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedReadinessProbe, readinessProbe)
	}
}

func TestRedisCustomStartupProbe(t *testing.T) {
	tests := []struct {
		name                 string
		customStartupProbe   *corev1.Probe
		expectedStartupProbe *corev1.Probe
	}{
		{
			name: "readiness_probe",
			customStartupProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/sh", "/redis-startup/startup.sh"},
					},
				},
			},
			expectedStartupProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{"/bin/sh", "/redis-startup/startup.sh"},
					},
				},
			},
		},
		{
			name:                 "readiness_probe_nil",
			customStartupProbe:   nil,
			expectedStartupProbe: nil,
		},
	}
	for _, test := range tests {
		assert := assert.New(t)

		var startupProbe *corev1.Probe
		rf := generateRF()
		rf.Spec.Redis.CustomStartupProbe = test.customStartupProbe

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			s := args.Get(1).(*appsv1.StatefulSet)
			startupProbe = s.Spec.Template.Spec.Containers[0].StartupProbe
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedStartupProbe, startupProbe)
	}
}

func TestSentinelCustomStartupProbe(t *testing.T) {
	tests := []struct {
		name                 string
		customStartupProbe   *corev1.Probe
		expectedStartupProbe *corev1.Probe
	}{
		{
			name: "liveness_probe",
			customStartupProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p 26379 ping",
						},
					},
				},
			},
			expectedStartupProbe: &corev1.Probe{
				InitialDelaySeconds: 30,
				TimeoutSeconds:      10,
				FailureThreshold:    10,
				PeriodSeconds:       25,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"redis-cli -h 127.0.0.1 -p 26379 ping",
						},
					},
				},
			},
		},
		{
			name:                 "liveness_probe_nil",
			customStartupProbe:   nil,
			expectedStartupProbe: nil,
		},
	}
	for _, test := range tests {
		assert := assert.New(t)

		var startupProbe *corev1.Probe
		rf := generateRF()
		rf.Spec.Sentinel.CustomStartupProbe = test.customStartupProbe

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			startupProbe = d.Spec.Template.Spec.Containers[0].StartupProbe
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.Equal(test.expectedStartupProbe, startupProbe)
	}
}

func TestRedisPDBUsesRedisReplicaCount(t *testing.T) {
	tests := []struct {
		name             string
		redisReplicas    int32
		sentinelReplicas int32
		expectedMin      intstr.IntOrString
	}{
		{
			name:             "Redis replicas > 2 gives minAvailable 2",
			redisReplicas:    3,
			sentinelReplicas: 1, // should not affect Redis PDB
			expectedMin:      intstr.FromInt(2),
		},
		{
			name:             "Redis replicas <= 2 gives minAvailable 1",
			redisReplicas:    2,
			sentinelReplicas: 5, // should not affect Redis PDB
			expectedMin:      intstr.FromInt(1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			rf.Spec.Redis.Replicas = test.redisReplicas
			rf.Spec.Sentinel.Replicas = test.sentinelReplicas

			var gotPDB *policyv1.PodDisruptionBudget
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotPDB = args.Get(1).(*policyv1.PodDisruptionBudget)
			}).Return(nil)
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

			assert.NoError(err)
			assert.NotNil(gotPDB)
			assert.Equal(test.expectedMin, *gotPDB.Spec.MinAvailable)
		})
	}
}

func TestSentinelPDBUsesSentinelReplicaCount(t *testing.T) {
	tests := []struct {
		name             string
		redisReplicas    int32
		sentinelReplicas int32
		expectedMin      intstr.IntOrString
	}{
		{
			name:             "Sentinel replicas > 2 gives minAvailable 2",
			redisReplicas:    1, // should not affect Sentinel PDB
			sentinelReplicas: 3,
			expectedMin:      intstr.FromInt(2),
		},
		{
			name:             "Sentinel replicas <= 2 gives minAvailable 1",
			redisReplicas:    5, // should not affect Sentinel PDB
			sentinelReplicas: 2,
			expectedMin:      intstr.FromInt(1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			rf.Spec.Redis.Replicas = test.redisReplicas
			rf.Spec.Sentinel.Replicas = test.sentinelReplicas

			var gotPDB *policyv1.PodDisruptionBudget
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotPDB = args.Get(1).(*policyv1.PodDisruptionBudget)
			}).Return(nil)
			ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
			ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

			assert.NoError(err)
			assert.NotNil(gotPDB)
			assert.Equal(test.expectedMin, *gotPDB.Spec.MinAvailable)
		})
	}
}

func TestPDBSelectorContainsOnlyStableLabels(t *testing.T) {
	// Extra metadata labels that should appear on the PDB resource but NOT in the selector.
	extraLabels := map[string]string{
		"team":        "infra",
		"environment": "production",
	}
	stableKeys := []string{
		"app.kubernetes.io/name",
		"app.kubernetes.io/component",
		"app.kubernetes.io/part-of",
	}

	t.Run("Redis PDB selector is stable", func(t *testing.T) {
		assert := assert.New(t)
		rf := generateRF()

		var gotPDB *policyv1.PodDisruptionBudget
		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			gotPDB = args.Get(1).(*policyv1.PodDisruptionBudget)
		}).Return(nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, extraLabels, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.NotNil(gotPDB)
		selector := gotPDB.Spec.Selector.MatchLabels
		// Extra propagated labels must not appear in the selector.
		for k := range extraLabels {
			assert.NotContains(selector, k, "extra label %q must not appear in PDB selector", k)
		}
		// Stable workload labels must be present.
		for _, k := range stableKeys {
			assert.Contains(selector, k, "stable label %q must be present in PDB selector", k)
		}
		// The selector must contain only the stable keys.
		assert.Len(selector, len(stableKeys))
	})

	t.Run("Sentinel PDB selector is stable", func(t *testing.T) {
		assert := assert.New(t)
		rf := generateRF()

		var gotPDB *policyv1.PodDisruptionBudget
		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			gotPDB = args.Get(1).(*policyv1.PodDisruptionBudget)
		}).Return(nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, extraLabels, []metav1.OwnerReference{})

		assert.NoError(err)
		assert.NotNil(gotPDB)
		selector := gotPDB.Spec.Selector.MatchLabels
		for k := range extraLabels {
			assert.NotContains(selector, k, "extra label %q must not appear in PDB selector", k)
		}
		for _, k := range stableKeys {
			assert.Contains(selector, k, "stable label %q must be present in PDB selector", k)
		}
		assert.Len(selector, len(stableKeys))
	})
}

// ---------------------------------------------------------------------------
// EnsureRedisConfigMap / generateRedisConfigMap
//
// These are the first tests asserting what generateRedisConfigMap actually
// *renders* into redis.conf, rather than only exercising the upstream
// CustomCommandRenames validation in api/redisfailover/v1/validate.go (which
// restricts From/To to ^[A-Za-z_]+$ to prevent redis.conf injection). Pinning
// the rendered template output here catches a regression in the template
// itself, which the validation-layer tests alone cannot do.
// ---------------------------------------------------------------------------

func TestEnsureRedisConfigMapNoPassword(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Redis.Port = 6379

	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotCM) {
		content := gotCM.Data["redis.conf"]
		assert.Contains(content, "slaveof 127.0.0.1 6379")
		assert.Contains(content, "port 6379")
		assert.Contains(content, "tcp-keepalive 60")
		assert.Contains(content, "save 900 1")
		assert.Contains(content, "save 300 10")
		assert.Contains(content, "user pinger -@all +ping on >pingpass")
		assert.NotContains(content, "masterauth", "no password configured: masterauth must not be rendered")
		assert.NotContains(content, "requirepass", "no password configured: requirepass must not be rendered")
	}
}

func TestEnsureRedisConfigMapWithPassword(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(&corev1.Secret{
		Data: map[string][]byte{"password": []byte("s3cr3t-pw")},
	}, nil)
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotCM) {
		content := gotCM.Data["redis.conf"]
		assert.Contains(content, "masterauth s3cr3t-pw")
		assert.Contains(content, "requirepass s3cr3t-pw")
	}
}

func TestEnsureRedisConfigMapCustomCommandRenames(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Redis.CustomCommandRenames = []redisfailoverv1.RedisCommandRename{
		{From: "FLUSHALL", To: "RENAMED_FLUSHALL"},
		{From: "CONFIG", To: "RENAMED_CONFIG"},
	}

	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotCM) {
		content := gotCM.Data["redis.conf"]

		firstLine := `rename-command "FLUSHALL" "RENAMED_FLUSHALL"`
		secondLine := `rename-command "CONFIG" "RENAMED_CONFIG"`

		firstIdx := strings.Index(content, firstLine)
		secondIdx := strings.Index(content, secondLine)

		assert.GreaterOrEqual(firstIdx, 0, "expected %q to be rendered on its own line", firstLine)
		assert.GreaterOrEqual(secondIdx, 0, "expected %q to be rendered on its own line", secondLine)
		assert.Less(firstIdx, secondIdx, "rename-command lines must be rendered in declaration order")
	}
}

func TestEnsureRedisConfigMapPasswordFetchErrorPropagates(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	ms := &mK8SService.Services{}
	ms.On("GetSecret", namespace, "redis-secret").Once().Return(nil, errors.New("secret fetch failed"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.Error(err)
	ms.AssertNotCalled(t, "CreateOrUpdateConfigMap", mock.Anything, mock.Anything)
}

// ---------------------------------------------------------------------------
// EnsureSentinelConfigMap / generateSentinelConfigMap
// ---------------------------------------------------------------------------

func TestEnsureSentinelConfigMapContent(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Redis.Port = 6379

	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureSentinelConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotCM) {
		// This pins down the current fixed sentinel.conf values so a future
		// accidental change to the template is caught.
		expected := `sentinel monitor mymaster 127.0.0.1 6379 2
sentinel down-after-milliseconds mymaster 1000
sentinel failover-timeout mymaster 3000
sentinel parallel-syncs mymaster 2`
		assert.Equal(expected, gotCM.Data["sentinel.conf"])
	}
}

// ---------------------------------------------------------------------------
// EnsureRedisShutdownConfigMap / generateRedisShutdownConfigMap
// ---------------------------------------------------------------------------

func TestEnsureRedisShutdownConfigMapGenerated(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Name = "my-redis"
	rf.Spec.Redis.Port = 6379

	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotCM) {
		content := gotCM.Data["shutdown.sh"]
		// rf.Name "my-redis" is upper-cased and its dashes replaced with
		// underscores to build the RFS_<NAME>_SERVICE_* env var names.
		assert.Contains(content, "RFS_MY_REDIS_SERVICE_HOST")
		assert.Contains(content, "RFS_MY_REDIS_SERVICE_PORT_SENTINEL")
		assert.Contains(content, "redis-cli -p 6379")
	}
}

func TestEnsureRedisShutdownConfigMapUserSuppliedExists(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Redis.ShutdownConfigMap = "custom-shutdown-cm"

	ms := &mK8SService.Services{}
	ms.On("GetConfigMap", namespace, "custom-shutdown-cm").Once().Return(&corev1.ConfigMap{}, nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	ms.AssertNotCalled(t, "CreateOrUpdateConfigMap", mock.Anything, mock.Anything)
	ms.AssertExpectations(t)
}

func TestEnsureRedisShutdownConfigMapUserSuppliedMissingPropagatesError(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Redis.ShutdownConfigMap = "custom-shutdown-cm"

	ms := &mK8SService.Services{}
	ms.On("GetConfigMap", namespace, "custom-shutdown-cm").Once().Return(nil, errors.New("configmaps \"custom-shutdown-cm\" not found"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.Error(err)
	ms.AssertNotCalled(t, "CreateOrUpdateConfigMap", mock.Anything, mock.Anything)
}

// ---------------------------------------------------------------------------
// EnsureRedisReadinessConfigMap / generateRedisReadinessConfigMap
// ---------------------------------------------------------------------------

func TestEnsureRedisReadinessConfigMap(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Redis.Port = 6380

	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisReadinessConfigMap(rf, nil, []metav1.OwnerReference{})

	assert.NoError(err)
	if assert.NotNil(gotCM) {
		content := gotCM.Data["ready.sh"]
		assert.Contains(content, "redis-cli -p 6380")
	}
}

// ---------------------------------------------------------------------------
// EnsureNotPresentRedisService
// ---------------------------------------------------------------------------

func TestEnsureNotPresentRedisServiceDeletesExisting(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(&corev1.Service{}, nil)
	ms.On("DeleteService", namespace, svcName).Once().Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	assert.NoError(err)
	ms.AssertExpectations(t)
}

func TestEnsureNotPresentRedisServiceNoopWhenAbsent(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(nil, errors.New("services \""+svcName+"\" not found"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	assert.NoError(err)
	ms.AssertNotCalled(t, "DeleteService", mock.Anything, mock.Anything)
}

// TestEnsureNotPresentRedisServiceSwallowsNonNotFoundGetErrors documents what
// looks like a real bug found while adding this coverage (left unfixed, as
// this PR is test-only): EnsureNotPresentRedisService only checks whether
// GetService returned a nil error, so *any* error from GetService -- a real
// API failure (forbidden, timeout, ...), not just "not found" -- is silently
// treated as "the service does not exist" and the function returns nil
// without ever attempting the delete or surfacing the failure to the caller.
func TestEnsureNotPresentRedisServiceSwallowsNonNotFoundGetErrors(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(nil, errors.New("connection reset by peer"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	assert.NoError(err, "documents current behavior: any GetService error is treated as absence, see comment above")
	ms.AssertNotCalled(t, "DeleteService", mock.Anything, mock.Anything)
}

func TestEnsureNotPresentRedisServiceDeleteErrorPropagates(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(&corev1.Service{}, nil)
	ms.On("DeleteService", namespace, svcName).Once().Return(errors.New("delete failed"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	assert.Error(err)
}

// ---------------------------------------------------------------------------
// EnsureNotPresentSentinelResources
// ---------------------------------------------------------------------------

func TestEnsureNotPresentSentinelResourcesDeletesAllExisting(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	resName := rfservice.GetSentinelName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, resName).Once().Return(&appsv1.Deployment{}, nil)
	ms.On("DeleteDeployment", namespace, resName).Once().Return(nil)
	ms.On("GetService", namespace, resName).Once().Return(&corev1.Service{}, nil)
	ms.On("DeleteService", namespace, resName).Once().Return(nil)
	ms.On("GetConfigMap", namespace, resName).Once().Return(&corev1.ConfigMap{}, nil)
	ms.On("DeleteConfigMap", namespace, resName).Once().Return(nil)
	ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(&policyv1.PodDisruptionBudget{}, nil)
	ms.On("DeletePodDisruptionBudget", namespace, resName).Once().Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentSentinelResources(rf)

	assert.NoError(err)
	ms.AssertExpectations(t)
}

func TestEnsureNotPresentSentinelResourcesNoopWhenAbsent(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	resName := rfservice.GetSentinelName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetConfigMap", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(nil, errors.New("not found"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentSentinelResources(rf)

	assert.NoError(err)
	ms.AssertNotCalled(t, "DeleteDeployment", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeleteService", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeleteConfigMap", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeletePodDisruptionBudget", mock.Anything, mock.Anything)
}

func TestEnsureNotPresentSentinelResourcesDeleteErrorPropagates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(ms *mK8SService.Services, resName string)
	}{
		{
			name: "Deployment delete error stops before other deletes",
			setup: func(ms *mK8SService.Services, resName string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(&appsv1.Deployment{}, nil)
				ms.On("DeleteDeployment", namespace, resName).Once().Return(errors.New("delete deployment failed"))
			},
		},
		{
			name: "Service delete error",
			setup: func(ms *mK8SService.Services, resName string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetService", namespace, resName).Once().Return(&corev1.Service{}, nil)
				ms.On("DeleteService", namespace, resName).Once().Return(errors.New("delete service failed"))
			},
		},
		{
			name: "ConfigMap delete error",
			setup: func(ms *mK8SService.Services, resName string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetConfigMap", namespace, resName).Once().Return(&corev1.ConfigMap{}, nil)
				ms.On("DeleteConfigMap", namespace, resName).Once().Return(errors.New("delete configmap failed"))
			},
		},
		{
			name: "PodDisruptionBudget delete error",
			setup: func(ms *mK8SService.Services, resName string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetConfigMap", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(&policyv1.PodDisruptionBudget{}, nil)
				ms.On("DeletePodDisruptionBudget", namespace, resName).Once().Return(errors.New("delete pdb failed"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			resName := rfservice.GetSentinelName(rf)

			ms := &mK8SService.Services{}
			test.setup(ms, resName)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureNotPresentSentinelResources(rf)

			assert.Error(err)
		})
	}
}
