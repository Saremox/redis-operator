//go:build integration

package redisfailover_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	rediscli "github.com/go-redis/redis/v8"
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

// The tests in this package run in parallel, so this test has its own
// namespace, name and Secret.
const (
	ommNamespace      = "rf-integration-tests-operator-managed"
	ommName           = "testing-omm"
	ommRedisSize      = int32(2)
	ommAuthSecretPath = "redis-auth-omm"
	ommTestPass       = "test-pass-omm"
)

// ommClients is the `clients` helper of creation_test.go for the namespace
// of this test.
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

func (c *ommClients) cleanup(cancel context.CancelFunc) {
	c.k8sClient.CoreV1().Namespaces().Delete(context.Background(), ommNamespace, metav1.DeleteOptions{})
	cancel()
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

// podUIDs returns the UID of each pod that matches labelSelector, by pod
// name. A new StatefulSet pod has the same name but a new UID, so only the
// UID shows a replacement.
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
// different UID in the cluster, or until the timeout.
func (c *ommClients) waitForAllPodsRecreated(labelSelector string, before map[string]types.UID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	recreated := 0
	for time.Now().Before(deadline) {
		current, err := c.podUIDs(labelSelector)
		if err != nil {
			return err
		}

		recreated = 0
		for podName, oldUID := range before {
			if newUID, ok := current[podName]; ok && newUID != oldUID {
				recreated++
			}
		}
		if recreated == len(before) && len(current) == len(before) {
			return nil
		}

		time.Sleep(5 * time.Second)
	}
	// The count shows a slow rollout apart from a rollout that stopped.
	return fmt.Errorf("timed out waiting for all pods matching %q to be recreated: %d of %d recreated",
		labelSelector, recreated, len(before))
}

// mastersNow returns the number of pods that answer as master. During a
// rollout, 0 is a valid result and not an error.
func (c *ommClients) mastersNow(labelSelector string) int {
	pods, err := c.k8sClient.CoreV1().Pods(ommNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return 0
	}
	n := 0
	for _, pod := range pods.Items {
		if pod.Status.PodIP == "" {
			continue
		}
		if ok, _ := c.redisClient.IsMaster(pod.Status.PodIP, "6379", ommTestPass); ok {
			n++
		}
	}
	return n
}

// masterAvailability is the result of sampleMasterAvailability. A single
// sample without a master can be a promotion in progress. longestOutage is
// the longest series of such samples, so it shows how long the clients had
// no master.
type masterAvailability struct {
	samples       int
	masterless    int
	longestOutage int
	interval      time.Duration
}

func (m masterAvailability) outage() time.Duration {
	return time.Duration(m.longestOutage) * m.interval
}

// sampleMasterAvailability counts the masters at each interval until stop
// closes, and then sends the result to done.
func (c *ommClients) sampleMasterAvailability(labelSelector string, interval time.Duration, stop <-chan struct{}, done chan<- masterAvailability) {
	result := masterAvailability{interval: interval}
	run := 0
	for {
		select {
		case <-stop:
			done <- result
			return
		default:
		}

		result.samples++
		if c.mastersNow(labelSelector) == 0 {
			result.masterless++
			run++
			if run > result.longestOutage {
				result.longestOutage = run
			}
		} else {
			run = 0
		}
		time.Sleep(interval)
	}
}

func (c *ommClients) masterClient(ip string) *rediscli.Client {
	return rediscli.NewClient(&rediscli.Options{Addr: net.JoinHostPort(ip, "6379"), Password: ommTestPass, MaxRetries: -1})
}

// writeToMaster writes a new key to the master at each interval until stop
// closes, and then sends the keys that a master acknowledged to done. A
// write to a master that became a replica gets READONLY and is not counted.
func (c *ommClients) writeToMaster(labelSelector string, interval time.Duration, stop <-chan struct{}, done chan<- []string) {
	var acked []string
	for i := 0; ; i++ {
		select {
		case <-stop:
			done <- acked
			return
		default:
		}
		if master, err := c.onlyMaster(labelSelector); err == nil {
			key := fmt.Sprintf("rollout-%d", i)
			client := c.masterClient(master)
			if client.Set(context.Background(), key, i, 0).Err() == nil {
				acked = append(acked, key)
			}
			_ = client.Close()
		}
		time.Sleep(interval)
	}
}

// missingKeys returns the number of keys that the master does not have.
func (c *ommClients) missingKeys(labelSelector string, keys []string) (int, error) {
	master, err := c.onlyMaster(labelSelector)
	if err != nil {
		return 0, err
	}
	client := c.masterClient(master)
	defer func() { _ = client.Close() }()
	missing := 0
	for _, key := range keys {
		n, err := client.Exists(context.Background(), key).Result()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			missing++
		}
	}
	return missing, nil
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

// TestRedisFailoverOperatorManagedModeRollout tests a rollout in
// operator-managed mode (#161). In this mode no Sentinel exists, so the
// Sentinel check before the master replacement must not run. Otherwise the
// rollout never replaces the master, and the RedisFailover stays NotHealthy.
// The test also measures how long the RedisFailover has no master during the
// rollout, and checks that no acknowledged write is lost.
//
// The other test in this package enables Sentinel. The e2e-sentinel-free job
// in e2e.yml also tests the default mode, but this is the only test of a
// rollout in that mode.
func TestRedisFailoverOperatorManagedModeRollout(t *testing.T) {
	// The operators of the tests use different namespaces, leases and
	// Secrets, so the tests can run in parallel.
	t.Parallel()

	require := require.New(t)

	errC := make(chan error)

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
	}

	flags := &utils.CMDFlags{
		KubeConfig:  kubeconfig,
		Development: true,
	}

	k8sClient, customClient, metadataClient, err := utils.CreateKubernetesClients(flags)
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
	// The resync is far longer than the rollout's timeout, so only pod events
	// can drive the rollout's steps.
	redisfailoverOperator, err := redisfailover.New(redisfailover.Config{SyncInterval: 600, SupportedNamespacesRegex: "^" + ommNamespace + "$"}, k8sservice, k8sClient, metadataClient, ommNamespace, redisClient, metrics.Dummy, log.Dummy)
	require.NoError(err)

	// Run stops only when its context is canceled.
	runCtx, cancelRun := context.WithCancel(context.Background())
	go func() {
		errC <- redisfailoverOperator.Run(runCtx)
	}()
	defer c.cleanup(cancelRun)

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
				// Operator-managed mode, set explicitly. It is also the
				// default.
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

		// The pod UIDs and the master count at the end do not show a
		// period without a master. Only samples during the rollout show it.
		availStop := make(chan struct{})
		availDone := make(chan masterAvailability, 1)
		go c.sampleMasterAvailability(redisLabelSelector, 500*time.Millisecond, availStop, availDone)
		defer func() {
			close(availStop)
			avail := <-availDone
			t.Logf("master availability during rollout: %d samples, %d masterless, longest masterless run %d samples (%s)",
				avail.samples, avail.masterless, avail.longestOutage, avail.outage())

			// A replica gets the master role before the old master pod
			// stops, so a master exists at all times. One sample of
			// tolerance is for a sample during the promotion. A delete of
			// the master before the election measured up to 118 samples.
			assert.LessOrEqual(avail.longestOutage, 1,
				"cluster was left without a master for %s (%d consecutive samples) during the rollout; "+
					"the master role must be handed over before the master pod is replaced",
				avail.outage(), avail.longestOutage)
		}()

		// FAILOVER pauses the writes until the new master has them all, so
		// each write that a master acknowledged must be on the last master.
		writeStop := make(chan struct{})
		writeDone := make(chan []string, 1)
		go c.writeToMaster(redisLabelSelector, 50*time.Millisecond, writeStop, writeDone)
		defer func() {
			close(writeStop)
			acked := <-writeDone
			missing, err := c.missingKeys(redisLabelSelector, acked)
			assert.NoError(err)
			t.Logf("acknowledged writes during rollout: %d, missing after rollout: %d", len(acked), missing)
			assert.NotEmpty(acked)
			assert.Zero(missing, "the rollout lost acknowledged writes")
		}()

		// generateRedisStatefulSet copies PodAnnotations into the pod
		// template, so this change gives a new update revision. The
		// StatefulSet uses OnDelete, so only UpdateRedisesPods replaces the
		// pods.
		live, err := c.rfClient.DatabasesV1().RedisFailovers(ommNamespace).Get(context.Background(), ommName, metav1.GetOptions{})
		require.NoError(err)
		live.Spec.Redis.PodAnnotations = map[string]string{"rollout-test": "1"}
		_, err = c.rfClient.DatabasesV1().RedisFailovers(ommNamespace).Update(context.Background(), live, metav1.UpdateOptions{})
		require.NoError(err)

		// The rollout must also replace the master without a Sentinel check.
		// It replaces one pod at a time, and each new pod does a full sync
		// before the next replacement. On a slow CI runner this took about
		// 5 minutes.
		if err := c.waitForAllPodsRecreated(redisLabelSelector, before, 10*time.Minute); err != nil {
			t.Fatalf("rollout never completed: %v", err)
		}

		if err := c.waitForPodsReady(redisLabelSelector, int(ommRedisSize), 3*time.Minute); err != nil {
			t.Fatalf("redis pods never became ready again after rollout: %v", err)
		}

		_, err = c.onlyMaster(redisLabelSelector)
		assert.NoError(err, "exactly one master must be elected again after the rollout, with no Sentinel involved")
	})
}
