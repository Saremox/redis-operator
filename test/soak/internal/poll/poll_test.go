package poll

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUntil(t *testing.T) {
	n := 0
	err := Until(context.Background(), time.Millisecond, time.Second, func(context.Context) error {
		if n++; n < 3 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Errorf("err %v after %d calls", err, n)
	}
	err = Until(context.Background(), time.Millisecond, 20*time.Millisecond, func(context.Context) error { return errors.New("never") })
	if err == nil || !strings.Contains(err.Error(), "not within 20ms: never") {
		t.Errorf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Until(ctx, time.Millisecond, 0, func(context.Context) error { return errors.New("never") }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}
