package service_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	mK8SService "github.com/saremox/redis-operator/mocks/service/k8s"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// A pod anti-affinity term that selects no Redis pod has no effect, so the
// example must select the labels that the operator sets on its Redis pods.
func TestPodAntiAffinityExampleSelectsRedisPods(t *testing.T) {
	data, err := os.ReadFile("../../../example/redisfailover/pod-anti-affinity.yaml")
	require.NoError(t, err)
	rf := &redisfailoverv1.RedisFailover{}
	require.NoError(t, yaml.UnmarshalStrict(data, rf))
	rf.Namespace = namespace
	require.NoError(t, rf.Validate())

	var gotSS *appsv1.StatefulSet
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Once().Return(nil, nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotSS = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)

	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{}))
	require.NotNil(t, gotSS)

	podLabels := labels.Set(gotSS.Spec.Template.Labels)
	terms := gotSS.Spec.Template.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	require.NotEmpty(t, terms)
	for _, term := range terms {
		selector, err := metav1.LabelSelectorAsSelector(term.PodAffinityTerm.LabelSelector)
		require.NoError(t, err)
		assert.True(t, selector.Matches(podLabels), "selector %s does not match the Redis pod labels %s", selector, podLabels)
	}
}
