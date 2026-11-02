package atlas_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/svallejo-dev/olympus-core/atlas-server"
)

func TestBackgroundWorkerStopsAfterDrainDelay(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	canceled := make(chan struct{})
	worker := func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	svc := start(t, helloMux(), atlas.WithShutdownDelay(200*time.Millisecond), atlas.WithBackground("consumer", worker))
	<-started

	svc.cancel()
	svc.logs.waitFor(t, `"msg":"shutting down"`)
	select {
	case <-canceled:
		t.Fatal("worker canceled before the drain delay ended")
	default:
	}
	if err := svc.stop(); err != nil {
		t.Errorf("Run() = %v, want nil for a worker that stops with ctx.Err()", err)
	}
	select {
	case <-canceled:
	default:
		t.Error("Run returned before the worker stopped")
	}
}

func TestBackgroundWorkerFailureStopsRun(t *testing.T) {
	t.Parallel()
	errLost := errors.New("queue connection lost")
	otherStopped := make(chan struct{})
	svc := start(t, helloMux(),
		atlas.WithBackground("consumer", func(context.Context) error { return errLost }),
		atlas.WithBackground("other", func(ctx context.Context) error {
			<-ctx.Done()
			close(otherStopped)
			return nil
		}),
	)

	err := svc.wait()
	if !errors.Is(err, errLost) || !strings.Contains(err.Error(), `background worker "consumer"`) {
		t.Errorf("Run() = %v, want the consumer's error", err)
	}
	select {
	case <-otherStopped:
	default:
		t.Error("the other worker was not stopped before Run returned")
	}
}

func TestBackgroundWorkerPanicStopsRun(t *testing.T) {
	t.Parallel()
	svc := start(t, helloMux(), atlas.WithBackground("consumer", func(context.Context) error { panic("nil channel") }))

	err := svc.wait()
	if err == nil || !strings.Contains(err.Error(), `background worker "consumer": panic: nil channel`) {
		t.Errorf("Run() = %v, want the worker panic as an error", err)
	}
	panics := svc.logs.records(t, "panic recovered")
	if len(panics) != 1 || panics[0]["background"] != "consumer" || !strings.Contains(fmt.Sprint(panics[0]["stack"]), "TestBackgroundWorkerPanicStopsRun") {
		t.Errorf("panic logs = %v, want one for worker consumer with its stack", panics)
	}
}

func TestBackgroundWorkerReturningNilJustEnds(t *testing.T) {
	t.Parallel()
	finished := make(chan struct{})
	svc := start(t, helloMux(), atlas.WithBackground("warmup", func(context.Context) error {
		close(finished)
		return nil
	}))
	<-finished

	if got := get(t, svc.url+"/hello"); got.status != http.StatusOK {
		t.Errorf("GET /hello after the worker ended = %d, want 200", got.status)
	}
	if err := svc.stop(); err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
}

func TestBackgroundWorkerIgnoringCancelIsBoundedByShutdownTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hookRan := false
	svc := start(t, helloMux(),
		atlas.WithShutdownTimeout(100*time.Millisecond),
		atlas.WithBackground("stuck", func(context.Context) error {
			<-release
			return nil
		}),
		atlas.WithShutdownHook("db", func(context.Context) error {
			hookRan = true
			return nil
		}),
	)

	err := svc.stop()
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `background worker "stuck" did not stop`) {
		t.Errorf("Run() = %v, want a did-not-stop error", err)
	}
	if !hookRan {
		t.Error("shutdown hook did not run after the worker timed out")
	}
}
