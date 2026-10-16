package redis

import (
	"errors"
	"io"
	"net"
	"net/url"
	"strings"

	rediscli "github.com/go-redis/redis/v8"
	"github.com/saremox/redis-operator/metrics"
)

// errNoIP is the error for a pod that has no IP yet. An empty host makes the
// client dial the loopback address of the operator and send the password there.
// IsUnreachableError treats it as an unreachable node.
var errNoIP = errors.New("pod has no IP yet")

func getRedisError(err error) string {
	if strings.Contains(err.Error(), "NOAUTH") {
		return metrics.NOAUTH
	} else if strings.Contains(err.Error(), "WRONGPASS") {
		return metrics.WRONG_PASSWORD_USED
	} else if strings.Contains(err.Error(), "NOPERM") {
		return metrics.NOPERM
	} else if strings.Contains(err.Error(), "i/o timeout") {
		return metrics.IO_TIMEOUT
	} else if strings.Contains(err.Error(), "connection refused") {
		return metrics.CONNECTION_REFUSED
	} else {
		return "MISC"
	}
}

// IsAuthError reports whether Redis refused the password (WRONGPASS), needs
// a password (NOAUTH), or got a password while it has none.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "WRONGPASS") ||
		strings.Contains(msg, "NOAUTH") ||
		IsNoPasswordError(err)
}

// ReplyHasPrefix reports whether the error reply of Redis in err starts with
// prefix. The client methods wrap the reply, so a check of err.Error() does
// not see the reply at the start.
func ReplyHasPrefix(err error, prefix string) bool {
	return err != nil && strings.HasPrefix(replyText(err), prefix)
}

// replyText returns the error reply of Redis in err, or the text of err when
// err holds no reply.
func replyText(err error) string {
	if reply, ok := errors.AsType[rediscli.Error](err); ok {
		return reply.Error()
	}
	return err.Error()
}

// IsNoPasswordError reports whether Redis was given a password while it has
// none configured.
func IsNoPasswordError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "without any password configured")
}

// IsUnreachableError reports whether the client could not reach the Redis
// node, for example after a dial error, a timeout or a reset. The callers skip
// such a node, but return the errors of a command, for example a bad config
// value or a wrong password.
func IsUnreachableError(err error) bool {
	if err == nil {
		return false
	}
	// A failed call to the Kubernetes API gives a *url.Error, also for a
	// timeout. It does not show that the node is unreachable.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return false
	}

	// A node that stops closes its connections, which gives EOF.
	if errors.Is(err, errNoIP) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	msg := err.Error()
	return strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "connection reset")
}
