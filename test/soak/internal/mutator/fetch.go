package mutator

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/maxmem"
	"github.com/saremox/redis-operator/test/soak/internal/observer"
)

// fetchOpts are what a convergence check reads beyond the objects.
type fetchOpts struct {
	// password is checked on every redis pod.
	password *string
	// sentinelMaster reads SENTINEL MASTER mymaster from every Sentinel.
	sentinelMaster bool
	// sentinelObjects reads the Sentinel Deployment, Service and ConfigMap
	// whatever the mode.
	sentinelObjects bool
	// servers reads INFO server from every redis and Sentinel pod.
	servers bool
}

func (m *Mutator) fetch(ctx context.Context, opts fetchOpts) (state, error) {
	rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Get(ctx, m.in.Name, metav1.GetOptions{})
	if err != nil {
		return state{}, err
	}
	m.auth.Update(rf)
	// Validate applies the operator defaults.
	_ = rf.Validate()
	s := state{rf: rf, password: m.auth.Password(), authSecret: m.in.AuthSecret()}
	pods, err := m.kube.CoreV1().Pods(m.in.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/part-of=redis-failover,app.kubernetes.io/name=" + m.in.Name,
	})
	if err != nil {
		return state{}, err
	}
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	s.redis, s.sentinels = byComponent(pods.Items)
	m.readPods(ctx, &s, opts)
	s.sts, err = m.kube.AppsV1().StatefulSets(m.in.Namespace).Get(ctx, "rfr-"+m.in.Name, metav1.GetOptions{})
	if err != nil {
		return state{}, err
	}
	if rf.SentinelEnabled() || opts.sentinelObjects {
		if err := m.fetchSentinel(ctx, &s, opts.sentinelObjects); err != nil {
			return state{}, err
		}
	}
	return s, nil
}

// byComponent returns the redis pods and the Sentinel pods, in the order of
// pods.
func byComponent(pods []corev1.Pod) (redisPods, sentinels []corev1.Pod) {
	for _, p := range pods {
		switch p.Labels["app.kubernetes.io/component"] {
		case "redis":
			redisPods = append(redisPods, p)
		case "sentinel":
			sentinels = append(sentinels, p)
		}
	}
	return redisPods, sentinels
}

// readPods reads from the pods what the RedisFailover and opts ask for.
func (m *Mutator) readPods(ctx context.Context, s *state, opts fetchOpts) {
	port := int(s.rf.Spec.Redis.Port)
	if s.rf.Spec.Redis.MaxMemory != nil {
		s.config = m.redisConfig(ctx, s.redis, port)
	}
	if opts.password != nil {
		s.auth = m.checkPassword(ctx, s.redis, port, *opts.password)
	}
	if opts.sentinelMaster {
		s.sentinelMasters = m.sentinelMasters(ctx, s.sentinels)
	}
	if opts.servers {
		s.servers = m.servers(ctx, s.redis, s.sentinels, port)
	}
}

// fetchSentinel reads the Sentinel Deployment, and with objects whether the
// Sentinel Service and ConfigMap exist.
func (m *Mutator) fetchSentinel(ctx context.Context, s *state, objects bool) error {
	name := "rfs-" + m.in.Name
	var err error
	s.sentinel, err = m.kube.AppsV1().Deployments(m.in.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s.sentinel, err = nil, nil
	}
	if err != nil {
		return err
	}
	if objects {
		if s.sentinelService, err = exists(m.kube.CoreV1().Services(m.in.Namespace).Get(ctx, name, metav1.GetOptions{})); err != nil {
			return err
		}
		if s.sentinelConfigMap, err = exists(m.kube.CoreV1().ConfigMaps(m.in.Namespace).Get(ctx, name, metav1.GetOptions{})); err != nil {
			return err
		}
	}
	return nil
}

func exists[T any](_ T, err error) (bool, error) {
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// eachPod runs f on a client of every pod with an IP, in parallel, and
// returns its results by pod name.
func eachPod[T any](ctx context.Context, m *Mutator, pods []corev1.Pod, port int, credentials func() (string, string), f func(context.Context, *redis.Client) (T, error)) (map[string]T, map[string]error) {
	var mu sync.Mutex
	vals, errs := map[string]T{}, map[string]error{}
	var wg sync.WaitGroup
	for i := range pods {
		p := &pods[i]
		if p.Status.PodIP == "" {
			mu.Lock()
			errs[p.Name] = errors.New("no pod IP")
			mu.Unlock()
			continue
		}
		wg.Go(func() {
			c := observer.Client(net.JoinHostPort(p.Status.PodIP, strconv.Itoa(port)), credentials, m.timeout)
			defer func() { _ = c.Close() }()
			cctx, cancel := context.WithTimeout(ctx, m.timeout)
			defer cancel()
			v, err := f(cctx, c)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[p.Name] = err
			} else {
				vals[p.Name] = v
			}
		})
	}
	wg.Wait()
	return vals, errs
}

// checkPassword reports whether every redis pod accepts password; the empty
// password is whether it needs none.
func (m *Mutator) checkPassword(ctx context.Context, pods []corev1.Pod, port int, password string) map[string]error {
	_, errs := eachPod(ctx, m, pods, port, auth.Fixed(password), func(ctx context.Context, c *redis.Client) (string, error) {
		return c.Ping(ctx).Result()
	})
	out := map[string]error{}
	for _, p := range pods {
		out[p.Name] = errs[p.Name]
	}
	return out
}

// servers reads INFO from every redis pod, and INFO server from every Sentinel
// pod. INFO takes one section: Redis 6.2 rejects a list of sections.
func (m *Mutator) servers(ctx context.Context, redisPods, sentinels []corev1.Pod, port int) map[string]server {
	out := map[string]server{}
	for _, set := range []struct {
		pods     []corev1.Pod
		port     int
		creds    func() (string, string)
		sections []string
	}{
		{redisPods, port, m.auth.Provider(), nil},
		{sentinels, m.sentinelPort, auth.Fixed(""), []string{"server"}},
	} {
		vals, errs := eachPod(ctx, m, set.pods, set.port, set.creds, func(ctx context.Context, c *redis.Client) (string, error) {
			return c.Info(ctx, set.sections...).Result()
		})
		for _, p := range set.pods {
			if err, ok := errs[p.Name]; ok {
				out[p.Name] = server{err: err}
				continue
			}
			name, version, fields := observer.ServerOf(vals[p.Name])
			out[p.Name] = server{name: name, version: version, fields: fields}
		}
	}
	return out
}

func (m *Mutator) sentinelMasters(ctx context.Context, pods []corev1.Pod) map[string]sentinelMaster {
	vals, errs := eachPod(ctx, m, pods, m.sentinelPort, auth.Fixed(""), observer.SentinelMaster)
	out := map[string]sentinelMaster{}
	for _, p := range pods {
		out[p.Name] = sentinelMaster{fields: vals[p.Name], err: errs[p.Name]}
	}
	return out
}

// redisConfig reads maxmemory and maxmemory-policy from every redis pod
// with an IP, the pods the operator configures.
func (m *Mutator) redisConfig(ctx context.Context, pods []corev1.Pod, port int) []maxmem.Pod {
	vals, errs := eachPod(ctx, m, pods, port, m.auth.Provider(), func(ctx context.Context, c *redis.Client) (map[string]string, error) {
		return c.ConfigGet(ctx, "maxmemory*").Result()
	})
	out := make([]maxmem.Pod, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		if p.Status.PodIP == "" {
			continue
		}
		mp := maxmem.Pod{Name: p.Name, Limit: maxmem.RedisLimit(p), Err: errs[p.Name]}
		if mp.Err == nil {
			cfg := vals[p.Name]
			mp.MaxMemory, mp.Err = strconv.ParseInt(cfg["maxmemory"], 10, 64)
			mp.Policy = cfg["maxmemory-policy"]
		}
		out = append(out, mp)
	}
	return out
}
