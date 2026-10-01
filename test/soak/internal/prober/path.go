package prober

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/saremox/redis-operator/test/soak/internal/config"
)

const PathRFRM = "rfrm"

// Path is one way of reaching an instance's master.
type Path struct {
	Name      string
	NewClient func(Client) *redis.Client
}

// MasterService reaches the master through the rfrm-<name> Service.
func MasterService(in config.Instance, timeout time.Duration) Path {
	addr := net.JoinHostPort(fmt.Sprintf("rfrm-%s.%s.svc", in.Name, in.Namespace), strconv.Itoa(in.Port))
	return Path{
		Name: PathRFRM,
		NewClient: func(c Client) *redis.Client {
			return redis.NewClient(&redis.Options{
				Addr:                  addr,
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

// maxRetries turns off go-redis's retries of READONLY, LOADING, MASTERDOWN
// and network errors, which would hide exactly what the probes measure,
// except for the Retrying style, which keeps the default.
func maxRetries(c Client) int {
	if c == Retrying {
		return 0
	}
	return -1
}
