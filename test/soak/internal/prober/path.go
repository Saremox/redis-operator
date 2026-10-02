package prober

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
)

const (
	PathRFRM     = "rfrm"
	PathSentinel = "sentinel"
	// PathRFRS reaches a bootstrapping instance's pods, which are all
	// replicas, through the rfrs-<name> Service.
	PathRFRS = "rfrs"

	sentinelPort       = 26379
	sentinelMasterName = "mymaster"
)

// Path is one way to reach an instance.
type Path struct {
	Name string
	// ReadKey makes the path read-only: probes only GET it.
	ReadKey   string
	NewClient func(Client) *redis.Client
}

// Names returns the paths an instance is probed through, following its
// current mode: a bootstrapping instance through rfrs, read-only; one with
// Sentinel through Sentinel and rfrm; one without through rfrm.
func Names(in config.Instance, sentinel bool) []string {
	switch {
	case in.Bootstrap != nil:
		return []string{PathRFRS}
	case sentinel:
		return []string{PathSentinel, PathRFRM}
	}
	return []string{PathRFRM}
}

// NewPath returns the path of that name.
func NewPath(name string, in config.Instance, timeout time.Duration, a *auth.Source) Path {
	switch name {
	case PathSentinel:
		return Sentinel(in, timeout, a)
	case PathRFRS:
		p := service("rfrs", name, in, timeout, a)
		// The source's pooled rfrm prober writes it every probe.
		p.ReadKey = fmt.Sprintf("soak:%s:%s:%s:seq", in.Bootstrap.Source, PathRFRM, Pooled)
		return p
	}
	return MasterService(in, timeout, a)
}

// MasterService reaches the master through the rfrm-<name> Service.
func MasterService(in config.Instance, timeout time.Duration, a *auth.Source) Path {
	return service("rfrm", PathRFRM, in, timeout, a)
}

func service(prefix, name string, in config.Instance, timeout time.Duration, a *auth.Source) Path {
	addr := net.JoinHostPort(fmt.Sprintf("%s-%s.%s.svc", prefix, in.Name, in.Namespace), strconv.Itoa(in.Port))
	return Path{
		Name: name,
		NewClient: func(c Client) *redis.Client {
			return redis.NewClient(&redis.Options{
				Addr:                  addr,
				CredentialsProvider:   credentials(c, a),
				DialTimeout:           timeout,
				ReadTimeout:           timeout,
				WriteTimeout:          timeout,
				PoolTimeout:           timeout,
				ContextTimeoutEnabled: true,
				MaxRetries:            maxRetries(c),
			})
		},
	}
}

// Sentinel reaches the master as a Sentinel-aware client does: it asks the
// Sentinels behind rfs-<name> for the master address and follows their
// failover announcements. The Sentinels need no password; the master does.
func Sentinel(in config.Instance, timeout time.Duration, a *auth.Source) Path {
	addr := net.JoinHostPort(fmt.Sprintf("rfs-%s.%s.svc", in.Name, in.Namespace), strconv.Itoa(sentinelPort))
	return Path{
		Name: PathSentinel,
		NewClient: func(c Client) *redis.Client {
			return redis.NewFailoverClient(&redis.FailoverOptions{
				MasterName:            sentinelMasterName,
				SentinelAddrs:         []string{addr},
				CredentialsProvider:   credentials(c, a),
				DialTimeout:           timeout,
				ReadTimeout:           timeout,
				WriteTimeout:          timeout,
				PoolTimeout:           timeout,
				ContextTimeoutEnabled: true,
				MaxRetries:            maxRetries(c),
			})
		},
	}
}

// credentials authenticates every new connection with the Secret's current
// password, except for Follower: its client is made for one password and
// replaced when the Secret changes.
func credentials(c Client, a *auth.Source) func() (string, string) {
	if c == Follower {
		return auth.Fixed(a.Password())
	}
	return a.Provider()
}

// maxRetries turns off go-redis's retries of READONLY, LOADING, MASTERDOWN
// and network errors, which would hide exactly what the probes measure,
// except for the Retrying style, which keeps the default.
func maxRetries(c Client) int {
	if c == Retrying {
		return 0
	}
	return -1
}
