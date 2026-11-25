package service_test

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestRedisStatefulSetStorageGeneration(t *testing.T) {
	executeMode := int32(0o744)
	configVolume := func(name, configMap string, mode *int32) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMap}, DefaultMode: mode,
		}}}
	}
	// The three ConfigMap volumes are the same for each storage. The data
	// volume of an emptyDir comes after them, and a claim template has none.
	volumes := func(data ...corev1.Volume) []corev1.Volume {
		rf := generateRF()
		return append([]corev1.Volume{
			configVolume("redis-config", rfservice.GetRedisName(rf), nil),
			configVolume("redis-shutdown-config", rfservice.GetRedisShutdownConfigMapName(rf), &executeMode),
			configVolume("redis-readiness-config", rfservice.GetRedisReadinessName(rf), &executeMode),
		}, data...)
	}
	mounts := func(data string) []corev1.VolumeMount {
		return []corev1.VolumeMount{
			{Name: "redis-config", MountPath: "/redis"},
			{Name: "redis-shutdown-config", MountPath: "/redis-shutdown"},
			{Name: "redis-readiness-config", MountPath: "/redis-readiness"},
			{Name: data, MountPath: "/data"},
		}
	}
	claimSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{"ReadWriteOnce"},
		Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
	}
	claimStorage := func(keep bool) redisfailoverv1.RedisStorage {
		return redisfailoverv1.RedisStorage{
			KeepAfterDeletion: keep,
			PersistentVolumeClaim: &redisfailoverv1.EmbeddedPersistentVolumeClaim{
				EmbeddedObjectMetadata: redisfailoverv1.EmbeddedObjectMetadata{Name: "pvc-data"}, Spec: claimSpec,
			},
		}
	}
	claimTemplate := func(owners []metav1.OwnerReference) []corev1.PersistentVolumeClaim {
		return []corev1.PersistentVolumeClaim{{
			TypeMeta:   metav1.TypeMeta{Kind: "PersistentVolumeClaim", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-data", OwnerReferences: owners},
			Spec:       claimSpec,
		}}
	}
	owners := []metav1.OwnerReference{{Name: "testing"}}
	memory := &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}

	tests := []struct {
		name          string
		storage       redisfailoverv1.RedisStorage
		ownerRefs     []metav1.OwnerReference
		wantVolumes   []corev1.Volume
		wantMounts    []corev1.VolumeMount
		wantTemplates []corev1.PersistentVolumeClaim
	}{
		{
			name:        "Default values",
			wantVolumes: volumes(corev1.Volume{Name: "redis-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
			wantMounts:  mounts("redis-data"),
		},
		{
			name:        "Defined an emptydir with storage on memory",
			storage:     redisfailoverv1.RedisStorage{EmptyDir: memory},
			wantVolumes: volumes(corev1.Volume{Name: "redis-data", VolumeSource: corev1.VolumeSource{EmptyDir: memory}}),
			wantMounts:  mounts("redis-data"),
		},
		{
			name:          "Defined an persistentvolumeclaim",
			storage:       claimStorage(false),
			wantVolumes:   volumes(),
			wantMounts:    mounts("pvc-data"),
			wantTemplates: claimTemplate(nil),
		},
		{
			name:          "Defined an persistentvolumeclaim with ownerRefs",
			storage:       claimStorage(false),
			ownerRefs:     owners,
			wantVolumes:   volumes(),
			wantMounts:    mounts("pvc-data"),
			wantTemplates: claimTemplate(owners),
		},
		{
			name:          "Defined an persistentvolumeclaim with ownerRefs keeping the pvc",
			storage:       claimStorage(true),
			ownerRefs:     owners,
			wantVolumes:   volumes(),
			wantMounts:    mounts("pvc-data"),
			wantTemplates: claimTemplate(nil),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rf := generateRF()
			rf.Spec.Redis.Storage = test.storage

			var got appsv1.StatefulSet
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				got = *args.Get(1).(*appsv1.StatefulSet)
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			require.NoError(t, client.EnsureRedisStatefulset(rf, nil, test.ownerRefs))

			assert.Equal(t, test.wantVolumes, got.Spec.Template.Spec.Volumes)
			assert.Equal(t, test.wantMounts, got.Spec.Template.Spec.Containers[0].VolumeMounts)
			assert.Equal(t, test.wantTemplates, got.Spec.VolumeClaimTemplates)
		})
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
			} else {
				autoName := rfservice.GetSentinelServiceAccountName(rf)
				ms.On("GetServiceAccount", namespace, autoName).Once().Return(nil, apierrors.NewNotFound(corev1.Resource("serviceaccounts"), autoName))
			}
			ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				d := args.Get(1).(*appsv1.Deployment)
				gotServiceAccountName = d.Spec.Template.Spec.ServiceAccountName
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

			assert.Equal(expectedServiceAccountName, gotServiceAccountName)
			require.NoError(t, err)

			if test.expectServiceAccountCreated {
				ms.AssertCalled(t, "CreateOrUpdateServiceAccount", namespace, mock.Anything)
			} else {
				ms.AssertNotCalled(t, "CreateOrUpdateServiceAccount", mock.Anything, mock.Anything)
			}
		})
	}
}

func TestRedisShutdownConfigMapRetries(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Sentinel.Enabled = new(true)

	var script string
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", rf.Namespace, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		c := args.Get(1).(*corev1.ConfigMap)
		if s, ok := c.Data["shutdown.sh"]; ok {
			script = s
		}
	})

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{})
	require.NoError(t, err)

	// Both sentinel calls are retried: a single failed query used to be enough
	// to skip the failover and shut the master down anyway.
	assert.Contains(script, `while [ -z "$master" ] && [ "$retries" -lt 3 ]`)
	assert.Contains(script, `while [ "$failover" != "OK" ] && [ "$retries" -lt 3 ]`)

	// The preStop hook runs under /bin/sh, which is dash on the debian-based
	// redis images. "let" does not exist there, so the counter would never
	// increment and the retry loop would spin forever.
	assert.NotContains(script, "let ")
	assert.Contains(script, "retries=$((retries + 1))")
}

func TestSentinelServiceExporterPort(t *testing.T) {
	sentinelPort := corev1.ServicePort{
		Name:       "sentinel",
		Port:       26379,
		TargetPort: intstr.FromInt(26379),
		Protocol:   corev1.ProtocolTCP,
	}
	metricsPort := corev1.ServicePort{
		Name:       "metrics",
		Port:       9355,
		TargetPort: intstr.FromInt(9355),
		Protocol:   corev1.ProtocolTCP,
	}
	customMetricsPort := corev1.ServicePort{
		Name:       "metrics",
		Port:       19355,
		TargetPort: intstr.FromInt(19355),
		Protocol:   corev1.ProtocolTCP,
	}

	tests := []struct {
		name            string
		exporterEnabled bool
		exporterPort    int32
		expectedPorts   []corev1.ServicePort
	}{
		{
			name:            "exporter disabled exposes only the sentinel port",
			exporterEnabled: false,
			expectedPorts:   []corev1.ServicePort{sentinelPort},
		},
		{
			name:            "exporter enabled also exposes the metrics port",
			exporterEnabled: true,
			expectedPorts:   []corev1.ServicePort{sentinelPort, metricsPort},
		},
		{
			name:            "exporter port override is honoured",
			exporterEnabled: true,
			exporterPort:    19355,
			expectedPorts:   []corev1.ServicePort{sentinelPort, customMetricsPort},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF()
			rf.Spec.Sentinel.Exporter.Enabled = test.exporterEnabled
			rf.Spec.Sentinel.Exporter.Port = test.exporterPort

			generatedService := corev1.Service{}

			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateService", rf.Namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				s := args.Get(1).(*corev1.Service)
				generatedService = *s
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureSentinelService(rf, nil, []metav1.OwnerReference{{Name: "testing"}})

			require.NoError(t, err)
			assert.Equal(test.expectedPorts, generatedService.Spec.Ports)
		})
	}
}

func TestRedisHostNetworkAndDnsPolicy(t *testing.T) {
	tests := []struct {
		name                string
		hostNetwork         bool
		expectedHostNetwork bool
		dnsPolicy           corev1.DNSPolicy
		expectedDNSPolicy   corev1.DNSPolicy
	}{
		{
			name:                "Default",
			expectedHostNetwork: false,
			expectedDNSPolicy:   corev1.DNSClusterFirst,
		},
		{
			name:                "Custom",
			hostNetwork:         true,
			expectedHostNetwork: true,
			dnsPolicy:           corev1.DNSClusterFirstWithHostNet,
			expectedDNSPolicy:   corev1.DNSClusterFirstWithHostNet,
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		rf := generateRF()
		rf.Spec.Redis.HostNetwork = test.hostNetwork
		rf.Spec.Redis.DNSPolicy = test.dnsPolicy

		var actualHostNetwork bool
		var actualDNSPolicy corev1.DNSPolicy

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			ss := args.Get(1).(*appsv1.StatefulSet)
			actualHostNetwork = ss.Spec.Template.Spec.HostNetwork
			actualDNSPolicy = ss.Spec.Template.Spec.DNSPolicy
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})
		require.NoError(t, err)

		assert.Equal(test.expectedHostNetwork, actualHostNetwork)
		assert.Equal(test.expectedDNSPolicy, actualDNSPolicy)
	}
}

func TestSentinelHostNetworkAndDnsPolicy(t *testing.T) {
	tests := []struct {
		name                string
		hostNetwork         bool
		expectedHostNetwork bool
		dnsPolicy           corev1.DNSPolicy
		expectedDNSPolicy   corev1.DNSPolicy
	}{
		{
			name:                "Default",
			expectedHostNetwork: false,
			expectedDNSPolicy:   corev1.DNSClusterFirst,
		},
		{
			name:                "Custom",
			hostNetwork:         true,
			expectedHostNetwork: true,
			dnsPolicy:           corev1.DNSClusterFirstWithHostNet,
			expectedDNSPolicy:   corev1.DNSClusterFirstWithHostNet,
		},
	}

	for _, test := range tests {
		assert := assert.New(t)

		rf := generateRF()
		rf.Spec.Sentinel.HostNetwork = test.hostNetwork
		rf.Spec.Sentinel.DNSPolicy = test.dnsPolicy

		var actualHostNetwork bool
		var actualDNSPolicy corev1.DNSPolicy

		ms := &mK8SService.Services{}
		ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
		ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
		ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
			d := args.Get(1).(*appsv1.Deployment)
			actualHostNetwork = d.Spec.Template.Spec.HostNetwork
			actualDNSPolicy = d.Spec.Template.Spec.DNSPolicy
		}).Return(nil)

		client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
		err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})
		require.NoError(t, err)

		assert.Equal(test.expectedHostNetwork, actualHostNetwork)
		assert.Equal(test.expectedDNSPolicy, actualDNSPolicy)
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
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			var policy corev1.PullPolicy
			var exporterPolicy corev1.PullPolicy

			rf := generateRF()
			rf.Spec.Redis.ImagePullPolicy = test.policy
			rf.Spec.Redis.Exporter.Enabled = true
			rf.Spec.Redis.Exporter.ImagePullPolicy = test.exporterPolicy

			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				ss := args.Get(1).(*appsv1.StatefulSet)
				policy = ss.Spec.Template.Spec.Containers[0].ImagePullPolicy
				exporterPolicy = ss.Spec.Template.Spec.Containers[1].ImagePullPolicy
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

			require.NoError(t, err)
			assert.Equal(string(test.expectedPolicy), string(policy))
			assert.Equal(string(test.expectedExporterPolicy), string(exporterPolicy))
		})
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
		t.Run(test.name, func(t *testing.T) {
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

			require.NoError(t, err)
			assert.Equal(string(test.expectedPolicy), string(policy))
			assert.Equal(string(test.expectedConfigPolicy), string(configPolicy))
		})
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

		require.NoError(t, err)
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

		require.NoError(t, err)
		assert.Equal(test.expectedVolumes[0], extraVolume)
		assert.Equal(test.expectedVolumeMounts[0], extraVolumeMount)
	}
}

func TestCustomPort(t *testing.T) {
	defaultPort := int32(6379)
	customPort := int32(12345)
	tests := []struct {
		name                  string
		port                  int32
		expectedContainerPort []corev1.ContainerPort
	}{
		{
			name: "Default port",
			port: defaultPort,
			expectedContainerPort: []corev1.ContainerPort{
				{
					Name:          "redis",
					ContainerPort: defaultPort,
					Protocol:      corev1.ProtocolTCP,
				},
			},
		},
		{
			name: "Custom port",
			port: customPort,
			expectedContainerPort: []corev1.ContainerPort{
				{
					Name:          "redis",
					ContainerPort: customPort,
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

		require.NoError(t, err)
		assert.Equal(test.expectedContainerPort[0], port)
	}
}

func TestRedisEnv(t *testing.T) {
	defaultPort := int32(6379)
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
					Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", defaultPort),
				},
				{
					Name:  "REDIS_PORT",
					Value: fmt.Sprintf("%[1]v", defaultPort),
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
					Value: fmt.Sprintf("redis://127.0.0.1:%[1]v", defaultPort),
				},
				{
					Name:  "REDIS_PORT",
					Value: fmt.Sprintf("%[1]v", defaultPort),
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
		rf.Spec.Redis.Port = defaultPort
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

		require.NoError(t, err)
		assert.Equal(test.expectedRedisEnv, env)
	}
}

func TestRedisStartupProbe(t *testing.T) {
	mode := int32(0o744)
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

		require.NoError(t, err)
		assert.Contains(startupVolumes, test.expectedVolume)
		assert.Contains(startupVolumeMounts, test.expectedVolumeMount)
	}
}

func TestSentinelStartupProbe(t *testing.T) {
	mode := int32(0o744)
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

		require.NoError(t, err)
		assert.Contains(startupVolumes, test.expectedVolume)
		assert.Contains(startupVolumeMounts, test.expectedVolumeMount)
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

			require.NoError(t, err)
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

			require.NoError(t, err)
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

		require.NoError(t, err)
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

		require.NoError(t, err)
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

	require.NoError(t, err)
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

// TestEnsureRedisConfigMapWithPassword used to assert that a configured auth
// secret's password was embedded in the redis ConfigMap as plaintext
// masterauth/requirepass directives. That was a real credential-exposure bug
// (ConfigMaps are not encrypted at rest and are broadly readable) - fixed by
// passing the password to redis-server via --requirepass/--masterauth
// command args sourced from the REDIS_PASSWORD env instead (see
// getRedisCommand and TestRedisConfigMapExcludesPassword /
// TestRedisCommandUsesEnvAuthWhenSecretConfigured in
// auth_out_of_configmap_test.go). This now asserts the fixed behavior:
// EnsureRedisConfigMap no longer needs the password at all, so it must not
// even attempt to fetch the secret.
func TestEnsureRedisConfigMapWithPassword(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()
	rf.Spec.Auth.SecretPath = "redis-secret"

	var gotCM *corev1.ConfigMap
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotCM = args.Get(1).(*corev1.ConfigMap)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisConfigMap(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	if assert.NotNil(gotCM) {
		content := gotCM.Data["redis.conf"]
		assert.NotContains(content, "masterauth")
		assert.NotContains(content, "requirepass")
	}
	ms.AssertNotCalled(t, "GetSecret", mock.Anything, mock.Anything)
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

	require.NoError(t, err)
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

	require.NoError(t, err)
	if assert.NotNil(gotCM) {
		// The timeouts must be the sentinel customConfig defaults. Otherwise a
		// restarted Sentinel disagrees with the other Sentinels.
		expected := `sentinel monitor mymaster 127.0.0.1 6379 2
sentinel down-after-milliseconds mymaster 5000
sentinel failover-timeout mymaster 10000
sentinel parallel-syncs mymaster 2`
		assert.Equal(expected, gotCM.Data["sentinel.conf"])
	}
}

// ---------------------------------------------------------------------------
// EnsureRedisShutdownConfigMap / generateRedisShutdownConfigMap
// ---------------------------------------------------------------------------

func TestEnsureRedisShutdownConfigMapGenerated(t *testing.T) {
	tests := []struct {
		name         string
		sentinel     *bool
		bootstrap    *redisfailoverv1.BootstrapSettings
		wantSentinel bool
	}{
		{name: "sentinel enabled", sentinel: new(true), wantSentinel: true},
		{name: "sentinel disabled", sentinel: new(false)},
		{name: "sentinel unset", sentinel: nil},
		{
			name:         "bootstrap with sentinels allowed",
			sentinel:     new(true),
			bootstrap:    &redisfailoverv1.BootstrapSettings{Host: "127.0.0.1", AllowSentinels: true},
			wantSentinel: true,
		},
		{
			name:      "bootstrap without sentinels",
			sentinel:  new(true),
			bootstrap: &redisfailoverv1.BootstrapSettings{Host: "127.0.0.1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			rf.Name = "my-redis"
			rf.Spec.Redis.Port = 6379
			rf.Spec.Sentinel.Enabled = test.sentinel
			rf.Spec.BootstrapNode = test.bootstrap

			var gotCM *corev1.ConfigMap
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdateConfigMap", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotCM = args.Get(1).(*corev1.ConfigMap)
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{})

			require.NoError(t, err)
			if assert.NotNil(gotCM) {
				content := gotCM.Data["shutdown.sh"]
				// The redis pods have no service links, so the script must
				// not depend on the RFS_<NAME>_SERVICE_* variables.
				assert.NotContains(content, "RFS_")
				assert.Contains(content, "redis-cli -p 6379")
				assert.True(strings.HasSuffix(content, "eval $save_command"))
				if test.wantSentinel {
					assert.Contains(content, "redis-cli -h rfs-my-redis -p 26379 --csv SENTINEL get-master-addr-by-name mymaster")
					assert.Contains(content, "redis-cli -h rfs-my-redis -p 26379 SENTINEL failover mymaster")
					// The script must end inside the default 30s grace period.
					assert.NotContains(content, "sleep 3")
					assert.Contains(content, "deadline=$(($(date +%s) + 12))")
					assert.Contains(content, "CLIENT PAUSE")
					assert.Contains(content, `[ "$(date +%s)" -lt "$deadline" ]`)
				} else {
					assert.NotContains(content, "SENTINEL")
					assert.NotContains(content, "26379")
					assert.NotContains(content, "CLIENT PAUSE")
					assert.True(strings.HasPrefix(content, `cmd="redis-cli -p 6379"`))
				}
			}
		})
	}
}

func TestEnsureRedisShutdownConfigMapUserSuppliedExists(t *testing.T) {
	rf := generateRF()
	rf.Spec.Redis.ShutdownConfigMap = "custom-shutdown-cm"

	ms := &mK8SService.Services{}
	ms.On("GetConfigMap", namespace, "custom-shutdown-cm").Once().Return(&corev1.ConfigMap{}, nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	ms.AssertNotCalled(t, "CreateOrUpdateConfigMap", mock.Anything, mock.Anything)
	ms.AssertExpectations(t)
}

func TestEnsureRedisShutdownConfigMapUserSuppliedMissingPropagatesError(t *testing.T) {
	rf := generateRF()
	rf.Spec.Redis.ShutdownConfigMap = "custom-shutdown-cm"

	ms := &mK8SService.Services{}
	ms.On("GetConfigMap", namespace, "custom-shutdown-cm").Once().Return(nil, errors.New("configmaps \"custom-shutdown-cm\" not found"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisShutdownConfigMap(rf, nil, []metav1.OwnerReference{})

	require.Error(t, err)
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

	require.NoError(t, err)
	if assert.NotNil(gotCM) {
		content := gotCM.Data["ready.sh"]
		assert.Contains(content, "redis-cli -p 6380")
	}
}

// ---------------------------------------------------------------------------
// EnsureNotPresentRedisService
// ---------------------------------------------------------------------------

func TestEnsureNotPresentRedisServiceDeletesExisting(t *testing.T) {
	rf := generateOwnedRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(&corev1.Service{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteService", namespace, svcName).Once().Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	require.NoError(t, err)
	ms.AssertExpectations(t)
}

func TestEnsureNotPresentRedisServiceNoopWhenAbsent(t *testing.T) {
	rf := generateOwnedRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(nil, errors.New("services \""+svcName+"\" not found"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	require.NoError(t, err)
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
	rf := generateOwnedRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(nil, errors.New("connection reset by peer"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	require.NoError(t, err, "documents current behavior: any GetService error is treated as absence, see comment above")
	ms.AssertNotCalled(t, "DeleteService", mock.Anything, mock.Anything)
}

func TestEnsureNotPresentRedisServiceDeleteErrorPropagates(t *testing.T) {
	assert := assert.New(t)
	rf := generateOwnedRF()
	svcName := rfservice.GetRedisName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetService", namespace, svcName).Once().Return(&corev1.Service{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteService", namespace, svcName).Once().Return(errors.New("delete failed"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentRedisService(rf)

	assert.Error(err)
}

// ---------------------------------------------------------------------------
// EnsureNotPresentSentinelResources
// ---------------------------------------------------------------------------

func TestEnsureNotPresentSentinelResourcesDeletesAllExisting(t *testing.T) {
	rf := generateOwnedRF()
	resName := rfservice.GetSentinelName(rf)
	saName := rfservice.GetSentinelServiceAccountName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, resName).Once().Return(&appsv1.Deployment{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteDeployment", namespace, resName).Once().Return(nil)
	ms.On("GetService", namespace, resName).Once().Return(&corev1.Service{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteService", namespace, resName).Once().Return(nil)
	ms.On("GetConfigMap", namespace, resName).Once().Return(&corev1.ConfigMap{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteConfigMap", namespace, resName).Once().Return(nil)
	ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(&policyv1.PodDisruptionBudget{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeletePodDisruptionBudget", namespace, resName).Once().Return(nil)
	ms.On("GetServiceAccount", namespace, saName).Once().Return(&corev1.ServiceAccount{ObjectMeta: ownedMeta()}, nil)
	ms.On("DeleteServiceAccount", namespace, saName).Once().Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentSentinelResources(rf)

	require.NoError(t, err)
	ms.AssertExpectations(t)
}

func TestEnsureNotPresentSentinelResourcesNoopWhenAbsent(t *testing.T) {
	rf := generateOwnedRF()
	resName := rfservice.GetSentinelName(rf)
	saName := rfservice.GetSentinelServiceAccountName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetConfigMap", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetServiceAccount", namespace, saName).Once().Return(nil, errors.New("not found"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentSentinelResources(rf)

	require.NoError(t, err)
	ms.AssertNotCalled(t, "DeleteDeployment", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeleteService", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeleteConfigMap", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeletePodDisruptionBudget", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeleteServiceAccount", mock.Anything, mock.Anything)
}

// TestEnsureNotPresentSentinelResourcesSkipsUserProvidedServiceAccount mirrors
// the guard EnsureSentinelDeployment uses on creation: when the user set
// rf.Spec.Sentinel.ServiceAccountName themselves, that ServiceAccount is
// theirs to manage, so cleanup must not even look it up, let alone delete it.
func TestEnsureNotPresentSentinelResourcesSkipsUserProvidedServiceAccount(t *testing.T) {
	rf := generateOwnedRF()
	rf.Spec.Sentinel.ServiceAccountName = "user-managed-sa"
	resName := rfservice.GetSentinelName(rf)

	ms := &mK8SService.Services{}
	ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetConfigMap", namespace, resName).Once().Return(nil, errors.New("not found"))
	ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(nil, errors.New("not found"))

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureNotPresentSentinelResources(rf)

	require.NoError(t, err)
	ms.AssertNotCalled(t, "GetServiceAccount", mock.Anything, mock.Anything)
	ms.AssertNotCalled(t, "DeleteServiceAccount", mock.Anything, mock.Anything)
	ms.AssertExpectations(t)
}

func TestEnsureNotPresentSentinelResourcesDeleteErrorPropagates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(ms *mK8SService.Services, resName string, saName string)
	}{
		{
			name: "Deployment delete error stops before other deletes",
			setup: func(ms *mK8SService.Services, resName string, _ string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(&appsv1.Deployment{ObjectMeta: ownedMeta()}, nil)
				ms.On("DeleteDeployment", namespace, resName).Once().Return(errors.New("delete deployment failed"))
			},
		},
		{
			name: "Service delete error",
			setup: func(ms *mK8SService.Services, resName string, _ string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetService", namespace, resName).Once().Return(&corev1.Service{ObjectMeta: ownedMeta()}, nil)
				ms.On("DeleteService", namespace, resName).Once().Return(errors.New("delete service failed"))
			},
		},
		{
			name: "ConfigMap delete error",
			setup: func(ms *mK8SService.Services, resName string, _ string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetConfigMap", namespace, resName).Once().Return(&corev1.ConfigMap{ObjectMeta: ownedMeta()}, nil)
				ms.On("DeleteConfigMap", namespace, resName).Once().Return(errors.New("delete configmap failed"))
			},
		},
		{
			name: "PodDisruptionBudget delete error",
			setup: func(ms *mK8SService.Services, resName string, _ string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetConfigMap", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(&policyv1.PodDisruptionBudget{ObjectMeta: ownedMeta()}, nil)
				ms.On("DeletePodDisruptionBudget", namespace, resName).Once().Return(errors.New("delete pdb failed"))
			},
		},
		{
			name: "ServiceAccount delete error",
			setup: func(ms *mK8SService.Services, resName string, saName string) {
				ms.On("GetDeployment", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetService", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetConfigMap", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetPodDisruptionBudget", namespace, resName).Once().Return(nil, errors.New("not found"))
				ms.On("GetServiceAccount", namespace, saName).Once().Return(&corev1.ServiceAccount{ObjectMeta: ownedMeta()}, nil)
				ms.On("DeleteServiceAccount", namespace, saName).Once().Return(errors.New("delete serviceaccount failed"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateOwnedRF()
			resName := rfservice.GetSentinelName(rf)
			saName := rfservice.GetSentinelServiceAccountName(rf)

			ms := &mK8SService.Services{}
			test.setup(ms, resName, saName)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureNotPresentSentinelResources(rf)

			assert.Error(err)
		})
	}
}

// ---------------------------------------------------------------------------
// InitContainers / ExtraContainers redis env merging
// (getInitContainersWithRedisEnv / getExtraContainersWithRedisEnv / getContainersWithRedisEnv)
// ---------------------------------------------------------------------------

func TestRedisInitAndExtraContainersGetRedisEnvAppended(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Redis.Port = 6379
	rf.Spec.Redis.InitContainers = []corev1.Container{
		{
			Name:  "init-container",
			Image: "busybox",
			Env: []corev1.EnvVar{
				{Name: "USER_SUPPLIED", Value: "init-value"},
			},
		},
	}
	rf.Spec.Redis.ExtraContainers = []corev1.Container{
		{
			Name:  "extra-container",
			Image: "busybox",
			Env: []corev1.EnvVar{
				{Name: "USER_SUPPLIED", Value: "extra-value"},
			},
		},
	}

	var gotSS *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotSS = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	if assert.NotNil(gotSS) {
		// The operator prepends its own rdb-tempfile-cleanup init container, so
		// look the user-supplied one up by name rather than by position.
		var initContainer corev1.Container
		for _, c := range gotSS.Spec.Template.Spec.InitContainers {
			if c.Name == "init-container" {
				initContainer = c
			}
		}
		if assert.Equal("init-container", initContainer.Name) {
			assert.Equal([]corev1.EnvVar{
				{Name: "USER_SUPPLIED", Value: "init-value"},
				{Name: "REDIS_ADDR", Value: "redis://127.0.0.1:6379"},
				{Name: "REDIS_PORT", Value: "6379"},
				{Name: "REDIS_USER", Value: "default"},
			}, initContainer.Env)
		}

		// Containers[0] is the redis container; the exporter is disabled here so
		// ExtraContainers are appended right after it.
		if assert.Len(gotSS.Spec.Template.Spec.Containers, 2) {
			extraContainer := gotSS.Spec.Template.Spec.Containers[1]
			assert.Equal("extra-container", extraContainer.Name)
			assert.Equal([]corev1.EnvVar{
				{Name: "USER_SUPPLIED", Value: "extra-value"},
				{Name: "REDIS_ADDR", Value: "redis://127.0.0.1:6379"},
				{Name: "REDIS_PORT", Value: "6379"},
				{Name: "REDIS_USER", Value: "default"},
			}, extraContainer.Env)
		}
	}
}

// ---------------------------------------------------------------------------
// createSentinelExporterContainer / sentinel Exporter+InitContainers+ExtraContainers
// ---------------------------------------------------------------------------

func TestSentinelExporterContainer(t *testing.T) {
	customResources := &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("250m"),
		},
	}

	tests := []struct {
		name              string
		resources         *corev1.ResourceRequirements
		expectedResources corev1.ResourceRequirements
	}{
		{
			name:      "default resources are used when none are given",
			resources: nil,
			expectedResources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1000m"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
		},
		{
			name:              "custom resources override the defaults",
			resources:         customResources,
			expectedResources: *customResources,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF()
			rf.Spec.Sentinel.Exporter.Enabled = true
			rf.Spec.Sentinel.Exporter.Image = "sentinel-exporter:latest"
			rf.Spec.Sentinel.Exporter.Args = []string{"--extra-arg"}
			rf.Spec.Sentinel.Exporter.Env = []corev1.EnvVar{{Name: "CUSTOM_ENV", Value: "yes"}}
			rf.Spec.Sentinel.Exporter.Resources = test.resources
			rf.Spec.Sentinel.InitContainers = []corev1.Container{{Name: "sentinel-init", Image: "busybox"}}
			rf.Spec.Sentinel.ExtraContainers = []corev1.Container{{Name: "sentinel-extra", Image: "busybox"}}

			var gotDeployment *appsv1.Deployment
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Once().Return(nil)
			ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotDeployment = args.Get(1).(*appsv1.Deployment)
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{})

			require.NoError(t, err)
			if assert.NotNil(gotDeployment) {
				// sentinel, sentinel-exporter, sentinel-extra
				if assert.Len(gotDeployment.Spec.Template.Spec.Containers, 3) {
					exporter := gotDeployment.Spec.Template.Spec.Containers[1]
					assert.Equal("sentinel-exporter", exporter.Name)
					assert.Equal("sentinel-exporter:latest", exporter.Image)
					assert.Equal([]string{"--extra-arg"}, exporter.Args)
					assert.Equal(test.expectedResources, exporter.Resources)
					assert.Equal([]corev1.ContainerPort{
						{Name: "metrics", ContainerPort: 9355, Protocol: corev1.ProtocolTCP},
					}, exporter.Ports)
					assert.Equal([]corev1.EnvVar{
						{Name: "CUSTOM_ENV", Value: "yes"},
						{
							Name: "REDIS_ALIAS",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							},
						},
						{Name: "REDIS_EXPORTER_WEB_LISTEN_ADDRESS", Value: "0.0.0.0:9355"},
						{Name: "REDIS_ADDR", Value: "redis://127.0.0.1:26379"},
					}, exporter.Env)

					assert.Equal("sentinel-extra", gotDeployment.Spec.Template.Spec.Containers[2].Name)
				}

				if assert.Len(gotDeployment.Spec.Template.Spec.InitContainers, 2) {
					assert.Equal("sentinel-config-copy", gotDeployment.Spec.Template.Spec.InitContainers[0].Name)
					assert.Equal("sentinel-init", gotDeployment.Spec.Template.Spec.InitContainers[1].Name)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// createRedisExporterContainer custom resources branch
// ---------------------------------------------------------------------------

func TestRedisExporterCustomResources(t *testing.T) {
	assert := assert.New(t)

	customResources := &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"),
		},
	}

	rf := generateRF()
	rf.Spec.Redis.Exporter.Enabled = true
	rf.Spec.Redis.Exporter.Resources = customResources

	var gotSS *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotSS = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	if assert.NotNil(gotSS) && assert.Len(gotSS.Spec.Template.Spec.Containers, 2) {
		assert.Equal(*customResources, gotSS.Spec.Template.Spec.Containers[1].Resources)
	}
}

func TestRedisExporterListensOnCustomPort(t *testing.T) {
	tests := []struct {
		name       string
		port       int32
		wantPort   int32
		wantListen string
	}{
		{name: "default port leaves the listen address alone", wantPort: 9121},
		{name: "custom port sets the listen address", port: 19121, wantPort: 19121, wantListen: "0.0.0.0:19121"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			rf := generateRF()
			rf.Spec.Redis.Exporter.Enabled = true
			rf.Spec.Redis.Exporter.Port = test.port

			var gotSS *appsv1.StatefulSet
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				gotSS = args.Get(1).(*appsv1.StatefulSet)
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			require.NoError(t, client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{}))

			if assert.NotNil(gotSS) && assert.Len(gotSS.Spec.Template.Spec.Containers, 2) {
				exporter := gotSS.Spec.Template.Spec.Containers[1]
				assert.Equal(test.wantPort, exporter.Ports[0].ContainerPort)
				var listen string
				for _, env := range exporter.Env {
					if env.Name == "REDIS_EXPORTER_WEB_LISTEN_ADDRESS" {
						listen = env.Value
					}
				}
				assert.Equal(test.wantListen, listen)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// getRedisExporterEnv / envExists
// ---------------------------------------------------------------------------

func TestRedisExporterEnvRespectsUserSuppliedVarsAndAuthSecret(t *testing.T) {
	tests := []struct {
		name        string
		exporterEnv []corev1.EnvVar
		secretPath  string
		expectedEnv []corev1.EnvVar
	}{
		{
			name: "user-supplied REDIS_USER is not duplicated",
			exporterEnv: []corev1.EnvVar{
				{Name: "REDIS_USER", Value: "custom-user"},
			},
			expectedEnv: []corev1.EnvVar{
				{Name: "REDIS_USER", Value: "custom-user"},
				{
					Name: "REDIS_ALIAS",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
					},
				},
				{Name: "REDIS_ADDR", Value: "redis://127.0.0.1:6379"},
				{Name: "REDIS_PORT", Value: "6379"},
			},
		},
		{
			name: "user-supplied REDIS_PASSWORD is kept over the auth secret reference",
			exporterEnv: []corev1.EnvVar{
				{Name: "REDIS_PASSWORD", Value: "user-supplied-pw"},
			},
			secretPath: "redis-secret",
			expectedEnv: []corev1.EnvVar{
				{Name: "REDIS_PASSWORD", Value: "user-supplied-pw"},
				{
					Name: "REDIS_ALIAS",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
					},
				},
				{Name: "REDIS_ADDR", Value: "redis://127.0.0.1:6379"},
				{Name: "REDIS_PORT", Value: "6379"},
				{Name: "REDIS_USER", Value: "default"},
			},
		},
		{
			name:       "auth secret is referenced when REDIS_PASSWORD isn't already set",
			secretPath: "redis-secret",
			expectedEnv: []corev1.EnvVar{
				{
					Name: "REDIS_ALIAS",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
					},
				},
				{Name: "REDIS_ADDR", Value: "redis://127.0.0.1:6379"},
				{Name: "REDIS_PORT", Value: "6379"},
				{Name: "REDIS_USER", Value: "default"},
				{
					Name: "REDIS_PASSWORD",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "redis-secret"},
							Key:                  "password",
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			rf := generateRF()
			rf.Spec.Redis.Port = 6379
			rf.Spec.Redis.Exporter.Enabled = true
			rf.Spec.Redis.Exporter.Env = test.exporterEnv
			rf.Spec.Auth.SecretPath = test.secretPath

			var gotEnv []corev1.EnvVar
			ms := &mK8SService.Services{}
			ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
			if test.secretPath != "" {
				ms.On("GetSecret", namespace, test.secretPath).Once().Return(&corev1.Secret{
					Data: map[string][]byte{"password": []byte("s3cr3t")},
				}, nil)
			}
			ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
				ss := args.Get(1).(*appsv1.StatefulSet)
				gotEnv = ss.Spec.Template.Spec.Containers[1].Env
			}).Return(nil)

			client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
			err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

			require.NoError(t, err)
			assert.Equal(test.expectedEnv, gotEnv)
		})
	}
}

// ---------------------------------------------------------------------------
// getAffinity / getSecurityContext / getTerminationGracePeriodSeconds
// user-supplied override branches
// ---------------------------------------------------------------------------

func TestGetAffinityUsesUserSuppliedValue(t *testing.T) {
	assert := assert.New(t)

	customAffinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{Key: "disktype", Operator: corev1.NodeSelectorOpIn, Values: []string{"ssd"}},
						},
					},
				},
			},
		},
	}

	rf := generateRF()
	rf.Spec.Redis.Affinity = customAffinity

	var gotAffinity *corev1.Affinity
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		ss := args.Get(1).(*appsv1.StatefulSet)
		gotAffinity = ss.Spec.Template.Spec.Affinity
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	// getAffinity must return the user-supplied Affinity unchanged, instead of
	// building the default soft anti-affinity.
	assert.Same(customAffinity, gotAffinity)
}

// TestGetAffinityDefaultMatchesMasterPod checks that the default soft
// anti-affinity also selects the master pod, whose role label the operator
// changes. Otherwise a replica can go to the node of the master.
func TestGetAffinityDefaultMatchesMasterPod(t *testing.T) {
	assert := assert.New(t)
	rf := generateRF()

	var ss *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		ss = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisStatefulset(rf, map[string]string{"some": "label"}, []metav1.OwnerReference{}))

	terms := ss.Spec.Template.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if assert.Len(terms, 1) {
		selector, err := metav1.LabelSelectorAsSelector(terms[0].PodAffinityTerm.LabelSelector)
		assert.NoError(err)
		masterPodLabels := map[string]string{}
		maps.Copy(masterPodLabels, ss.Spec.Template.Labels)
		masterPodLabels["redisfailovers-role"] = "master"
		assert.True(selector.Matches(k8slabels.Set(masterPodLabels)), "selector %v must match the master pod", selector)
		assert.True(selector.Matches(k8slabels.Set(ss.Spec.Template.Labels)), "selector %v must match a replica pod", selector)
	}
}

// TestGetSecurityContextUsesUserSuppliedValue used to assert that a partial
// user-supplied PodSecurityContext replaced the operator's defaults
// wholesale (returned unchanged, same pointer) - a real bug: setting only
// RunAsUser silently dropped RunAsGroup/RunAsNonRoot/FSGroup too. Fixed to
// merge instead: the user's fields win, unset fields keep their default. See
// TestGetSecurityContextMerge in generator_securitycontext_test.go for
// direct coverage of getSecurityContext itself; this asserts the same fixed
// behavior end-to-end through EnsureRedisStatefulset.
func TestGetSecurityContextUsesUserSuppliedValue(t *testing.T) {
	assert := assert.New(t)

	uid := int64(2000)
	customSecCtx := &corev1.PodSecurityContext{
		RunAsUser: &uid,
	}

	rf := generateRF()
	rf.Spec.Redis.SecurityContext = customSecCtx

	var gotSecCtx *corev1.PodSecurityContext
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		ss := args.Get(1).(*appsv1.StatefulSet)
		gotSecCtx = ss.Spec.Template.Spec.SecurityContext
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	// The user's RunAsUser wins...
	assert.Equal(uid, *gotSecCtx.RunAsUser)
	// ...but the operator defaults still fill every field the user left unset.
	assert.Equal(int64(1000), *gotSecCtx.RunAsGroup)
	assert.True(*gotSecCtx.RunAsNonRoot)
	assert.Equal(int64(1000), *gotSecCtx.FSGroup)
	// The input spec object itself must not be mutated by the merge.
	assert.Nil(customSecCtx.RunAsGroup)
}

// TestGetContainerSecurityContextUsesUserSuppliedValue: same fix as above,
// for the per-container SecurityContext. See
// TestGetContainerSecurityContextMerge in generator_securitycontext_test.go
// for direct coverage of getContainerSecurityContext itself.
func TestGetContainerSecurityContextUsesUserSuppliedValue(t *testing.T) {
	assert := assert.New(t)

	privileged := true
	customSecCtx := &corev1.SecurityContext{
		Privileged: &privileged,
	}

	rf := generateRF()
	rf.Spec.Redis.ContainerSecurityContext = customSecCtx

	var gotSecCtx *corev1.SecurityContext
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		ss := args.Get(1).(*appsv1.StatefulSet)
		gotSecCtx = ss.Spec.Template.Spec.Containers[0].SecurityContext
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	// The user's Privileged wins...
	assert.True(*gotSecCtx.Privileged)
	// ...but the operator defaults still fill every field the user left unset.
	assert.Equal([]corev1.Capability{"ALL"}, gotSecCtx.Capabilities.Drop)
	assert.True(*gotSecCtx.ReadOnlyRootFilesystem)
	assert.False(*gotSecCtx.AllowPrivilegeEscalation)
	// The input spec object itself must not be mutated by the merge.
	assert.Nil(customSecCtx.Capabilities)
}

func TestGetTerminationGracePeriodSecondsUsesUserSuppliedValue(t *testing.T) {
	assert := assert.New(t)

	rf := generateRF()
	rf.Spec.Redis.TerminationGracePeriodSeconds = 120

	var got *int64
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		ss := args.Get(1).(*appsv1.StatefulSet)
		got = ss.Spec.Template.Spec.TerminationGracePeriodSeconds
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	err := client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{})

	require.NoError(t, err)
	if assert.NotNil(got) {
		assert.EqualValues(120, *got)
	}
}
