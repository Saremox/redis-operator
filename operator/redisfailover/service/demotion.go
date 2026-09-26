package service

import (
	"context"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/service/k8s"
	"github.com/saremox/redis-operator/service/redis"
)

const endpointPollInterval = 200 * time.Millisecond

// Option configures optional behaviour shared by RedisFailoverChecker and
// RedisFailoverHealer.
type Option func(*options)

type options struct {
	disconnector ClientDisconnector
}

func applyOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithClientDisconnector sets what closes a pod's client connections when
// its role label moves from master to slave. Without it they are left open.
func WithClientDisconnector(d ClientDisconnector) Option {
	return func(o *options) {
		o.disconnector = d
	}
}

// ClientDisconnector closes the client connections of a pod that has stopped
// being the master, so clients reconnect through the master Service.
type ClientDisconnector interface {
	DisconnectDemoted(rf *redisfailoverv1.RedisFailover, pod corev1.Pod, port, password string)
}

// setSlaveLabel gives pod the slave role label if it doesn't already have it,
// and disconnects its clients if the label it replaces was master. Only then
// does it mark the pod evictable.
func setSlaveLabel(k8sService k8s.Services, o options, rf *redisfailoverv1.RedisFailover, pod corev1.Pod, port, password string) error {
	previousRole := pod.Labels[redisRoleLabelKey]
	if previousRole != redisRoleLabelSlave {
		if err := k8sService.UpdatePodLabels(rf.Namespace, pod.Name, generateRedisSlaveRoleLabel()); err != nil {
			return err
		}
		if previousRole == redisRoleLabelMaster && o.disconnector != nil {
			o.disconnector.DisconnectDemoted(rf, pod, port, password)
		}
	}
	return applyMasterEvictionAnnotation(k8sService, rf, pod, false)
}

type endpointAwareDisconnector struct {
	kubeClient  kubernetes.Interface
	redisClient redis.Client
	logger      log.Logger
	timeout     time.Duration
	grace       time.Duration
	pending     sync.Map
}

// NewClientDisconnector returns a ClientDisconnector that works in the
// background: it waits (up to timeout) for the pod to leave the master
// Service's EndpointSlices, then for grace so kube-proxy can catch up, and
// only then disconnects. Disconnecting earlier sends clients straight back.
func NewClientDisconnector(kubeClient kubernetes.Interface, redisClient redis.Client, logger log.Logger, timeout, grace time.Duration) ClientDisconnector {
	return &endpointAwareDisconnector{
		kubeClient:  kubeClient,
		redisClient: redisClient,
		logger:      logger,
		timeout:     timeout,
		grace:       grace,
	}
}

func (d *endpointAwareDisconnector) DisconnectDemoted(rf *redisfailoverv1.RedisFailover, pod corev1.Pod, port, password string) {
	key := rf.Namespace + "/" + pod.Name
	if _, busy := d.pending.LoadOrStore(key, struct{}{}); busy {
		return
	}
	go func() {
		defer d.pending.Delete(key)
		d.disconnect(rf, pod, port, password)
	}()
}

func (d *endpointAwareDisconnector) disconnect(rf *redisfailoverv1.RedisFailover, pod corev1.Pod, port, password string) {
	logger := d.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	service := GetRedisMasterName(rf)
	if err := d.waitForEndpointRemoval(rf.Namespace, service, pod.Status.PodIP); err != nil {
		logger.Warningf("Pod %s may still be behind Service %s: %v", pod.Name, service, err)
	}
	time.Sleep(d.grace)

	logger.Infof("Pod %s is no longer the master, disconnecting its clients", pod.Name)
	if err := d.redisClient.DisconnectClients(pod.Status.PodIP, port, password); err != nil {
		logger.Warningf("Could not disconnect clients of demoted pod %s: %v", pod.Name, err)
	}
}

func (d *endpointAwareDisconnector) waitForEndpointRemoval(namespace, service, ip string) error {
	selector := metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + service}
	return wait.PollUntilContextTimeout(context.Background(), endpointPollInterval, d.timeout, true, func(ctx context.Context) (bool, error) {
		endpointSlices, err := d.kubeClient.DiscoveryV1().EndpointSlices(namespace).List(ctx, selector)
		if err != nil {
			return false, err
		}
		for _, endpointSlice := range endpointSlices.Items {
			for _, endpoint := range endpointSlice.Endpoints {
				if slices.Contains(endpoint.Addresses, ip) {
					return false, nil
				}
			}
		}
		return true, nil
	})
}
