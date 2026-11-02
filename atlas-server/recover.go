package atlas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// callSafely runs fn and turns a panic into its error, logging the stack, which the error alone would lose.
func callSafely(ctx context.Context, logger *slog.Logger, fn func() error, attrs ...slog.Attr) (err error) {
	defer func() {
		if v := recover(); v != nil {
			logPanic(ctx, logger, v, attrs...)
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return fn()
}

func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	respond := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
	return recoverPanicsWith(logger, respond, next)
}

func recoverPanicsWith(logger *slog.Logger, respond func(http.ResponseWriter, *http.Request), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rec := recorderFor(w)
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			logPanic(ctx, logger, v,
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			if rec.status != 0 {
				// Headers are gone; aborting makes the client see a broken response, not a truncated one that looks complete.
				panic(http.ErrAbortHandler)
			}
			respond(rec, r)
		}()
		next.ServeHTTP(rec, r)
	})
}

func logPanic(ctx context.Context, logger *slog.Logger, v any, attrs ...slog.Attr) {
	attrs = append(attrs,
		slog.String("panic", fmt.Sprint(v)),
		slog.String("stack", string(debug.Stack())),
	)
	logger.LogAttrs(ctx, slog.LevelError, "panic recovered", attrs...)
}
