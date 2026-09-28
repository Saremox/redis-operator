package prober

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/redis/go-redis/v9"
)

// Result labels. The set is fixed so the probe_total cardinality stays
// bounded.
const (
	ResultOK         = "ok"
	ResultTimeout    = "timeout"
	ResultRefused    = "refused"
	ResultReadOnly   = "readonly"
	ResultLoading    = "loading"
	ResultAuth       = "auth"
	ResultOOM        = "oom"
	ResultMasterDown = "masterdown"
	ResultNoReplicas = "noreplicas"
	ResultDNS        = "dns"
	ResultOther      = "other"
)

var errorCodes = map[string]string{
	"READONLY":   ResultReadOnly,
	"LOADING":    ResultLoading,
	"NOAUTH":     ResultAuth,
	"WRONGPASS":  ResultAuth,
	"OOM":        ResultOOM,
	"MASTERDOWN": ResultMasterDown,
	"NOREPLICAS": ResultNoReplicas,
}

// Classify maps a probe error to a result label. Server errors are matched
// by their leading error code only: Redis and Valkey word the messages
// differently.
func Classify(err error) string {
	if err == nil {
		return ResultOK
	}
	var redisErr redis.Error
	if errors.As(err, &redisErr) {
		code, _, _ := strings.Cut(redisErr.Error(), " ")
		if r, ok := errorCodes[code]; ok {
			return r
		}
		return ResultOther
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ResultDNS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return ResultRefused
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return ResultTimeout
	}
	return ResultOther
}
