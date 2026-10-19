package service

import (
	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// SentinelAddrs holds the addresses that the Sentinels of a RedisFailover
// know and that no pod of the RedisFailover has. Each map gives the IPs of the
// Sentinels that know the address.
type SentinelAddrs struct {
	// Peers holds the peer IPs that no Sentinel pod has.
	Peers map[string][]string
	// Masters holds the master IPs that no Redis pod has.
	Masters map[string][]string
	// Unread holds the IPs of the running Sentinels that did not answer.
	Unread []string
}

// GetUnknownSentinelAddrs reads the group of each running Sentinel, also of a
// Sentinel in deletion. A pod in each phase, also in deletion, is a pod of the
// RedisFailover. The operator does not list other pods, so an unknown IP can
// also be the IP of a deleted pod of the RedisFailover.
func (r *RedisFailoverChecker) GetUnknownSentinelAddrs(rf *redisfailoverv1.RedisFailover) (SentinelAddrs, error) {
	sentinels, err := r.k8sService.GetDeploymentPods(rf.Namespace, GetSentinelName(rf))
	if err != nil {
		return SentinelAddrs{}, err
	}
	redises, err := r.k8sService.GetStatefulSetPods(rf.Namespace, GetRedisName(rf))
	if err != nil {
		return SentinelAddrs{}, err
	}
	ownSentinels, ownRedises := podIPs(sentinels), podIPs(redises)
	addrs := SentinelAddrs{Peers: map[string][]string{}, Masters: map[string][]string{}}
	for _, sp := range sentinels.Items {
		if sp.Status.Phase != corev1.PodRunning {
			continue
		}
		sip := sp.Status.PodIP
		group, err := r.redisClient.GetSentinelPeers(sip)
		if err != nil {
			addrs.Unread = append(addrs.Unread, sip)
			continue
		}
		for _, peer := range group.Peers {
			if !ownSentinels[peer] {
				addrs.Peers[peer] = append(addrs.Peers[peer], sip)
			}
		}
		if !ownRedises[group.Master] {
			addrs.Masters[group.Master] = append(addrs.Masters[group.Master], sip)
		}
	}
	return addrs, nil
}

// podIPs returns the IPs of the pods.
func podIPs(pods *corev1.PodList) map[string]bool {
	ips := make(map[string]bool, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Status.PodIP != "" {
			ips[pod.Status.PodIP] = true
		}
	}
	return ips
}
