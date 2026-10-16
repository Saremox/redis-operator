package mutator

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// phases returns the phase log lines of scenario C in order, as "done name"
// or "failed name".
func (r *rotation) phases() []string {
	var out []string
	for line := range strings.SplitSeq(r.out.String(), "\n") {
		var l struct {
			Msg  string `json:"msg"`
			Name string `json:"phase_name"`
		}
		if json.Unmarshal([]byte(line), &l) != nil || !strings.HasPrefix(l.Msg, "scenario phase ") {
			continue
		}
		out = append(out, strings.TrimPrefix(l.Msg, "scenario phase ")+" "+l.Name)
	}
	return out
}

var errDenied = errors.New("denied")

// faults records the writes to the operator Deployment and the Secrets, and
// fails the calls that fail names: a verb and resource, and the numbers of
// the calls, from 1.
type faults struct {
	mu    sync.Mutex
	fail  map[string][]int
	count map[string]int
	calls []string
}

func (f *faults) install(r *rotation) {
	for _, vr := range [][2]string{{"get", "deployments"}, {"patch", "deployments"}, {"update", "secrets"}, {"delete", "secrets"}} {
		r.kube.PrependReactor(vr[0], vr[1], func(a k8stesting.Action) (bool, runtime.Object, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			key := vr[0] + " " + vr[1]
			f.count[key]++
			// The waits read the Deployment again and again.
			if key != "get deployments" {
				c := key
				if p, ok := a.(k8stesting.PatchAction); ok {
					c += " " + string(p.GetPatch())
				}
				f.calls = append(f.calls, c)
			}
			if slices.Contains(f.fail[key], f.count[key]) {
				return true, nil, errDenied
			}
			return false, nil, nil
		})
	}
}

// rotateOffline runs its phases in order, stops at the first failed phase,
// and starts a stopped operator again on the way out.
func TestRotateOfflineOrder(t *testing.T) {
	const (
		stop  = `patch deployments {"spec":{"replicas":0}}`
		start = `patch deployments {"spec":{"replicas":1}}`
	)
	healthy := redisfailoverv1.RedisFailoverStatus{State: redisfailoverv1.HealthyState}
	forgetPhases := []string{"done stop the operator", "done rotate the password", "done delete the applied passwords", "done start the operator"}
	cases := []struct {
		name        string
		forget      bool
		status      redisfailoverv1.RedisFailoverStatus
		fail        map[string][]int
		err         string
		expectation bool
		calls       []string
		phases      []string
	}{
		{name: "operator not read", fail: map[string][]int{"get deployments": {1}}, err: "denied"},
		{name: "stop fails", fail: map[string][]int{"patch deployments": {1}}, err: "denied",
			calls: []string{stop, start}, phases: []string{"failed stop the operator"}},
		{name: "rotate fails", fail: map[string][]int{"update secrets": {1}}, err: "denied",
			calls:  []string{stop, "update secrets", start},
			phases: []string{"done stop the operator", "failed rotate the password"}},
		{name: "start fails", fail: map[string][]int{"patch deployments": {2, 3}}, err: "denied\ndenied",
			calls:  []string{stop, "update secrets", start, start},
			phases: []string{"done stop the operator", "done rotate the password", "failed start the operator"}},
		{name: "converges", status: healthy,
			calls:  []string{stop, "update secrets", start},
			phases: []string{"done stop the operator", "done rotate the password", "done start the operator"}},
		{name: "delete fails", forget: true, fail: map[string][]int{"delete secrets": {1}}, err: "denied",
			calls:  []string{stop, "update secrets", "delete secrets", start},
			phases: []string{"done stop the operator", "done rotate the password", "failed delete the applied passwords"}},
		{name: "previous fails", forget: true, status: stuck, fail: map[string][]int{"update secrets": {2}}, err: "denied",
			calls:  []string{stop, "update secrets", "delete secrets", start, "update secrets"},
			phases: append(slices.Clone(forgetPhases), "done expect "+passwordNotApplied, "failed put the previous password back")},
		{name: "again fails", forget: true, status: stuck, fail: map[string][]int{"update secrets": {3}}, err: "denied",
			calls: []string{stop, "update secrets", "delete secrets", start, "update secrets", "update secrets"},
			phases: append(slices.Clone(forgetPhases), "done expect "+passwordNotApplied, "done put the previous password back",
				"failed expect Healthy", "failed rotate again")},
		{name: "not stuck", forget: true, status: healthy, err: `state "Healthy": ""`, expectation: true,
			calls: []string{stop, "update secrets", "delete secrets", start, "update secrets", "update secrets"},
			phases: append(slices.Clone(forgetPhases), "failed expect "+passwordNotApplied, "done put the previous password back",
				"done expect Healthy", "done rotate again")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ctx := newRotation(t, c.status)
			r.operatorDeployment(t)
			r.appliedPasswords(t)
			r.m.convergeTimeout = 50 * time.Millisecond
			f := &faults{fail: c.fail, count: map[string]int{}}
			f.install(r)
			o := &offline{secret: "x-auth", previous: "old", first: "p1", second: "p2", forget: c.forget}
			err := r.m.rotateOffline(ctx, o, r.m.log)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if !strings.HasSuffix(got, c.err) || (c.err == "") != (err == nil) {
				t.Errorf("error %q, want %q", got, c.err)
			}
			var exp expectation
			if errors.As(err, &exp) != c.expectation {
				t.Errorf("error %v: expectation %v", err, !c.expectation)
			}
			if !slices.Equal(f.calls, c.calls) {
				t.Errorf("calls %q\nwant  %q", f.calls, c.calls)
			}
			if got := r.phases(); !slices.Equal(got, c.phases) {
				t.Errorf("phases %q\nwant   %q", got, c.phases)
			}
			if o.phases != len(c.phases) {
				t.Errorf("%d phases, want %d", o.phases, len(c.phases))
			}
		})
	}
}
