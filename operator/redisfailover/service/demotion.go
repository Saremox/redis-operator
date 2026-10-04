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

// WithClientDisconnector sets what closes the client connections of a pod
// that stops being the master: when its role label changes from master to
// slave, or when SetMasterOnAll demotes a master without the master label.
// Without it, the connections stay open.
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

// setSlaveLabel gives the pod the slave role label, and disconnects its
// clients when the old label was master. It marks the pod evictable only
// after that.
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
	mu          sync.Mutex
	// runs holds the pods with a disconnect in progress. The value is true
	// when the pod was demoted again during the run. A failed run then runs
	// again, because the pod can answer only after the first attempt.
	runs map[string]bool
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
		runs:        map[string]bool{},
	}
}

func (d *endpointAwareDisconnector) DisconnectDemoted(rf *redisfailoverv1.RedisFailover, pod corev1.Pod, port, password string) {
	key := rf.Namespace + "/" + pod.Name
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, busy := d.runs[key]; busy {
		d.runs[key] = true
		return
	}
	d.runs[key] = false
	go func() {
		for {
			failed := d.disconnect(rf, pod, port, password) != nil
			d.mu.Lock()
			again := failed && d.runs[key]
			if again {
				d.runs[key] = false
			} else {
				delete(d.runs, key)
			}
			d.mu.Unlock()
			if !again {
				return
			}
		}
	}()
}

func (d *endpointAwareDisconnector) disconnect(rf *redisfailoverv1.RedisFailover, pod corev1.Pod, port, password string) error {
	logger := d.logger.WithField("redisfailover", rf.Name).WithField("namespace", rf.Namespace)
	service := GetRedisMasterName(rf)
	if err := d.waitForEndpointRemoval(rf.Namespace, service, pod.Status.PodIP); err != nil {
		logger.Warningf("Pod %s may still be behind Service %s: %v", pod.Name, service, err)
	}
	time.Sleep(d.grace)

	logger.Infof("Pod %s is no longer the master, disconnecting its clients", pod.Name)
	err := d.redisClient.DisconnectClients(pod.Status.PodIP, port, password)
	if err != nil {
		logger.Warningf("Could not disconnect clients of demoted pod %s: %v", pod.Name, err)
	}
	return err
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
