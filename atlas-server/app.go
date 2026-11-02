package atlas

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// HandlerFunc handles a request and may fail; App turns a returned error into the response.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Middleware wraps a handler; any func(http.Handler) http.Handler qualifies.
type Middleware = func(http.Handler) http.Handler

// App is an HTTP API: routes, middleware and error responses on an http.ServeMux, served by Run.
// Register everything before Run or the first request; registering is not safe for concurrent use.
type App struct {
	*Router
	cfg     config
	once    sync.Once
	handler http.Handler
}

// New returns an App configured with the same options as the package-level Run.
func New(opts ...Option) *App {
	cfg := newConfig(opts)
	table := &routeTable{mux: http.NewServeMux(), logger: cfg.logger, writeError: cfg.errorHandler}
	return &App{Router: &Router{table: table, root: true}, cfg: cfg}
}

// Run serves the App and the ops endpoints as the package-level Run does; a route that an ops endpoint would shadow makes it fail before listening.
func (a *App) Run(ctx context.Context) error {
	a.once.Do(a.build)
	cfg := a.cfg
	cfg.routes = a.table.routes
	return serve(ctx, a, cfg)
}

// ServeHTTP makes the App an http.Handler, for httptest or for mounting it elsewhere.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.once.Do(a.build)
	a.handler.ServeHTTP(w, r)
}

func (a *App) build() {
	a.sealed = true
	respond := func(w http.ResponseWriter, r *http.Request) {
		a.table.writeError(w, r, Errorf(http.StatusInternalServerError, "internal server error"))
	}
	a.handler = recoverPanicsWith(a.cfg.logger, respond, wrap(a.table, a.chain))
}

// Router registers routes under a path prefix with its own middleware.
type Router struct {
	table  *routeTable
	prefix string
	chain  []Middleware
	root   bool
	sealed bool
}

// Use adds middleware to everything this Router serves; on the App it also runs for requests no route matches. It panics after a route or group is registered.
func (r *Router) Use(mw ...Middleware) {
	if r.sealed {
		panic("atlas: Use must be called before registering routes or groups")
	}
	r.chain = append(r.chain, mw...)
}

// Group returns a Router that adds prefix, which may be empty, and mw to the routes registered on it.
func (r *Router) Group(prefix string, mw ...Middleware) *Router {
	validatePrefix(prefix)
	r.sealed = true
	return &Router{table: r.table, prefix: r.prefix + prefix, chain: slices.Concat(r.routeChain(), mw)}
}

// Get registers h for GET and HEAD on path, which matches exactly; {name} captures a segment for r.PathValue and {name...} the rest of the path.
func (r *Router) Get(path string, h HandlerFunc, mw ...Middleware) {
	r.Method(http.MethodGet, path, h, mw...)
}

// Post registers h for POST on path.
func (r *Router) Post(path string, h HandlerFunc, mw ...Middleware) {
	r.Method(http.MethodPost, path, h, mw...)
}

// Put registers h for PUT on path.
func (r *Router) Put(path string, h HandlerFunc, mw ...Middleware) {
	r.Method(http.MethodPut, path, h, mw...)
}

// Patch registers h for PATCH on path.
func (r *Router) Patch(path string, h HandlerFunc, mw ...Middleware) {
	r.Method(http.MethodPatch, path, h, mw...)
}

// Delete registers h for DELETE on path.
func (r *Router) Delete(path string, h HandlerFunc, mw ...Middleware) {
	r.Method(http.MethodDelete, path, h, mw...)
}

// Method registers h for method on path; it panics, like http.ServeMux, on an invalid or conflicting route.
func (r *Router) Method(method, path string, h HandlerFunc, mw ...Middleware) {
	if h == nil {
		panic(fmt.Sprintf("atlas: nil handler for %s %s", method, r.prefix+path))
	}
	r.register(method, path, r.table.adapt(h), mw)
}

// Handle registers a standard handler for pattern, "METHOD /path" or "/path" for every method.
func (r *Router) Handle(pattern string, h http.Handler, mw ...Middleware) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = "", pattern
	}
	if h == nil {
		panic(fmt.Sprintf("atlas: nil handler for %s", pattern))
	}
	r.register(method, strings.TrimSpace(path), h, mw)
}

// Mount serves h for every method under prefix, without wildcards, removing prefix from the path h sees.
func (r *Router) Mount(prefix string, h http.Handler, mw ...Middleware) {
	validatePrefix(prefix)
	full := r.prefix + prefix
	switch {
	case prefix == "":
		panic("atlas: Mount needs a prefix")
	case strings.Contains(full, "{"):
		panic(fmt.Sprintf("atlas: Mount prefix %q cannot contain wildcards", full))
	case h == nil:
		panic(fmt.Sprintf("atlas: nil handler for Mount %s", full))
	}
	r.sealed = true
	mounted := wrap(wrap(stripPrefix(full, h), mw), r.routeChain())
	r.table.mux.Handle(full, mounted)
	r.table.mux.Handle(full+"/", mounted)
	r.table.routes = append(r.table.routes, route{path: full})
}

func (r *Router) register(method, path string, h http.Handler, mw []Middleware) {
	if path != "" && !strings.HasPrefix(path, "/") {
		panic(fmt.Sprintf("atlas: path %q must start with /", path))
	}
	r.sealed = true
	full := joinPath(r.prefix, path)
	r.table.mux.Handle(muxPattern(method, full), wrap(wrap(h, mw), r.routeChain()))
	r.table.routes = append(r.table.routes, route{method: method, path: full})
}

// routeChain is empty on the App's root Router, whose middleware wraps the whole table so it also sees unmatched requests.
func (r *Router) routeChain() []Middleware {
	if r.root {
		return nil
	}
	return r.chain
}

// joinPath makes "/" on a group mean the group's own path.
func joinPath(prefix, path string) string {
	switch {
	case prefix == "" && path == "":
		return "/"
	case path == "/" && prefix != "":
		return prefix
	}
	return prefix + path
}

// muxPattern makes a path match exactly: to http.ServeMux a trailing slash means a subtree, and "/" every path.
func muxPattern(method, path string) string {
	if strings.HasSuffix(path, "/") {
		path += "{$}"
	}
	if method == "" {
		return path
	}
	return method + " " + path
}

func validatePrefix(prefix string) {
	if prefix != "" && (!strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/")) {
		panic(fmt.Sprintf("atlas: prefix %q must start with / and not end with /", prefix))
	}
}

// wrap applies chain so that its first middleware runs first.
func wrap(h http.Handler, chain []Middleware) http.Handler {
	for _, mw := range slices.Backward(chain) {
		h = mw(h)
	}
	return h
}

// stripPrefix also turns the mount point itself into "/", which is what a nested router expects.
func stripPrefix(prefix string, h http.Handler) http.Handler {
	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" {
			r.URL.Path, r.URL.RawPath = "/", ""
		}
		h.ServeHTTP(w, r)
	}))
}

type route struct {
	method string // empty means every method
	path   string
}

// routeTable is what an App and its groups share.
type routeTable struct {
	mux        *http.ServeMux
	routes     []route
	logger     *slog.Logger
	writeError func(http.ResponseWriter, *http.Request, error)
}

func (t *routeTable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := t.mux.Handler(r); pattern != "" {
		t.mux.ServeHTTP(w, r)
		return
	}
	render := func(status int) { t.writeError(w, r, unmatchedError(status, r.Method)) }
	t.mux.ServeHTTP(&unmatchedWriter{ResponseWriter: w, render: render}, r)
}

func unmatchedError(status int, method string) error {
	if status == http.StatusMethodNotAllowed {
		return Errorf(status, "method %s is not allowed on this path", method)
	}
	return Errorf(status, "no route matches this path")
}

// unmatchedWriter replaces the plain-text 404 and 405 of http.ServeMux, keeping the Allow header it sets.
type unmatchedWriter struct {
	http.ResponseWriter
	render   func(status int)
	rendered bool
}

func (w *unmatchedWriter) WriteHeader(status int) {
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.rendered = true
	w.render(status)
}

func (w *unmatchedWriter) Write(p []byte) (int, error) {
	if w.rendered {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}
