// Package atlas runs an http.Handler as a production HTTP service with health checks, telemetry and graceful shutdown.
package atlas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Run serves handler (http.DefaultServeMux if nil) and the ops endpoints until ctx ends, SIGINT/SIGTERM arrives or a server or worker fails, then shuts down gracefully; nil means a clean stop.
func Run(ctx context.Context, handler http.Handler, opts ...Option) error {
	cfg := newConfig(opts)
	if err := cfg.validate(); err != nil {
		// Run promises to close the listeners it is given, even when it never serves.
		closeListeners(cfg.listener, cfg.adminListener)
		return err
	}
	if handler == nil {
		handler = http.DefaultServeMux
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := newService(ctx, handler, cfg)
	if err != nil {
		return err
	}
	return svc.run(ctx, stop)
}

type service struct {
	cfg     config
	health  *health
	tel     *telemetry
	servers []server
}

func newService(ctx context.Context, handler http.Handler, cfg config) (*service, error) {
	appLn, adminLn, err := openListeners(ctx, &cfg)
	if err != nil {
		return nil, err
	}
	tel, err := newTelemetry(ctx, &cfg)
	if err != nil {
		closeListeners(appLn, adminLn)
		return nil, err
	}

	h := &health{checks: cfg.checks}
	app := appHandler(handler, tel, &cfg)
	svc := &service{cfg: cfg, health: h, tel: tel}
	if adminLn == nil {
		ops := newOpsRouter(h, tel.metricsHandler(), &cfg, app)
		svc.servers = []server{newServer("app", appLn, ops, &cfg)}
	} else {
		ops := newOpsRouter(h, tel.metricsHandler(), &cfg, http.NotFoundHandler())
		svc.servers = []server{newServer("app", appLn, app, &cfg), newServer("admin", adminLn, ops, &cfg)}
	}
	return svc, nil
}

// openListeners returns a nil admin listener when the ops endpoints share the app server.
func openListeners(ctx context.Context, cfg *config) (app, admin net.Listener, err error) {
	app, err = listen(ctx, cfg.listener, cfg.addr)
	if err != nil {
		closeListeners(cfg.adminListener)
		return nil, nil, err
	}
	if cfg.adminListener == nil && cfg.adminAddr == "" {
		return app, nil, nil
	}
	admin, err = listen(ctx, cfg.adminListener, cfg.adminAddr)
	if err != nil {
		_ = app.Close()
		return nil, nil, err
	}
	return app, admin, nil
}

func (s *service) run(ctx context.Context, stopSignals context.CancelFunc) error {
	serveErrs := make(chan error, len(s.servers))
	for _, srv := range s.servers {
		go func() { serveErrs <- srv.serve() }()
	}
	workers := startWorkers(ctx, s.cfg.logger, s.cfg.workers)
	s.health.ready.Store(true)
	s.logListening(ctx)

	var cause error
	pending := len(s.servers)
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case cause = <-serveErrs:
			pending--
			break wait
		case res := <-workers.results:
			workers.record(res)
			if res.err != nil {
				cause = res.err
				break wait
			}
		}
	}
	// From here on a second signal kills the process the default way.
	stopSignals()
	s.health.ready.Store(false)
	s.cfg.logger.InfoContext(ctx, "shutting down", "delay", s.cfg.shutdownDelay, "timeout", s.cfg.shutdownTimeout)

	err := s.shutdown(context.WithoutCancel(ctx), workers)
	for range pending {
		cause = errors.Join(cause, <-serveErrs)
	}
	s.cfg.logger.InfoContext(ctx, "stopped")
	return errors.Join(cause, err)
}

func (s *service) logListening(ctx context.Context) {
	attrs := []any{"addr", s.servers[0].listener.Addr().String()}
	if len(s.servers) > 1 {
		attrs = append(attrs, "admin_addr", s.servers[1].listener.Addr().String())
	}
	attrs = append(attrs, "service", s.tel.serviceName)
	s.cfg.logger.InfoContext(ctx, "listening", attrs...)
}

// shutdown waits out the drain delay, then gives servers, workers, hooks and telemetry one shared timeout.
func (s *service) shutdown(ctx context.Context, workers *workerGroup) error {
	time.Sleep(s.cfg.shutdownDelay)
	ctx, cancel := context.WithTimeout(ctx, s.cfg.shutdownTimeout)
	defer cancel()

	errs := make([]error, len(s.servers))
	var wg sync.WaitGroup
	for i, srv := range s.servers {
		wg.Go(func() { errs[i] = srv.shutdown(ctx) })
	}
	workersErr := workers.stop(ctx)
	wg.Wait()
	errs = append(errs, workersErr)

	for i := len(s.cfg.hooks) - 1; i >= 0; i-- {
		hook := s.cfg.hooks[i]
		err := callSafely(ctx, s.cfg.logger, func() error { return hook.fn(ctx) }, slog.String("shutdown_hook", hook.name))
		if err != nil {
			errs = append(errs, fmt.Errorf("shutdown hook %q: %w", hook.name, err))
		}
	}
	errs = append(errs, s.tel.shutdown(ctx))
	return errors.Join(errs...)
}
