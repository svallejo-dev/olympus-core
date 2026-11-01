package atlas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

type workerResult struct {
	index int
	err   error
}

// workerGroup runs the WithBackground workers; only Run's goroutine reads results and stopped.
type workerGroup struct {
	workers []namedFunc
	cancel  context.CancelFunc
	results chan workerResult
	stopped []bool
}

func startWorkers(ctx context.Context, logger *slog.Logger, workers []namedFunc) *workerGroup {
	// Not canceled with ctx: workers keep going through the drain delay, until stop cancels them.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	g := &workerGroup{
		workers: workers,
		cancel:  cancel,
		results: make(chan workerResult, len(workers)),
		stopped: make([]bool, len(workers)),
	}
	for i, w := range workers {
		go func() {
			err := callSafely(ctx, logger, func() error { return w.fn(ctx) }, slog.String("background", w.name))
			if err != nil {
				err = fmt.Errorf("background worker %q: %w", w.name, err)
			}
			g.results <- workerResult{index: i, err: err}
		}()
	}
	return g
}

func (g *workerGroup) record(res workerResult) {
	g.stopped[res.index] = true
}

// stop cancels the workers and waits for them until ctx is done; an error wrapping context.Canceled is a clean stop.
func (g *workerGroup) stop(ctx context.Context) error {
	g.cancel()
	var errs []error
	for slices.Contains(g.stopped, false) {
		select {
		case res := <-g.results:
			g.record(res)
			if res.err != nil && !errors.Is(res.err, context.Canceled) {
				errs = append(errs, res.err)
			}
		case <-ctx.Done():
			for i, w := range g.workers {
				if !g.stopped[i] {
					errs = append(errs, fmt.Errorf("background worker %q did not stop: %w", w.name, ctx.Err()))
				}
			}
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}
