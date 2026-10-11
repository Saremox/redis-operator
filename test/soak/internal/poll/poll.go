// Package poll waits for a condition, and keeps the reason why it does not
// hold yet.
package poll

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// Until calls f every interval until f returns nil, ctx is done, or the
// timeout passed. A timeout of 0 has no limit. On a timeout, it returns the
// last error of f.
func Until(ctx context.Context, interval, timeout time.Duration, f func(context.Context) error) error {
	var last error
	cond := func(ctx context.Context) (bool, error) {
		last = f(ctx)
		return last == nil, nil
	}
	var err error
	if timeout > 0 {
		err = wait.PollUntilContextTimeout(ctx, interval, timeout, true, cond)
	} else {
		err = wait.PollUntilContextCancel(ctx, interval, true, cond)
	}
	if err != nil && last != nil && ctx.Err() == nil {
		return fmt.Errorf("not within %s: %w", timeout, last)
	}
	return err
}
