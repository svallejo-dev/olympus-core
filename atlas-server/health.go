package atlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const checkTimeout = 2 * time.Second

const (
	statusOK          = "ok"
	statusUnavailable = "unavailable"
)

// Check reports whether a dependency is usable; a non-nil error marks the service unavailable.
type Check func(ctx context.Context) error

var errCheckTimeout = errors.New("still running after timeout")

type check struct {
	name    string
	fn      Check
	timeout time.Duration

	mu       sync.Mutex
	inflight *checkRun
	failing  atomic.Bool
}

// checkRun is one execution of a check, shared by every probe that arrives while it runs.
type checkRun struct {
	ctx  context.Context // carries the run's timeout
	done chan struct{}
	err  error
}

type report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

type health struct {
	ready      atomic.Bool
	checks     []*check
	logger     *slog.Logger
	showErrors bool
}

func hideError(error) string { return "fail" }

func showError(err error) string { return "error: " + err.Error() }

func ping(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, "pong")
}

func (h *health) liveness(w http.ResponseWriter, _ *http.Request) {
	writeReport(w, report{Status: statusOK})
}

// readiness hides error details: in shared mode it is public, and errors can carry internal addresses.
func (h *health) readiness(w http.ResponseWriter, r *http.Request) {
	if !h.ready.Load() {
		writeReport(w, report{Status: statusUnavailable})
		return
	}
	writeReport(w, h.run(r.Context(), hideError))
}

// detailed shows error messages only on a separate admin server; on the app's port they would reach the internet with their internal addresses.
func (h *health) detailed(w http.ResponseWriter, r *http.Request) {
	describe := hideError
	if h.showErrors {
		describe = showError
	}
	writeReport(w, h.run(r.Context(), describe))
}

func (h *health) run(ctx context.Context, describe func(error) string) report {
	errs := make([]error, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		wg.Go(func() { errs[i] = c.run(ctx) })
	}
	wg.Wait()

	rep := report{Status: statusOK, Checks: make(map[string]string, len(h.checks))}
	for i, c := range h.checks {
		h.logTransition(ctx, c, errs[i])
		if errs[i] != nil {
			rep.Status = statusUnavailable
			rep.Checks[c.name] = describe(errs[i])
			continue
		}
		rep.Checks[c.name] = statusOK
	}
	return rep
}

// logTransition is where a check's error can be seen without an admin server; only changes are logged, because probes repeat every few seconds.
func (h *health) logTransition(ctx context.Context, c *check, err error) {
	if ctx.Err() != nil {
		// The probe went away, which says nothing about the dependency.
		return
	}
	failing := err != nil
	if c.failing.Swap(failing) == failing {
		return
	}
	if failing {
		h.logger.WarnContext(ctx, "readiness check failing", "check", c.name, "err", err.Error())
		return
	}
	h.logger.InfoContext(ctx, "readiness check recovered", "check", c.name)
}

// run joins the execution in progress instead of starting another, so a check that ignores ctx
// costs at most one goroutine however often it is probed.
func (c *check) run(ctx context.Context) error {
	c.mu.Lock()
	run := c.inflight
	if run == nil {
		run = c.start(ctx)
	}
	c.mu.Unlock()

	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	case <-run.ctx.Done():
		// The run cancels its ctx once it finishes, so a finished run can land here too.
		select {
		case <-run.done:
			return run.err
		default:
			return errCheckTimeout
		}
	}
}

// start must be called with c.mu held.
func (c *check) start(ctx context.Context) *checkRun {
	// Other probes share this run, so one probe going away must not cancel it.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	run := &checkRun{ctx: runCtx, done: make(chan struct{})}
	c.inflight = run
	go func() {
		defer cancel()
		run.err = c.call(runCtx)
		c.mu.Lock()
		c.inflight = nil
		c.mu.Unlock()
		close(run.done)
	}()
	return run
}

func (c *check) call(ctx context.Context) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return c.fn(ctx)
}

func writeReport(w http.ResponseWriter, rep report) {
	code := http.StatusOK
	if rep.Status != statusOK {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	// A failed write means the client went away; there is nobody left to tell.
	_ = json.NewEncoder(w).Encode(rep)
}
