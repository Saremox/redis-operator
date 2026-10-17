package observer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
)

// timingObserver returns an observer with a short read timeout, whose log
// lines go to the returned function.
func timingObserver(t *testing.T) (*Observer, func() []map[string]any) {
	t.Helper()
	rf := &redisfailoverv1.RedisFailover{}
	rf.Name, rf.Namespace = "x", "ns"
	o, _, _ := collectObserver(t, "instances: [{name: x, namespace: ns}]", rf)
	o.timeout = 200 * time.Millisecond
	var mu sync.Mutex
	var buf bytes.Buffer
	o.log = slog.New(slog.NewJSONHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})).With("rf", "x", "namespace", "ns")
	return o, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var lines []map[string]any
		for l := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			if l == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, m)
		}
		return lines
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}

// silentServer accepts connections and never replies.
func silentServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-done
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return l.Addr().String()
}

// blockingDial waits for the end of the dial's context, as a dial to an
// address that does not answer.
func blockingDial(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func refusingDial(context.Context, string, string) (net.Conn, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// A failed read logs at warn level the phase in which it failed: dial
// without a connection, reply when the connection was made, and context
// when the round's context ended. The error of the read stays the one of
// go-redis.
func TestReadPhase(t *testing.T) {
	silent := silentServer(t)
	for _, tc := range []struct {
		name   string
		dial   func(context.Context, string, string) (net.Conn, error)
		addr   string
		cancel bool
		phase  string
		err    string
	}{
		{name: "dial does not end", dial: blockingDial, addr: "10.0.0.1:6379", phase: phaseDial, err: "context deadline exceeded"},
		{name: "dial refused", dial: refusingDial, addr: "10.0.0.1:6379", phase: phaseDial},
		{name: "no reply", addr: silent, phase: phaseReply, err: "i/o timeout"},
		{name: "round ended", dial: blockingDial, addr: "10.0.0.1:6379", cancel: true, phase: phaseContext, err: "context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, lines := timingObserver(t)
			o.dial = tc.dial
			ip, port := hostPort(t, tc.addr)
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancel {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			defer cancel()
			p := &redisPod{pod: pod{Name: "rfr-x-0", IP: ip}}
			o.redisInfo(ctx, p, port, nil)
			if p.err == nil || !strings.Contains(p.err.Error(), tc.err) {
				t.Errorf("error %v, want %q", p.err, tc.err)
			}
			got := lines()
			if len(got) != 1 {
				t.Fatalf("log %v", got)
			}
			l := got[0]
			if l["level"] != "WARN" || l["msg"] != "observer read slow" || l["phase"] != tc.phase || l["kind"] != "redis" ||
				l["pod"] != "rfr-x-0" || l["ip"] != ip || l["rf"] != "x" || l["namespace"] != "ns" || l["error"] != p.err.Error() {
				t.Errorf("log %v", l)
			}
			if dials, _ := l["dials"].(float64); dials < 1 {
				t.Errorf("dials %v", l["dials"])
			}
		})
	}
}

// The finding of a read whose dial does not end in time keeps the text
// that the alerts and the soak sessions know.
func TestReadTimeoutReason(t *testing.T) {
	o, _ := timingObserver(t)
	o.dial = blockingDial
	s := snapshot{rf: &redisfailoverv1.RedisFailover{}, redis: []redisPod{{pod: pod{Name: "rfr-x-0", IP: "10.0.0.1"}}}}
	o.redisInfo(context.Background(), &s.redis[0], 6379, []string{"maxmemory"})
	if err := s.checkConfig(); err == nil || err.Error() != "rfr-x-0: context deadline exceeded" {
		t.Errorf("reason %v", err)
	}
	p := &sentinelPod{pod: pod{Name: "rfs-x-0", IP: "10.0.0.2"}}
	o.sentinelInfo(context.Background(), p)
	if p.err == nil || p.err.Error() != "context deadline exceeded" {
		t.Errorf("sentinel %v", p.err)
	}
}

// stepClock advances by step at each call.
func stepClock(step time.Duration) func() time.Time {
	var n atomic.Int64
	base := time.Unix(1000, 0)
	return func() time.Time { return base.Add(time.Duration(n.Add(1)) * step) }
}

// A read that succeeds logs only when it took more than 1 second, without a
// phase. The source kind reads the master of the source.
func TestReadSlow(t *testing.T) {
	mr := miniredis.RunT(t)
	ip, port := hostPort(t, mr.Addr())
	o, lines := timingObserver(t)
	p := &redisPod{pod: pod{Name: "rfr-x-0", IP: ip}}
	o.redisInfo(context.Background(), p, port, nil)
	if p.err != nil || len(lines()) != 0 {
		t.Fatalf("fast read: %v %v", p.err, lines())
	}
	o.now = stepClock(1500 * time.Millisecond)
	o.redisInfo(context.Background(), p, port, nil)
	got := lines()
	if p.err != nil || len(got) != 1 || got[0]["msg"] != "observer read slow" || got[0]["phase"] != "" || got[0]["error"] != "" ||
		got[0]["duration_seconds"] != 1.5 || got[0]["dials"] != 1.0 {
		t.Fatalf("slow read: %v %v", p.err, got)
	}

	src, _ := timingObserver(t)
	src.in.Port = port
	src.view.Store(&view{master: "rfr-src-0", masterIP: ip})
	o.source = src
	// miniredis has no INFO replication: the read fails after the reply.
	if _, _, err := o.masterPosition(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if got := lines(); len(got) != 2 || got[1]["kind"] != "source" || got[1]["pod"] != "rfr-src-0" || got[1]["ip"] != ip || got[1]["phase"] != phaseReply {
		t.Errorf("source read: %v", got)
	}
}

// A slow Sentinel read logs its kind.
func TestReadSlowSentinel(t *testing.T) {
	o, lines := timingObserver(t)
	o.dial = refusingDial
	o.sentinelInfo(context.Background(), &sentinelPod{pod: pod{Name: "rfs-x-0", IP: "10.0.0.2"}})
	if got := lines(); len(got) != 1 || got[0]["kind"] != "sentinel" || got[0]["pod"] != "rfs-x-0" || got[0]["phase"] != phaseDial {
		t.Errorf("log %v", got)
	}
}

// phase tells a read without a connection from one with a connection, and
// both from the end of the round.
func TestPhase(t *testing.T) {
	r := &read{}
	if d, n := r.dialTime(); d != 0 || n != 0 {
		t.Errorf("no dial: dial time %v %d", d, n)
	}
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close(); _ = peer.Close() }()
	dial := r.dialer(func(context.Context, string, string) (net.Conn, error) { return conn, nil })
	before := time.Now()
	if c, err := dial(context.Background(), "tcp", "x"); c != conn || err != nil {
		t.Fatal(c, err)
	}
	after := time.Now()
	if d, n := r.dialTime(); n != 1 || d < 0 {
		t.Errorf("dial time %v %d", d, n)
	}
	refused := errors.New("WRONGPASS invalid username-password pair")
	for _, tc := range []struct {
		name  string
		r     *read
		end   time.Time
		err   error
		phase string
	}{
		{"no connection", &read{}, after, refused, phaseDial},
		{"connected", r, after, refused, phaseReply},
		// The read ended before the dial made the connection.
		{"connected late", r, before, refused, phaseDial},
		{"no connection in time", r, after, context.DeadlineExceeded, phaseDial},
		{"no reply in time", &read{}, after, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, phaseReply},
		{"dial timeout", r, after, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, phaseDial},
		{"round ended", r, after, &net.OpError{Op: "dial", Err: context.Canceled}, phaseContext},
	} {
		if got := tc.r.phase(tc.err, tc.end); got != tc.phase {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.phase)
		}
	}
}

// A read logs the error of INFO, or else the error of CONFIG GET, on one
// line.
func TestReadLogsOneError(t *testing.T) {
	mr := miniredis.RunT(t)
	ip, port := hostPort(t, mr.Addr())
	o, lines := timingObserver(t)
	p := &redisPod{pod: pod{Name: "rfr-x-0", IP: ip}}
	// miniredis has no CONFIG GET.
	o.redisInfo(context.Background(), p, port, []string{"maxmemory"})
	if got := lines(); p.err != nil || p.configErr == nil || len(got) != 1 || got[0]["error"] != p.configErr.Error() || got[0]["phase"] != phaseReply {
		t.Fatalf("config error: %v %v", p.configErr, got)
	}
	o, lines = timingObserver(t)
	o.dial = blockingDial
	p = &redisPod{pod: pod{Name: "rfr-x-0", IP: "10.0.0.1"}}
	o.redisInfo(context.Background(), p, 6379, []string{"maxmemory"})
	if got := lines(); p.err == nil || p.configErr == nil || len(got) != 1 || got[0]["error"] != "context deadline exceeded" {
		t.Errorf("both errors: %v %v %v", p.err, p.configErr, got)
	}
}

// dialTime runs to now while a dial runs.
func TestDialTimeRunning(t *testing.T) {
	r := &read{}
	started, release := make(chan struct{}), make(chan struct{})
	dial := r.dialer(func(context.Context, string, string) (net.Conn, error) {
		close(started)
		<-release
		return nil, errors.New("refused")
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = dial(context.Background(), "tcp", "x")
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	if d, n := r.dialTime(); n != 1 || d < 20*time.Millisecond {
		t.Errorf("running dial %v %d", d, n)
	}
	close(release)
	<-done
	d, _ := r.dialTime()
	time.Sleep(20 * time.Millisecond)
	if d2, _ := r.dialTime(); d2 != d {
		t.Errorf("ended dial grew from %v to %v", d, d2)
	}
}

// A round that starts more than 1 second after its tick, or takes more than
// 1 second, logs its phases at warn level. Another round logs at debug
// level. Each round adds a sample to both histograms.
func TestTimeRound(t *testing.T) {
	o, lines := timingObserver(t)
	at := time.Unix(1000, 0)
	s := snapshot{kubeTime: 100 * time.Millisecond, readTime: 300 * time.Millisecond}
	o.timeRound(t.Context(), at, at.Add(50*time.Millisecond), at.Add(450*time.Millisecond), at.Add(500*time.Millisecond), s)
	o.timeRound(t.Context(), at, at.Add(1500*time.Millisecond), at.Add(1600*time.Millisecond), at.Add(1700*time.Millisecond), snapshot{})
	o.timeRound(t.Context(), at, at, at.Add(1900*time.Millisecond), at.Add(2*time.Second), snapshot{readTime: 1800 * time.Millisecond})
	got := lines()
	if len(got) != 3 {
		t.Fatalf("log %v", got)
	}
	want := []struct {
		level, msg     string
		late, duration float64
	}{
		{"DEBUG", "observer round", 0.05, 0.45},
		{"WARN", "observer round slow", 1.5, 0.2},
		{"WARN", "observer round slow", 0, 2},
	}
	for i, w := range want {
		l := got[i]
		if l["level"] != w.level || l["msg"] != w.msg || l["lateness_seconds"] != w.late || l["duration_seconds"] != w.duration || l["rf"] != "x" {
			t.Errorf("round %d: %v", i, l)
		}
	}
	if l := got[0]; l["kube_seconds"] != 0.1 || l["read_seconds"] != 0.3 || l["apply_seconds"] != 0.05 {
		t.Errorf("phases %v", l)
	}
	if n, sum := samples(t, o.roundTime); n != 3 || math.Abs(sum-2.65) > 1e-9 {
		t.Errorf("round samples %d, sum %v", n, sum)
	}
	if n, sum := samples(t, o.lateness); n != 3 || math.Abs(sum-1.55) > 1e-9 {
		t.Errorf("lateness samples %d, sum %v", n, sum)
	}
}

// A round that starts before it is due has no lateness.
func TestTimeRoundEarly(t *testing.T) {
	o, lines := timingObserver(t)
	at := time.Unix(1000, 0)
	o.timeRound(t.Context(), at, at.Add(-time.Second), at, at, snapshot{})
	if got := lines(); len(got) != 1 || got[0]["lateness_seconds"] != 0.0 {
		t.Errorf("log %v", got)
	}
	if _, sum := samples(t, o.lateness); sum != 0 {
		t.Errorf("lateness sum %v", sum)
	}
}

func samples(t *testing.T, obs prometheus.Observer) (uint64, float64) {
	t.Helper()
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

// round measures its lateness from the tick it was due at, on the clock of
// the observer.
func TestRoundLateness(t *testing.T) {
	o, lines := timingObserver(t)
	at := time.Unix(1000, 0)
	o.now = func() time.Time { return at }
	o.round(context.Background(), at.Add(-3*time.Second))
	got := roundLines(lines())
	if len(got) != 1 || got[0]["msg"] != "observer round slow" || got[0]["lateness_seconds"] != 3.0 || got[0]["duration_seconds"] != 0.0 {
		t.Errorf("log %v", got)
	}
	if n, _ := samples(t, o.roundTime); n != 1 {
		t.Errorf("round samples %d", n)
	}
}

// collect measures the reads of the API and of the pods on the clock of the
// observer.
func TestCollectTimes(t *testing.T) {
	o, _ := timingObserver(t)
	o.now = stepClock(time.Second)
	s, _, err := o.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.kubeTime != time.Second || s.readTime != time.Second {
		t.Errorf("kube %v, read %v", s.kubeTime, s.readTime)
	}
}

// A round whose read of the API fails after a delay gives the delay to the
// API reads, for each of the three reads.
func TestCollectFailedTimes(t *testing.T) {
	for _, tc := range []struct{ verb, resource string }{
		{"get", "redisfailovers"},
		{"list", "pods"},
		{"list", "endpointslices"},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			o, lines := timingObserver(t)
			var clock atomic.Int64
			o.now = func() time.Time { return time.Unix(0, clock.Load()) }
			slow := func(k8stesting.Action) (bool, runtime.Object, error) {
				clock.Add(int64(2 * time.Second))
				return true, nil, errors.New("the server was unable to return a response in the time allotted")
			}
			if tc.resource == "redisfailovers" {
				o.rfs.(*rffake.Clientset).PrependReactor(tc.verb, tc.resource, slow)
			} else {
				o.kube.(*fake.Clientset).PrependReactor(tc.verb, tc.resource, slow)
			}
			o.round(context.Background(), o.now())
			got := roundLines(lines())
			if len(got) != 1 || got[0]["msg"] != "observer round slow" || got[0]["kube_seconds"] != 2.0 || got[0]["read_seconds"] != 0.0 {
				t.Errorf("log %v", got)
			}
		})
	}
}

// Run starts the first round at once, with no lateness.
func TestRunFirstRound(t *testing.T) {
	o, lines := timingObserver(t)
	at := time.Unix(1000, 0)
	o.now = func() time.Time { return at }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.Run(ctx)
	got := roundLines(lines())
	if len(got) != 1 || got[0]["msg"] != "observer round" || got[0]["lateness_seconds"] != 0.0 {
		t.Errorf("log %v", got)
	}
}

// A normal round logs nothing at info level and allocates nothing for its
// timing. The timing of a fast read allocates the read and its dialer.
func TestTimingCost(t *testing.T) {
	o, _ := timingObserver(t)
	var buf bytes.Buffer
	o.log = slog.New(slog.NewJSONHandler(&buf, nil))
	at := time.Unix(1000, 0)
	if n := testing.AllocsPerRun(100, func() {
		o.timeRound(t.Context(), at, at.Add(time.Millisecond), at.Add(20*time.Millisecond), at.Add(25*time.Millisecond), snapshot{})
	}); n != 0 {
		t.Errorf("round timing allocates %v", n)
	}
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close(); _ = peer.Close() }()
	dial := func(context.Context, string, string) (net.Conn, error) { return conn, nil }
	n := testing.AllocsPerRun(100, func() {
		r := o.startRead("redis", "rfr-x-0", "10.0.0.1")
		_, _ = r.dialer(dial)(context.Background(), "tcp", "x")
		o.endRead(r, nil)
	})
	t.Logf("a fast read allocates %v times for its timing", n)
	if n > 2 {
		t.Errorf("read timing allocates %v", n)
	}
	if buf.Len() != 0 {
		t.Errorf("log %s", buf.String())
	}
}

// Run starts each next round at a tick of the interval. A round whose
// reads of the API fail logs the error, and is timed.
func TestRunTicks(t *testing.T) {
	o, lines := timingObserver(t)
	o.rfs = rffake.NewSimpleClientset()
	o.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(45*time.Millisecond, cancel)
	o.Run(ctx)
	all := lines()
	if got := roundLines(all); len(got) < 2 {
		t.Errorf("rounds %v", got)
	}
	if all[0]["msg"] != "observing" || all[0]["level"] != "WARN" {
		t.Errorf("log %v", all[0])
	}
}

// A round that takes more than two intervals does not make the next round
// late: the next round is due at the end of the long round.
func TestRunOverrun(t *testing.T) {
	o, lines := timingObserver(t)
	rfs := rffake.NewSimpleClientset()
	var calls atomic.Int32
	rfs.PrependReactor("get", "redisfailovers", func(k8stesting.Action) (bool, runtime.Object, error) {
		if calls.Add(1) == 1 {
			time.Sleep(70 * time.Millisecond)
		}
		return false, nil, nil
	})
	o.rfs = rfs
	o.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	o.Run(ctx)
	got := roundLines(lines())
	if len(got) < 2 {
		t.Fatalf("rounds %v", got)
	}
	if d, _ := got[0]["duration_seconds"].(float64); d < 0.07 {
		t.Errorf("first round %v", got[0])
	}
	if late, _ := got[1]["lateness_seconds"].(float64); late > 0.015 {
		t.Errorf("round after the long round is %v seconds late", late)
	}
}

// roundLines returns the log lines of the round timing.
func roundLines(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if msg, _ := l["msg"].(string); strings.HasPrefix(msg, "observer round") {
			out = append(out, l)
		}
	}
	return out
}
