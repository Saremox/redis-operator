// sentloss writes sequence-numbered keys through three client paths and
// verifies which acknowledged writes survive.
//
//	sentloss write -rfrm HOST:6379 -sentinel HOST:26379 -rate 20
//	sentloss verify -master IP:6379 -log writer.log [-after UNIXMS] [-before UNIXMS]
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

var laddrRe = regexp.MustCompile(`laddr=([0-9.]+):`)

func main() {
	switch os.Args[1] {
	case "write":
		write(os.Args[2:])
	case "verify":
		verify(os.Args[2:])
	}
}

var outMu sync.Mutex

func emit(format string, a ...any) {
	outMu.Lock()
	fmt.Printf(format+"\n", a...)
	outMu.Unlock()
}

func loop(mode string, rate int, do func(ctx context.Context, key string, seq int64) (string, error)) {
	t := time.NewTicker(time.Second / time.Duration(rate))
	for seq := int64(0); ; seq++ {
		<-t.C
		key := fmt.Sprintf("w:%s:%d", mode, seq)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		start := time.Now()
		srv, err := do(ctx, key, seq)
		cancel()
		if err != nil {
			e := strings.ReplaceAll(err.Error(), " ", "_")
			if len(e) > 60 {
				e = e[:60]
			}
			emit("%d %s %d ERR %s %s", start.UnixMilli(), mode, seq, srv, e)
			continue
		}
		emit("%d %s %d OK %s", start.UnixMilli(), mode, seq, srv)
	}
}

// setInfo sends SET and CLIENT INFO on one connection; laddr tells which
// pod acknowledged the write.
func setInfo(ctx context.Context, c redis.UniversalClient, key string, seq int64) (string, error) {
	cmds, err := c.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, key, seq, 0)
		p.Do(ctx, "CLIENT", "INFO")
		return nil
	})
	srv := "?"
	if len(cmds) == 2 {
		if s, e := cmds[1].(*redis.Cmd).Text(); e == nil {
			if m := laddrRe.FindStringSubmatch(s); m != nil {
				srv = m[1]
			}
		}
		if e := cmds[0].Err(); e != nil {
			return srv, e
		}
	}
	return srv, err
}

func write(args []string) {
	fs := flag.NewFlagSet("write", flag.ExitOnError)
	rfrm := fs.String("rfrm", "", "")
	sent := fs.String("sentinel", "", "")
	rate := fs.Int("rate", 20, "")
	_ = fs.Parse(args)
	opts := func() *redis.Options {
		return &redis.Options{Addr: *rfrm, DialTimeout: 300 * time.Millisecond, ReadTimeout: 400 * time.Millisecond, WriteTimeout: 400 * time.Millisecond, MaxRetries: -1}
	}
	pooled := redis.NewClient(opts())
	go loop("pooled", *rate, func(ctx context.Context, key string, seq int64) (string, error) {
		return setInfo(ctx, pooled, key, seq)
	})
	go loop("fresh", *rate, func(ctx context.Context, key string, seq int64) (string, error) {
		c := redis.NewClient(opts())
		defer c.Close()
		return setInfo(ctx, c, key, seq)
	})
	fo := redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName: "mymaster", SentinelAddrs: []string{*sent},
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 400 * time.Millisecond, WriteTimeout: 400 * time.Millisecond, MaxRetries: -1,
	})
	go loop("sentinel", *rate, func(ctx context.Context, key string, seq int64) (string, error) {
		return setInfo(ctx, fo, key, seq)
	})
	select {}
}

type ack struct {
	ts   int64
	mode string
	seq  int64
	srv  string
}

func verify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	master := fs.String("master", "", "")
	logf := fs.String("log", "", "")
	before := fs.Int64("before", 0, "only acks before this unix ms")
	after := fs.Int64("after", 0, "only acks at or after this unix ms")
	_ = fs.Parse(args)
	f, err := os.Open(*logf)
	if err != nil {
		panic(err)
	}
	var acks []ack
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 5 || p[3] != "OK" {
			continue
		}
		ts, _ := strconv.ParseInt(p[0], 10, 64)
		seq, _ := strconv.ParseInt(p[2], 10, 64)
		if (*before > 0 && ts >= *before) || ts < *after {
			continue
		}
		acks = append(acks, ack{ts, p[1], seq, p[4]})
	}
	c := redis.NewClient(&redis.Options{Addr: *master})
	ctx := context.Background()
	lost := map[string][]ack{}
	total := map[string]int{}
	for i := 0; i < len(acks); i += 500 {
		j := min(i+500, len(acks))
		keys := make([]string, 0, j-i)
		for _, a := range acks[i:j] {
			keys = append(keys, fmt.Sprintf("w:%s:%d", a.mode, a.seq))
		}
		vals, err := c.MGet(ctx, keys...).Result()
		if err != nil {
			panic(err)
		}
		for k, v := range vals {
			a := acks[i+k]
			total[a.mode]++
			if v == nil {
				lost[a.mode] = append(lost[a.mode], a)
			}
		}
	}
	for _, m := range []string{"pooled", "fresh", "sentinel"} {
		l := lost[m]
		sort.Slice(l, func(i, j int) bool { return l[i].seq < l[j].seq })
		fmt.Printf("mode=%s acked=%d lost=%d\n", m, total[m], len(l))
		for i := 0; i < len(l); {
			j := i
			for j+1 < len(l) && l[j+1].seq == l[j].seq+1 {
				j++
			}
			srv := map[string]int{}
			for _, a := range l[i : j+1] {
				srv[a.srv]++
			}
			fmt.Printf("  seqs %d-%d (%d) acked %s .. %s (%.1fs) by %v\n", l[i].seq, l[j].seq, j-i+1,
				time.UnixMilli(l[i].ts).UTC().Format("15:04:05.000"), time.UnixMilli(l[j].ts).UTC().Format("15:04:05.000"),
				float64(l[j].ts-l[i].ts)/1000, srv)
			i = j + 1
		}
	}
}
