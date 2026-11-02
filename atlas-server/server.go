package atlas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 1 << 20
)

type server struct {
	name     string
	listener net.Listener
	http     *http.Server
}

func newServer(name string, ln net.Listener, h http.Handler, cfg *config) server {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(cfg.logger.Handler(), slog.LevelError),
	}
	if cfg.serverConfig != nil {
		cfg.serverConfig(srv)
	}
	return server{name: name, listener: ln, http: srv}
}

func (s server) serve() error {
	if err := s.http.Serve(s.listener); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s on %s: %w", s.name, s.listener.Addr(), err)
	}
	return nil
}

func (s server) shutdown(ctx context.Context) error {
	if err := s.http.Shutdown(ctx); err != nil {
		return errors.Join(fmt.Errorf("shutdown %s server: %w", s.name, err), s.http.Close())
	}
	return nil
}

// closeListeners skips nil listeners.
func closeListeners(lns ...net.Listener) {
	for _, ln := range lns {
		if ln != nil {
			_ = ln.Close()
		}
	}
}

func listen(ctx context.Context, ln net.Listener, addr string) (net.Listener, error) {
	if ln != nil {
		return ln, nil
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}

var opsPaths = []string{"/health", "/ping", "/liveness", "/readiness", "/metrics"}

// opsRouter serves the ops endpoints and hands every other request to next untouched: no path
// cleaning or redirects, unlike mounting the app under a ServeMux.
type opsRouter struct {
	probes   map[string]http.Handler // GET and HEAD only, so the app keeps other methods on these paths
	exact    map[string]http.Handler
	subtrees []opsHandler // longest pattern first
	next     http.Handler
}

func newOpsRouter(h *health, metrics http.Handler, cfg *config, next http.Handler) *opsRouter {
	wrap := func(h http.Handler) http.Handler { return recoverPanics(cfg.logger, h) }
	o := &opsRouter{
		probes: map[string]http.Handler{
			"/health":    wrap(http.HandlerFunc(h.detailed)),
			"/ping":      wrap(http.HandlerFunc(ping)),
			"/liveness":  wrap(http.HandlerFunc(h.liveness)),
			"/readiness": wrap(http.HandlerFunc(h.readiness)),
			"/metrics":   wrap(metrics),
		},
		exact: make(map[string]http.Handler),
		next:  next,
	}
	for _, oh := range cfg.opsHandlers {
		if strings.HasSuffix(oh.pattern, "/") {
			o.subtrees = append(o.subtrees, opsHandler{pattern: oh.pattern, handler: wrap(oh.handler)})
			continue
		}
		o.exact[oh.pattern] = wrap(oh.handler)
	}
	slices.SortFunc(o.subtrees, func(a, b opsHandler) int { return len(b.pattern) - len(a.pattern) })
	return o
}

func (o *opsRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := o.match(r); h != nil {
		h.ServeHTTP(w, r)
		return
	}
	o.next.ServeHTTP(w, r)
}

func (o *opsRouter) match(r *http.Request) http.Handler {
	p := r.URL.Path
	if h, ok := o.probes[p]; ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		return h
	}
	if h, ok := o.exact[p]; ok {
		return h
	}
	for _, st := range o.subtrees {
		if strings.HasPrefix(p, st.pattern) {
			return st.handler
		}
	}
	return nil
}

func validateOpsPattern(pattern string) error {
	clean := path.Clean(pattern)
	if strings.HasSuffix(pattern, "/") && clean != "/" {
		clean += "/"
	}
	switch {
	case !strings.HasPrefix(pattern, "/"):
		return fmt.Errorf("ops handler %q: pattern must be a path, without method or host", pattern)
	case strings.ContainsAny(pattern, "{}"):
		return fmt.Errorf("ops handler %q: wildcards are not supported", pattern)
	case clean != pattern:
		return fmt.Errorf("ops handler %q: path is not clean, want %q", pattern, clean)
	case pattern == "/":
		return fmt.Errorf("ops handler %q: would take every path of the server", pattern)
	case slices.Contains(opsPaths, pattern):
		return fmt.Errorf("ops handler %q: path is reserved for atlas", pattern)
	}
	return nil
}

func appHandler(handler http.Handler, tel *telemetry, cfg *config) http.Handler {
	h := recoverPanics(cfg.logger, handler)
	if cfg.accessLog {
		h = logRequests(cfg.logger, h)
	}
	return tel.instrument(withRequestID(h))
}
