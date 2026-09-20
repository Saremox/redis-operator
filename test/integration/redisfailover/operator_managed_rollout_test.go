//go:build integration

package redisfailover_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/util/homedir"
	"k8s.io/utils/ptr"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	redisfailoverclientset "github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/cmd/utils"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

// This file's own namespace/name/secret constants, distinct from
// creation_test.go's, so the two integration tests never share a namespace:
// namespace deletion in a real cluster is asynchronous, and starting this
// test's own namespace create/delete cycle would risk racing the previous
// test's still-terminating one.
const (
	ommNamespace      = "rf-integration-tests-operator-managed"
	ommName           = "testing-omm"
	ommRedisSize      = int32(2)
	ommAuthSecretPath = "redis-auth-omm"
	ommTestPass       = "test-pass-omm"
)

// ommClients mirrors the `clients` helper in creation_test.go, but keeps its
// own namespace baked in rather than sharing the package-level `namespace`
// constant, since this test's RedisFailover is operator-managed
// (sentinel.enabled: false) and must not collide with the sentinel-managed
// one created elsewhere in this package.
type ommClients struct {
	k8sClient   kubernetes.Interface
	rfClient    redisfailoverclientset.Interface
	redisClient redis.Client
}

func (c *ommClients) prepareNS() error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ommNamespace,
		},
	}
	_, err := c.k8sClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	return err
}

func (c *ommClients) cleanup(stopC chan struct{}) {
	c.k8sClient.CoreV1().Namespaces().Delete(context.Background(), ommNamespace, metav1.DeleteOptions{})
	close(stopC)
}

func (c *ommClients) waitForPodsReady(labelSelector string, expectedCount int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pods, err := c.k8sClient.CoreV1().Pods(ommNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return err
		}

		readyCount := 0
		for _, pod := range pods.Items {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					readyCount++
					break
				}
			}
		}

		if readyCount >= expectedCount {
			return nil
		}

		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timed out waiting for %d pods to be Ready", expectedCount)
}

// podUIDs returns the current UID of every pod matching labelSelector, keyed
// by pod name. StatefulSet pods keep the same name across a delete+recreate
// cycle (ordinal-based naming), so name alone can't detect a replacement -
// the UID changes on every recreation and is what actually proves the
// operator tore down and rebuilt a given pod, rather than the pod simply
// still being the original one.
func (c *ommClients) podUIDs(labelSelector string) (map[string]types.UID, error) {
	pods, err := c.k8sClient.CoreV1().Pods(ommNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	uids := make(map[string]types.UID, len(pods.Items))
	for _, pod := range pods.Items {
		uids[pod.Name] = pod.UID
	}
	return uids, nil
}

// waitForAllPodsRecreated polls until every pod name present in before has a
// different UID in the live cluster (i.e. every original pod was deleted and
// replaced), or the timeout elapses.
func (c *ommClients) waitForAllPodsRecreated(labelSelector string, before map[string]types.UID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current, err := c.podUIDs(labelSelector)
		if err != nil {
			return err
		}

		allRecreated := len(current) == len(before)
		for podName, oldUID := range before {
			newUID, ok := current[podName]
			if !ok || newUID == oldUID {
				allRecreated = false
				break
			}
		}
		if allRecreated {
			return nil
		}

		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timed out waiting for all pods matching %q to be recreated", labelSelector)
}

func (c *ommClients) onlyMaster(labelSelector string) (string, error) {
	pods, err := c.k8sClient.CoreV1().Pods(ommNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return "", err
	}

	masters := []string{}
	for _, pod := range pods.Items {
		ip := pod.Status.PodIP
		if ok, _ := c.redisClient.IsMaster(ip, "6379", ommTestPass); ok {
			masters = append(masters, ip)
		}
	}
	if len(masters) != 1 {
		return "", fmt.Errorf("expected exactly one master, found %d: %v", len(masters), masters)
	}
	return masters[0], nil
}

// TestRedisFailoverOperatorManagedModeRollout reproduces, end-to-end against
// a real cluster, the exact scenario behind the production outage fixed in
// #161: UpdateRedisesPods (operator/redisfailover/checker.go) used to gate
// replacing a stale-revision master pod on a Sentinel quorum check that
// unconditionally called GetSentinelsIPs - which 404'd in operator-managed
// mode (sentinel.enabled: false, the default since v4.0.0) because no
// Sentinel Deployment exists there. That left any operator-managed
// RedisFailover permanently NotHealthy the moment its master pod needed
// replacing, which happens on any routine rollout.
//
// Every other integration test in this package runs with Sentinel explicitly
// enabled, so this is also the only end-to-end coverage of the default mode
// at all, and the only one that exercises a rollout (a StatefulSet template
// change) rather than just initial creation.
func TestRedisFailoverOperatorManagedModeRollout(t *testing.T) {
	// Runs alongside TestRedisFailover (creation_test.go): separate
	// namespaces, separate in-process operator instances (each with its own
	// leader-election lease scoped to its own namespace), separate Secrets -
	// nothing here is shared state, so there's no reason to pay for the two
	// tests' pod-startup waits back to back instead of concurrently.
	t.Parallel()

	require := require.New(t)

	stopC := make(chan struct{})
	errC := make(chan error)

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
	}

	flags := &utils.CMDFlags{
		KubeConfig:  kubeconfig,
		Development: true,
	}

	k8sClient, customClient, err := utils.CreateKubernetesClients(flags)
	require.NoError(err)

	redisClient := redis.New(metrics.Dummy)

	c := ommClients{
		k8sClient:   k8sClient,
		rfClient:    customClient,
		redisClient: redisClient,
	}

	require.NoError(c.prepareNS())
	// Wait for the namespace to be ready, rather than guessing how long that takes.
	require.NoError(waitForNamespaceActive(k8sClient, ommNamespace, 15*time.Second))

	k8sservice := k8s.New(k8sClient, customClient, log.Dummy, metrics.Dummy)
	redisfailoverOperator, err := redisfailover.New(redisfailover.Config{}, k8sservice, k8sClient, ommNamespace, redisClient, metrics.Dummy, log.Dummy)
	require.NoError(err)

	go func() {
		errC <- redisfailoverOperator.Run(context.Background())
	}()
	defer c.cleanup(stopC)

	// There's no external readiness signal for "the operator started"; this
	// just fails fast if it crashed immediately instead of silently waiting
	// out the full window.
	require.NoError(waitForOperatorStartup(errC, 15*time.Second))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ommAuthSecretPath,
			Namespace: ommNamespace,
		},
		Data: map[string][]byte{
			"password": []byte(ommTestPass),
		},
	}
	_, err = k8sClient.CoreV1().Secrets(ommNamespace).Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(err)

	toCreate := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ommName,
			Namespace: ommNamespace,
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas:        ommRedisSize,
				ImagePullPolicy: corev1.PullIfNotPresent,
				CustomConfig:    []string{`save ""`},
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				// The point of this test: explicitly operator-managed, the
				// default since v4.0.0 and the mode #161 broke.
				Enabled: ptr.To(false),
			},
			Auth: redisfailoverv1.AuthSettings{
				SecretPath: ommAuthSecretPath,
			},
		},
	}
	_, err = c.rfClient.DatabasesV1().RedisFailovers(ommNamespace).Create(context.Background(), toCreate, metav1.CreateOptions{})
	require.NoError(err)

	redisLabelSelector := fmt.Sprintf("app.kubernetes.io/component=redis,redisfailovers.databases.spotahome.com/name=%s", ommName)

	t.Run("Redis pods become Ready without Sentinel", func(t *testing.T) {
		assert := assert.New(t)
		if err := c.waitForPodsReady(redisLabelSelector, int(ommRedisSize), 3*time.Minute); err != nil {
			t.Fatalf("redis pods never became ready: %v", err)
		}

		// No Sentinel Deployment should exist at all in operator-managed mode.
		_, err := k8sClient.AppsV1().Deployments(ommNamespace).Get(context.Background(), fmt.Sprintf("rfs-%s", ommName), metav1.GetOptions{})
		assert.True(kubeerrors.IsNotFound(err), "expected no Sentinel Deployment in operator-managed mode, got: %v", err)
	})

	t.Run("Exactly one master is elected without Sentinel", func(t *testing.T) {
		assert := assert.New(t)
		_, err := c.onlyMaster(redisLabelSelector)
		assert.NoError(err)
	})

	t.Run("Rollout replaces every pod including the master, with no Sentinel gate blocking it", func(t *testing.T) {
		assert := assert.New(t)

		before, err := c.podUIDs(redisLabelSelector)
		require.NoError(err)
		require.Len(before, int(ommRedisSize))

		// Trigger a StatefulSet template change: PodAnnotations is copied
		// straight into the pod template (generateRedisStatefulSet), so this
		// bumps the StatefulSet's UpdateRevision without changing anything
		// about redis itself. The StatefulSet uses OnDelete update strategy,
		// so Kubernetes will NOT roll the pods itself - only the operator's
		// UpdateRedisesPods does, which is exactly the code path #161 broke
		// for operator-managed mode.
		live, err := c.rfClient.DatabasesV1().RedisFailovers(ommNamespace).Get(context.Background(), ommName, metav1.GetOptions{})
		require.NoError(err)
		live.Spec.Redis.PodAnnotations = map[string]string{"rollout-test": "1"}
		_, err = c.rfClient.DatabasesV1().RedisFailovers(ommNamespace).Update(context.Background(), live, metav1.UpdateOptions{})
		require.NoError(err)

		// Before #161, this would hang forever in operator-managed mode: the
		// slave pods would get replaced, but replacing the master would never
		// happen because the (inapplicable) Sentinel-quorum gate returned nil
		// indefinitely instead of ever proceeding, and GetSentinelsIPs would
		// have errored against a Sentinel Deployment that doesn't exist here.
		if err := c.waitForAllPodsRecreated(redisLabelSelector, before, 5*time.Minute); err != nil {
			t.Fatalf("rollout never completed: %v", err)
		}

		if err := c.waitForPodsReady(redisLabelSelector, int(ommRedisSize), 3*time.Minute); err != nil {
			t.Fatalf("redis pods never became ready again after rollout: %v", err)
		}

		_, err = c.onlyMaster(redisLabelSelector)
		assert.NoError(err, "exactly one master must be elected again after the rollout, with no Sentinel involved")
	})
}
