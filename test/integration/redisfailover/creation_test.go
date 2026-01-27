//go:build integration

package redisfailover_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/util/homedir"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	redisfailoverclientset "github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/cmd/utils"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/operator/redisfailover"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

const (
	name           = "testing"
	namespace      = "rf-integration-tests"
	redisSize      = int32(3)
	sentinelSize   = int32(3)
	authSecretPath = "redis-auth"
	testPass       = "test-pass"
	redisAddr      = "redis://127.0.0.1:6379"
)

type clients struct {
	k8sClient   kubernetes.Interface
	rfClient    redisfailoverclientset.Interface
	aeClient    apiextensionsclientset.Interface
	redisClient redis.Client
}

func boolPtr(b bool) *bool {
	return &b
}

// waitForPodsReady waits for all pods matching the label selector to be Ready
func (c *clients) waitForPodsReady(labelSelector string, expectedCount int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		listOptions := metav1.ListOptions{
			LabelSelector: labelSelector,
		}
		pods, err := c.k8sClient.CoreV1().Pods(namespace).List(context.Background(), listOptions)
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

// printPodDiagnostics prints pod status for debugging
func (c *clients) printPodDiagnostics() {
	pods, err := c.k8sClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fmt.Printf("Error listing pods: %v\n", err)
		return
	}

	fmt.Println("=== Pod Diagnostics ===")
	for _, pod := range pods.Items {
		fmt.Printf("Pod: %s, Phase: %s\n", pod.Name, pod.Status.Phase)
		for _, cond := range pod.Status.Conditions {
			fmt.Printf("  Condition: %s = %s (Reason: %s)\n", cond.Type, cond.Status, cond.Reason)
		}
		for _, cs := range pod.Status.ContainerStatuses {
			fmt.Printf("  Container: %s, Ready: %t, RestartCount: %d\n", cs.Name, cs.Ready, cs.RestartCount)
			if cs.State.Waiting != nil {
				fmt.Printf("    Waiting: %s - %s\n", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
			if cs.State.Terminated != nil {
				fmt.Printf("    Terminated: %s - %s\n", cs.State.Terminated.Reason, cs.State.Terminated.Message)
			}
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			fmt.Printf("  InitContainer: %s, Ready: %t\n", cs.Name, cs.Ready)
			if cs.State.Waiting != nil {
				fmt.Printf("    Waiting: %s - %s\n", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
			if cs.State.Terminated != nil {
				fmt.Printf("    Terminated: ExitCode=%d, Reason=%s\n", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
			}
		}
	}
	fmt.Println("=== End Pod Diagnostics ===")
}

func (c *clients) prepareNS() error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	_, err := c.k8sClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	return err
}

func (c *clients) cleanup(stopC chan struct{}) {
	c.k8sClient.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
	close(stopC)
}

func TestRedisFailover(t *testing.T) {
	require := require.New(t)

	// Create signal channels.
	stopC := make(chan struct{})
	errC := make(chan error)

	flags := &utils.CMDFlags{
		KubeConfig:  filepath.Join(homedir.HomeDir(), ".kube", "config"),
		Development: true,
	}

	// Kubernetes clients.
	k8sClient, customClient, aeClientset, err := utils.CreateKubernetesClients(flags)
	require.NoError(err)

	// Create the redis clients
	redisClient := redis.New(metrics.Dummy)

	clients := clients{
		k8sClient:   k8sClient,
		rfClient:    customClient,
		aeClient:    aeClientset,
		redisClient: redisClient,
	}

	// Create kubernetes service.
	k8sservice := k8s.New(k8sClient, customClient, aeClientset, log.Dummy, metrics.Dummy)

	// Prepare namespace
	prepErr := clients.prepareNS()
	require.NoError(prepErr)

	// Give time to the namespace to be ready
	time.Sleep(15 * time.Second)

	// Create operator and run.
	redisfailoverOperator, err := redisfailover.New(redisfailover.Config{}, k8sservice, k8sClient, namespace, redisClient, metrics.Dummy, log.Dummy)
	require.NoError(err)

	go func() {
		errC <- redisfailoverOperator.Run(context.Background())
	}()

	// Prepare cleanup for when the test ends
	defer clients.cleanup(stopC)

	// Give time to the operator to start
	time.Sleep(15 * time.Second)

	// Create secret
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      authSecretPath,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"password": []byte(testPass),
		},
	}
	_, err = k8sClient.CoreV1().Secrets(namespace).Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(err)

	// Check that if we create a RedisFailover, it is certainly created and we can get it
	ok := t.Run("Check Custom Resource Creation", clients.testCRCreation)
	require.True(ok, "the custom resource has to be created to continue")

	// Giving time to the operator to create the resources
	time.Sleep(3 * time.Minute)

	// Wait for Redis pods to be Ready before running connectivity tests
	redisLabelSelector := fmt.Sprintf("app.kubernetes.io/component=redis,redisfailovers.databases.spotahome.com/name=%s", name)
	if err := clients.waitForPodsReady(redisLabelSelector, int(redisSize), 3*time.Minute); err != nil {
		t.Logf("Warning: Redis pods not ready: %v", err)
		clients.printPodDiagnostics()
	}

	// Wait for Sentinel pods to be Ready
	sentinelLabelSelector := fmt.Sprintf("app.kubernetes.io/component=sentinel,redisfailovers.databases.spotahome.com/name=%s", name)
	if err := clients.waitForPodsReady(sentinelLabelSelector, int(sentinelSize), 2*time.Minute); err != nil {
		t.Logf("Warning: Sentinel pods not ready: %v", err)
		clients.printPodDiagnostics()
	}

	// Verify that auth is set and actually working
	t.Run("Check that auth is set in sentinel and redis configs", clients.testAuth)

	// Check custom config is set
	t.Run("Check that custom config is behave expected", clients.testCustomConfig)

	// Check that a Redis Statefulset is created and the size of it is the one defined by the
	// Redis Failover definition created before.
	t.Run("Check Redis Statefulset existing and size", clients.testRedisStatefulSet)

	// Check that a Sentinel Deployment is created and the size of it is the one defined by the
	// Redis Failover definition created before.
	t.Run("Check Sentinel Deployment existing and size", clients.testSentinelDeployment)

	// Connect to all the Redis pods and, asking to the Redis running inside them, check
	// that only one of them is the master of the failover.
	t.Run("Check Only One Redis Master", clients.testRedisMaster)

	// Connect to all the Sentinel pods and, asking to the Sentinel running inside them,
	// check that all of them are connected to the same Redis node, and also that that node
	// is the master.
	t.Run("Check Sentinels Checking the Redis Master", clients.testSentinelMonitoring)
}

func (c *clients) testCRCreation(t *testing.T) {
	assert := assert.New(t)

	toCreate := &redisfailoverv1.RedisFailover{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisfailoverv1.RedisFailoverSpec{
			Redis: redisfailoverv1.RedisSettings{
				Replicas:        redisSize,
				ImagePullPolicy: corev1.PullIfNotPresent, // Use locally built image
				Exporter: redisfailoverv1.Exporter{
					Enabled: true,
				},
				CustomConfig: []string{`save ""`},
			},
			Sentinel: redisfailoverv1.SentinelSettings{
				Replicas:        sentinelSize,
				ImagePullPolicy: corev1.PullIfNotPresent, // Use locally built image
				// Sentinel must be explicitly enabled in v4.0.0+ (default is false)
				Enabled: boolPtr(true),
			},
			Auth: redisfailoverv1.AuthSettings{
				SecretPath: authSecretPath,
			},
		},
	}

	c.rfClient.DatabasesV1().RedisFailovers(namespace).Create(context.Background(), toCreate, metav1.CreateOptions{})
	gotRF, err := c.rfClient.DatabasesV1().RedisFailovers(namespace).Get(context.Background(), name, metav1.GetOptions{})

	assert.NoError(err)
	assert.Equal(toCreate.Spec, gotRF.Spec)
}

func (c *clients) testRedisStatefulSet(t *testing.T) {
	assert := assert.New(t)
	redisSS, err := c.k8sClient.AppsV1().StatefulSets(namespace).Get(context.Background(), fmt.Sprintf("rfr-%s", name), metav1.GetOptions{})
	assert.NoError(err)
	assert.Equal(redisSize, int32(redisSS.Status.Replicas))
}

func (c *clients) testSentinelDeployment(t *testing.T) {
	assert := assert.New(t)
	sentinelD, err := c.k8sClient.AppsV1().Deployments(namespace).Get(context.Background(), fmt.Sprintf("rfs-%s", name), metav1.GetOptions{})
	assert.NoError(err)
	assert.Equal(3, int(sentinelD.Status.Replicas))
}

func (c *clients) testRedisMaster(t *testing.T) {
	assert := assert.New(t)
	masters := []string{}

	redisSS, err := c.k8sClient.AppsV1().StatefulSets(namespace).Get(context.Background(), fmt.Sprintf("rfr-%s", name), metav1.GetOptions{})
	assert.NoError(err)

	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(redisSS.Spec.Selector.MatchLabels),
	}

	redisPodList, err := c.k8sClient.CoreV1().Pods(namespace).List(context.Background(), listOptions)

	assert.NoError(err)

	for _, pod := range redisPodList.Items {
		ip := pod.Status.PodIP
		if ok, _ := c.redisClient.IsMaster(ip, "6379", testPass); ok {
			masters = append(masters, ip)
		}
	}

	assert.Equal(1, len(masters), "only one master expected")
}

func (c *clients) testSentinelMonitoring(t *testing.T) {
	assert := assert.New(t)
	masters := []string{}

	sentinelD, err := c.k8sClient.AppsV1().Deployments(namespace).Get(context.Background(), fmt.Sprintf("rfs-%s", name), metav1.GetOptions{})
	assert.NoError(err)

	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(sentinelD.Spec.Selector.MatchLabels),
	}
	sentinelPodList, err := c.k8sClient.CoreV1().Pods(namespace).List(context.Background(), listOptions)
	assert.NoError(err)

	for _, pod := range sentinelPodList.Items {
		ip := pod.Status.PodIP
		master, _, _ := c.redisClient.GetSentinelMonitor(ip)
		masters = append(masters, master)
	}

	for _, masterIP := range masters {
		assert.Equal(masters[0], masterIP, "all master ip monitoring should equal")
	}

	isMaster, err := c.redisClient.IsMaster(masters[0], "6379", testPass)
	assert.NoError(err)
	assert.True(isMaster, "Sentinel should monitor the Redis master")
}

func (c *clients) testAuth(t *testing.T) {
	assert := assert.New(t)

	redisCfg, err := c.k8sClient.CoreV1().ConfigMaps(namespace).Get(context.Background(), fmt.Sprintf("rfr-%s", name), metav1.GetOptions{})
	assert.NoError(err)
	assert.Contains(redisCfg.Data["redis.conf"], "requirepass "+testPass)
	assert.Contains(redisCfg.Data["redis.conf"], "masterauth "+testPass)

	redisSS, err := c.k8sClient.AppsV1().StatefulSets(namespace).Get(context.Background(), fmt.Sprintf("rfr-%s", name), metav1.GetOptions{})
	assert.NoError(err)

	assert.Len(redisSS.Spec.Template.Spec.Containers, 2)
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[1].Name, "REDIS_ADDR")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[1].Value, redisAddr)
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[2].Name, "REDIS_PORT")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[2].Value, "6379")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[3].Name, "REDIS_USER")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[3].Value, "default")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[4].Name, "REDIS_PASSWORD")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[4].ValueFrom.SecretKeyRef.Key, "password")
	assert.Equal(redisSS.Spec.Template.Spec.Containers[1].Env[4].ValueFrom.SecretKeyRef.LocalObjectReference.Name, authSecretPath)
}

func (c *clients) testCustomConfig(t *testing.T) {
	assert := assert.New(t)

	redisSS, err := c.k8sClient.AppsV1().StatefulSets(namespace).Get(context.Background(), fmt.Sprintf("rfr-%s", name), metav1.GetOptions{})
	assert.NoError(err)

	listOptions := metav1.ListOptions{
		LabelSelector: labels.FormatLabels(redisSS.Spec.Selector.MatchLabels),
	}
	redisPodList, err := c.k8sClient.CoreV1().Pods(namespace).List(context.Background(), listOptions)
	assert.NoError(err)

	rClient := rediscli.NewClient(&rediscli.Options{
		Addr:     net.JoinHostPort(redisPodList.Items[0].Status.PodIP, "6379"),
		Password: testPass,
		DB:       0,
	})
	defer rClient.Close()

	result := rClient.ConfigGet(context.TODO(), "save")
	assert.NoError(result.Err())

	values, err := result.Result()
	assert.NoError(err)

	assert.Len(values, 2)
	assert.Empty(values[1])
}
