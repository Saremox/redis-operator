package observer

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
)

func newPod(p *corev1.Pod) pod {
	return pod{Name: p.Name, UID: string(p.UID), IP: p.Status.PodIP, Ready: Ready(p), OOMKills: oomKills(p)}
}

// Ready reports whether a pod is Ready and not in deletion.
func Ready(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

var errNoIP = errors.New("no pod IP")

// Client connects to one pod without retries, so that each failure shows.
// creds gives the password of each new connection.
func Client(addr string, creds func() (string, string), timeout time.Duration) *redis.Client {
	return redis.NewClient(options(addr, creds, timeout))
}

func options(addr string, creds func() (string, string), timeout time.Duration) *redis.Options {
	return &redis.Options{
		Addr:                  addr,
		CredentialsProvider:   creds,
		DialTimeout:           timeout,
		ReadTimeout:           timeout,
		WriteTimeout:          timeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
	}
}

// client connects to the pod of r with the current password of a, or with no
// password for a nil a. r records the dials.
func (o *Observer) client(r *read, port int, a *auth.Source) *redis.Client {
	opt := options(net.JoinHostPort(r.ip, strconv.Itoa(port)), a.Provider(), o.timeout)
	dial := o.dial
	if dial == nil {
		dial = redis.NewDialer(opt)
	}
	opt.Dialer = r.dialer(dial)
	return redis.NewClient(opt)
}

// redisInfo sends INFO to the pod, and CONFIG GET keys.
func (o *Observer) redisInfo(ctx context.Context, p *redisPod, port int, keys []string) {
	if p.IP == "" {
		p.err, p.configErr = errNoIP, errNoIP
		return
	}
	r := o.startRead("redis", p.Name, p.IP)
	defer func() {
		err := p.err
		if err == nil {
			err = p.configErr
		}
		o.endRead(r, err)
	}()
	c := o.client(r, port, o.auth)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s, err := c.Info(ctx).Result()
	if err == nil {
		p.info = ParseInfo(s)
	}
	p.err = err
	if len(keys) > 0 {
		p.config, p.configErr = ConfigGet(ctx, c, keys)
	}
}

// ConfigGet reads several keys with one CONFIG GET, which Redis and Valkey
// accept from 7.0 on.
func ConfigGet(ctx context.Context, c *redis.Client, keys []string) (map[string]string, error) {
	args := make([]any, 0, 2+len(keys))
	args = append(args, "config", "get")
	for _, k := range keys {
		args = append(args, k)
	}
	cmd := redis.NewMapStringStringCmd(ctx, args...)
	_ = c.Process(ctx, cmd)
	return cmd.Result()
}

// sentinelInfo asks a Sentinel for the master's address, as clients do,
// and for SENTINEL MASTER mymaster's fields.
func (o *Observer) sentinelInfo(ctx context.Context, p *sentinelPod) {
	if p.IP == "" {
		p.err = errNoIP
		return
	}
	r := o.startRead("sentinel", p.Name, p.IP)
	defer func() { o.endRead(r, p.err) }()
	c := o.client(r, sentinelPort, nil)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	addr, err := c.Do(ctx, "SENTINEL", "get-master-addr-by-name", sentinelMasterName).StringSlice()
	if err == nil && len(addr) != 2 {
		err = errors.New("no master known")
	}
	if err != nil {
		p.err = err
		return
	}
	p.master = net.JoinHostPort(addr[0], addr[1])
	p.fields, p.err = SentinelMaster(ctx, c)
}

// SentinelMaster returns SENTINEL MASTER mymaster's fields.
func SentinelMaster(ctx context.Context, c *redis.Client) (map[string]string, error) {
	cmd := redis.NewMapStringStringCmd(ctx, "SENTINEL", "MASTER", sentinelMasterName)
	_ = c.Process(ctx, cmd)
	return cmd.Result()
}

// masterPosition returns the replication ID and offset of the source's
// master.
func (o *Observer) masterPosition(ctx context.Context) (_ string, _ int64, err error) {
	src := o.source
	v := src.view.Load()
	if v == nil || v.masterIP == "" {
		return "", 0, errNoMaster
	}
	r := o.startRead("source", v.master, v.masterIP)
	defer func() { o.endRead(r, err) }()
	c := o.client(r, src.in.Port, src.auth)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	s, err := c.Info(ctx, "replication").Result()
	if err != nil {
		return "", 0, err
	}
	i := info(ParseInfo(s))
	return i["master_replid"], i.int("master_repl_offset"), nil
}
