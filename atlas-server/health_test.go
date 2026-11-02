package atlas

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// stuckCheck ignores ctx and blocks until release is closed, counting its calls.
func stuckCheck(timeout time.Duration) (c *check, calls *atomic.Int32, release func()) {
	calls = new(atomic.Int32)
	unblock := make(chan struct{})
	c = &check{name: "stuck", timeout: timeout, fn: func(context.Context) error {
		calls.Add(1)
		<-unblock
		return nil
	}}
	return c, calls, func() { close(unblock) }
}

func TestCheckStillRunningIsReportedWithoutNewGoroutine(t *testing.T) {
	t.Parallel()
	c, calls, release := stuckCheck(20 * time.Millisecond)

	for i := range 3 {
		if err := c.run(t.Context()); !errors.Is(err, errCheckTimeout) {
			t.Fatalf("probe %d = %v, want %v", i, err, errCheckTimeout)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("check started %d times while stuck, want 1", n)
	}

	release()
	deadline := time.Now().Add(5 * time.Second)
	for c.run(t.Context()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("check never recovered after the stuck run finished")
		}
		time.Sleep(time.Millisecond)
	}
	if n := calls.Load(); n < 2 {
		t.Errorf("check started %d times, want a fresh run after the stuck one finished", n)
	}
}

func TestCheckProbeJoinsRunInProgress(t *testing.T) {
	t.Parallel()
	c, calls, release := stuckCheck(time.Minute)
	defer release()

	for range 3 {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		err := c.run(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("probe = %v, want the probe's own deadline", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("check started %d times for concurrent probes, want 1", n)
	}
}
