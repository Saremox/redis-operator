package prober

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type serverError string

func (e serverError) Error() string { return string(e) }
func (serverError) RedisError()     {}

func TestClassifyServerErrors(t *testing.T) {
	cases := map[string]string{
		"READONLY You can't write against a read only replica.":                            ResultReadOnly,
		"READONLY You can't write against a read only replica":                             ResultReadOnly,
		"LOADING Redis is loading the dataset in memory":                                   ResultLoading,
		"LOADING Valkey is loading the dataset in memory":                                  ResultLoading,
		"NOAUTH Authentication required.":                                                  ResultAuth,
		"WRONGPASS invalid username-password pair or user is disabled.":                    ResultAuth,
		"OOM command not allowed when used memory > 'maxmemory'.":                          ResultOOM,
		"MASTERDOWN Link with MASTER is down and replica-serve-stale-data is set to 'no'.": ResultMasterDown,
		"NOREPLICAS Not enough good replicas to write.":                                    ResultNoReplicas,
		"ERR unknown command 'FOO'":                                                        ResultOther,
		// The code decides, not the text.
		"ERR READONLY in the message": ResultOther,
		"READONLYX not a known code":  ResultOther,
	}
	for msg, want := range cases {
		if got := Classify(serverError(msg)); got != want {
			t.Errorf("Classify(%q) = %q, want %q", msg, got, want)
		}
		wrapped := fmt.Errorf("probe: %w", serverError(msg))
		if got := Classify(wrapped); got != want {
			t.Errorf("Classify(wrapped %q) = %q, want %q", msg, got, want)
		}
	}
}

func TestClassifyNonServerErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ResultOK},
		{context.DeadlineExceeded, ResultTimeout},
		{&net.OpError{Op: "read", Err: timeoutError{}}, ResultTimeout},
		{&net.DNSError{Err: "no such host", Name: "rfrm-x.ns.svc", IsNotFound: true}, ResultDNS},
		{&net.DNSError{Err: "i/o timeout", Name: "rfrm-x.ns.svc", IsTimeout: true}, ResultDNS},
		{io.EOF, ResultClosed},
		{fmt.Errorf("probe: %w", io.ErrUnexpectedEOF), ResultClosed},
		{&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, ResultClosed},
		{&net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)}, ResultClosed},
		{errors.New("EOF"), ResultOther},
		{redis.ErrClosed, ResultOther},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// The errors go-redis actually returns must classify the same way.
func TestClassifyClientErrors(t *testing.T) {
	newClient := func(addr string) *redis.Client {
		return redis.NewClient(&redis.Options{
			Addr: addr, MaxRetries: -1, DialTimeout: 200 * time.Millisecond,
			ReadTimeout: 200 * time.Millisecond, ContextTimeoutEnabled: true,
		})
	}
	ping := func(addr string) error {
		c := newClient(addr)
		defer func() { _ = c.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return c.Set(ctx, "k", "v", 0).Err()
	}

	t.Run("server error", func(t *testing.T) {
		s := miniredis.RunT(t)
		s.SetError("READONLY You can't write against a read only replica.")
		if got := Classify(ping(s.Addr())); got != ResultReadOnly {
			t.Errorf("got %q", got)
		}
	})

	t.Run("refused", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		if got := Classify(ping(addr)); got != ResultRefused {
			t.Errorf("got %q", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
			}
		}()
		if got := Classify(ping(l.Addr().String())); got != ResultTimeout {
			t.Errorf("got %q", got)
		}
	})

	t.Run("closed", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_, _ = c.Read(make([]byte, 64))
				_ = c.Close()
			}
		}()
		if got := Classify(ping(l.Addr().String())); got != ResultClosed {
			t.Errorf("got %q", got)
		}
	})

	t.Run("dns", func(t *testing.T) {
		if got := Classify(ping("rfrm-missing.invalid:6379")); got != ResultDNS {
			t.Errorf("got %q", got)
		}
	})
}
