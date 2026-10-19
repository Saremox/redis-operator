package service

import (
	"net"

	corev1 "k8s.io/api/core/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// SentinelAddrs holds the addresses that the Sentinels of a RedisFailover
// know and that no pod of the RedisFailover has. Each map gives the IPs of the
// Sentinels that know the address. Each IP has the form of net.IP.String.
type SentinelAddrs struct {
	// Peers holds the peer IPs that no Sentinel pod has.
	Peers map[string][]string
	// Masters holds the master IPs that no Redis pod has.
	Masters map[string][]string
	// Own holds the IPs of the Sentinel and Redis pods.
	Own map[string]bool
	// Unread holds the IPs of the running Sentinels that did not answer. A
	// Sentinel in deletion is not in it, because on a lost node it never
	// answers.
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
	addrs := SentinelAddrs{Peers: map[string][]string{}, Masters: map[string][]string{}, Own: map[string]bool{}}
	for ip := range ownSentinels {
		addrs.Own[ip] = true
	}
	for ip := range ownRedises {
		addrs.Own[ip] = true
	}
	for _, sp := range sentinels.Items {
		if sp.Status.Phase != corev1.PodRunning {
			continue
		}
		sip := sp.Status.PodIP
		group, err := r.redisClient.GetSentinelPeers(sip)
		if err != nil {
			if sp.DeletionTimestamp == nil {
				addrs.Unread = append(addrs.Unread, sip)
			}
			continue
		}
		for _, peer := range group.Peers {
			if peer = canonicalIP(peer); !ownSentinels[peer] {
				addrs.Peers[peer] = append(addrs.Peers[peer], sip)
			}
		}
		if master := canonicalIP(group.Master); !ownRedises[master] {
			addrs.Masters[master] = append(addrs.Masters[master], sip)
		}
	}
	return addrs, nil
}

// canonicalIP returns the standard form of ip, so that two forms of one IPv6
// address compare equal. A value that is no IP stays as it is.
func canonicalIP(ip string) string {
	if addr := net.ParseIP(ip); addr != nil {
		return addr.String()
	}
	return ip
}

// podIPs returns the IPs of the pods. A dual-stack pod has two IPs.
func podIPs(pods *corev1.PodList) map[string]bool {
	ips := make(map[string]bool, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Status.PodIP != "" {
			ips[canonicalIP(pod.Status.PodIP)] = true
		}
		for _, ip := range pod.Status.PodIPs {
			if ip.IP != "" {
				ips[canonicalIP(ip.IP)] = true
			}
		}
	}
	return ips
}
