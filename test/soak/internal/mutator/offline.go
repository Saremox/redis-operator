package mutator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

// passwordNotApplied is the status message of an operator that can't bring
// the pods onto the Secret's password.
const passwordNotApplied = "unable to apply the configured password"

// offline is scenario C: the password changes while the operator is
// stopped, so the restarted operator can't apply it. The documented
// recovery is to put the previous password back, wait for Healthy, and
// change it again.
type offline struct {
	secret string
	// previous is the password before; first is the one the stopped
	// operator misses, second the one changed to after the recovery.
	previous, first, second string
}

// rotateOffline runs scenario C up to the second change, whose
// convergence is the mutation's. Every other instance's mutations are
// paused meanwhile, and every instance is in a window while the operator
// is stopped.
func (m *Mutator) rotateOffline(ctx context.Context, o *offline, log *slog.Logger) (err error) {
	n := 0
	phase := func(name string, f func() error) error {
		n++
		start := time.Now()
		err := f()
		l := log.With("phase", n, "phase_name", name, "duration_seconds", time.Since(start).Seconds())
		if err != nil {
			l.Warn("scenario phase failed", "error", err.Error())
		} else {
			l.Info("scenario phase done")
		}
		return err
	}
	m.lock.SetOperatorDown(true)
	running := false
	defer func() {
		if !running {
			// Never leave the operator stopped.
			err = errors.Join(err, m.scaleOperator(context.WithoutCancel(ctx), 1))
		}
		m.lock.SetOperatorDown(false)
	}()
	if err := phase("stop the operator", func() error { return m.scaleOperator(ctx, 0) }); err != nil {
		return err
	}
	if err := phase("rotate the password", func() error { return m.setPassword(ctx, o.secret, o.first) }); err != nil {
		return err
	}
	if err := phase("start the operator", func() error { return m.scaleOperator(ctx, 1) }); err != nil {
		return err
	}
	running = true
	m.lock.SetOperatorDown(false)

	var failed error
	if err := phase("expect "+passwordNotApplied, func() error {
		return m.waitRF(ctx, func(rf *redisfailoverv1.RedisFailover) error {
			if rf.Status.State != redisfailoverv1.NotHealthyState || rf.Status.Message != passwordNotApplied {
				return fmt.Errorf("state %q: %q", rf.Status.State, rf.Status.Message)
			}
			return nil
		})
	}); err != nil {
		failed = expectation{err}
	}
	if err := phase("put the previous password back", func() error { return m.setPassword(ctx, o.secret, o.previous) }); err != nil {
		return err
	}
	if err := phase("expect Healthy", func() error {
		return m.waitConverged(ctx, fetchOpts{password: &o.previous}, func(s state) error {
			if err := passwordAccepted(s); err != nil {
				return err
			}
			return healthy(s.rf)
		})
	}); err != nil && failed == nil {
		failed = expectation{err}
	}
	if err := phase("rotate again", func() error { return m.setPassword(ctx, o.secret, o.second) }); err != nil {
		return err
	}
	return failed
}

// scaleOperator scales the operator Deployment through its scale
// subresource, and waits until it runs that many available pods, or none
// at all.
func (m *Mutator) scaleOperator(ctx context.Context, replicas int32) error {
	body := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	err := m.kube.AppsV1().RESTClient().Patch(types.MergePatchType).Namespace(m.operator.Namespace).
		Resource("deployments").Name(m.operator.Deployment).SubResource("scale").Body(body).Do(ctx).Error()
	if err != nil {
		return err
	}
	return m.wait(ctx, func() error {
		d, err := m.kube.AppsV1().Deployments(m.operator.Namespace).Get(ctx, m.operator.Deployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if replicas > 0 {
			if d.Status.ObservedGeneration < d.Generation || d.Status.AvailableReplicas != replicas || d.Status.UpdatedReplicas != replicas {
				return fmt.Errorf("%d of %d operator pods available", d.Status.AvailableReplicas, replicas)
			}
			return nil
		}
		selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
		if err != nil {
			return err
		}
		pods, err := m.kube.CoreV1().Pods(m.operator.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return err
		}
		if len(pods.Items) > 0 {
			return fmt.Errorf("%d operator pods left", len(pods.Items))
		}
		return nil
	})
}

// waitRF waits until the RedisFailover satisfies ok.
func (m *Mutator) waitRF(ctx context.Context, ok func(*redisfailoverv1.RedisFailover) error) error {
	return m.wait(ctx, func() error {
		rf, err := m.rfs.DatabasesV1().RedisFailovers(m.in.Namespace).Get(ctx, m.in.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return ok(rf)
	})
}

// waitConverged waits until the instance satisfies converged.
func (m *Mutator) waitConverged(ctx context.Context, opts fetchOpts, converged func(state) error) error {
	return m.wait(ctx, func() error {
		s, err := m.fetch(ctx, opts)
		if err != nil {
			return err
		}
		return converged(s)
	})
}

// wait polls f every second until it returns nil, for at most the
// convergence timeout, and returns its last error then.
func (m *Mutator) wait(ctx context.Context, f func() error) error {
	deadline := time.Now().Add(m.convergeTimeout)
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not within %s: %w", m.convergeTimeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func healthy(rf *redisfailoverv1.RedisFailover) error {
	if rf.Status.State != redisfailoverv1.HealthyState {
		return fmt.Errorf("state %q: %s", rf.Status.State, rf.Status.Message)
	}
	return nil
}
