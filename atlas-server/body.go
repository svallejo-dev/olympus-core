package atlas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
)

const defaultMaxBodyBytes = 1 << 20

// JSON writes v as a JSON response; an encoding error is returned before anything is written.
func JSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A failed write means the client went away; there is nobody left to tell.
	_, _ = w.Write(append(body, '\n'))
	return nil
}

// Decode reads a JSON request body of up to 1 MiB into a T, ignoring unknown fields; its errors carry a 4xx status, or a 500 when reading the body fails.
func Decode[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	return DecodeLimit[T](w, r, defaultMaxBodyBytes)
}

// DecodeLimit is Decode with a body limit of limit bytes.
func DecodeLimit[T any](w http.ResponseWriter, r *http.Request, limit int64) (T, error) {
	var v, zero T
	if !isJSON(r.Header.Get("Content-Type")) {
		return zero, Errorf(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
	}
	if r.Body == nil {
		return zero, Errorf(http.StatusBadRequest, "request body is empty")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(&v); err != nil {
		return zero, decodeError(r, err)
	}
	err := dec.Decode(&struct{}{})
	switch {
	case errors.Is(err, io.EOF):
		return v, nil
	case isReadFailure(err):
		return zero, decodeError(r, err)
	default:
		return zero, Errorf(http.StatusBadRequest, "request body must contain a single JSON value")
	}
}

// isJSON rejects a missing Content-Type too: a cross-site POST of an untyped blob skips the CORS preflight.
func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
}

func isReadFailure(err error) bool {
	_, tooLarge := errors.AsType[*http.MaxBytesError](err)
	_, network := errors.AsType[net.Error](err)
	return tooLarge || network
}

// decodeError never passes on the decoder's own message: it can carry Go type names and network addresses.
func decodeError(r *http.Request, err error) error {
	if tooLarge, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return Errorf(http.StatusRequestEntityTooLarge, "request body exceeds %d bytes", tooLarge.Limit)
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return ctxErr
		}
		if netErr.Timeout() {
			return Errorf(http.StatusRequestTimeout, "timed out reading the request body")
		}
		return fmt.Errorf("read request body: %w", err)
	}
	if errors.Is(err, io.EOF) {
		return Errorf(http.StatusBadRequest, "request body is empty")
	}
	if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && typeErr.Field != "" {
		return Errorf(http.StatusBadRequest, "field %q in the request body has the wrong type", typeErr.Field)
	}
	if e, ok := errors.AsType[httpStatuser](err); ok {
		return e
	}
	return Errorf(http.StatusBadRequest, "request body is not valid JSON for this request")
}
