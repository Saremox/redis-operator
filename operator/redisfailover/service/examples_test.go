package service_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
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

// examplePodLabels returns the labels of the Redis and the Sentinel pods that
// the operator creates for the RedisFailover name.
func examplePodLabels(t *testing.T, name string) (labels.Set, labels.Set) {
	t.Helper()
	rf := generateRF()
	rf.Name = name
	var gotSS *appsv1.StatefulSet
	var gotD *appsv1.Deployment
	ms := &mK8SService.Services{}
	ms.On("CreateOrUpdatePodDisruptionBudget", namespace, mock.Anything).Return(nil, nil)
	ms.On("CreateOrUpdateServiceAccount", namespace, mock.Anything).Return(nil)
	ms.On("CreateOrUpdateStatefulSet", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotSS = args.Get(1).(*appsv1.StatefulSet)
	}).Return(nil)
	ms.On("CreateOrUpdateDeployment", namespace, mock.Anything).Once().Run(func(args mock.Arguments) {
		gotD = args.Get(1).(*appsv1.Deployment)
	}).Return(nil)
	client := rfservice.NewRedisFailoverKubeClient(ms, log.Dummy, metrics.Dummy)
	require.NoError(t, client.EnsureRedisStatefulset(rf, nil, []metav1.OwnerReference{}))
	require.NoError(t, client.EnsureSentinelDeployment(rf, nil, []metav1.OwnerReference{}))
	return labels.Set(gotSS.Spec.Template.Labels), labels.Set(gotD.Spec.Template.Labels)
}

// TestNetworkPolicyExampleSelectsThePods checks the selectors of the example
// against the labels that the operator sets. A selector that selects no pod
// isolates nothing, and a peer selector that selects the pods of another
// RedisFailover lets its Sentinels merge with this group.
func TestNetworkPolicyExampleSelectsThePods(t *testing.T) {
	data, err := os.ReadFile("../../../example/redisfailover/networkpolicy.yaml")
	require.NoError(t, err)
	var policies []networkingv1.NetworkPolicy
	for _, doc := range bytes.Split(data, []byte("\n---\n")) {
		var meta metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal(doc, &meta))
		if meta.Kind != "NetworkPolicy" {
			continue
		}
		var policy networkingv1.NetworkPolicy
		require.NoError(t, yaml.UnmarshalStrict(doc, &policy))
		policies = append(policies, policy)
	}
	require.Len(t, policies, 2)
	redisPod, sentinelPod := examplePodLabels(t, "redisfailover")
	otherRedis, otherSentinel := examplePodLabels(t, "other")

	selects := func(selector metav1.LabelSelector, pod labels.Set) bool {
		s, err := metav1.LabelSelectorAsSelector(&selector)
		require.NoError(t, err)
		return s.Matches(pod)
	}
	for i := range 2 {
		policy := policies[i]
		assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, policy.Spec.PolicyTypes, policy.Name)
		for j, other := range []labels.Set{sentinelPod, redisPod, otherSentinel, otherRedis} {
			assert.Equal(t, j == i, selects(policy.Spec.PodSelector, other), "%s selects pod %d", policy.Name, j)
		}
		// The first ingress rule and the first egress rule allow the pods of
		// the same RedisFailover, and no pod of another one.
		peers := []metav1.LabelSelector{*policy.Spec.Ingress[0].From[0].PodSelector, *policy.Spec.Egress[0].To[0].PodSelector}
		for _, peer := range peers {
			assert.True(t, selects(peer, sentinelPod), policy.Name)
			assert.True(t, selects(peer, redisPod), policy.Name)
			assert.False(t, selects(peer, otherSentinel), policy.Name)
			assert.False(t, selects(peer, otherRedis), policy.Name)
		}
	}
}
