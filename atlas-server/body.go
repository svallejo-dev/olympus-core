package atlas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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

// Decode reads a JSON request body of up to 1 MiB into a T; its errors carry a 400, 413 or 415 status.
func Decode[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	return DecodeLimit[T](w, r, defaultMaxBodyBytes)
}

// DecodeLimit is Decode with a body limit of limit bytes.
func DecodeLimit[T any](w http.ResponseWriter, r *http.Request, limit int64) (T, error) {
	var v, zero T
	if !isJSON(r.Header.Get("Content-Type")) {
		return zero, Errorf(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(&v); err != nil {
		return zero, decodeError(err)
	}
	err := dec.Decode(&struct{}{})
	if errors.Is(err, io.EOF) {
		return v, nil
	}
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		return zero, decodeError(err)
	}
	return zero, Errorf(http.StatusBadRequest, "request body must contain a single JSON value")
}

// isJSON accepts a missing Content-Type, which is what many HTTP clients send by default.
func isJSON(contentType string) bool {
	if contentType == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
}

func decodeError(err error) error {
	if tooLarge, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return Errorf(http.StatusRequestEntityTooLarge, "request body exceeds %d bytes", tooLarge.Limit)
	}
	if errors.Is(err, io.EOF) {
		return Errorf(http.StatusBadRequest, "request body is empty")
	}
	return Errorf(http.StatusBadRequest, "invalid JSON in request body: %w", err)
}
