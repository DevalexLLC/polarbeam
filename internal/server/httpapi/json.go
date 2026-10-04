// Shared request/response plumbing for every handler in the package: the
// uniform JSON error shape, strict request decoding, and the mapping from
// the store's typed admin errors onto HTTP statuses.
// (Not the package doc — that lives in probeconfig.go.)

package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/devalexllc/polarbeam/internal/server/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("httpapi: encode response", "err", err)
	}
}

// writeError emits the uniform API error shape {"error": "..."}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs the real cause and returns an opaque 500 (internals
// belong in the server log, not in a browser).
func internalError(w http.ResponseWriter, what string, err error) {
	slog.Error("httpapi: "+what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// decodeStrict decodes exactly one JSON object, rejecting unknown fields
// (a client bug or version skew — never silently dropped) and trailing
// data, and refuses a NUL character in any decoded string (see hasNUL). It
// writes the 400/413 itself; callers bail on false.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if isBodyTooLarge(w, err) {
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return false
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if isBodyTooLarge(w, err) {
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid body: trailing data after JSON object")
		return false
	}
	if hasNUL(reflect.ValueOf(v)) {
		writeError(w, http.StatusBadRequest, nulBodyError)
		return false
	}
	return true
}

const nulBodyError = "invalid body: strings must not contain NUL characters"

// hasNUL reports whether any string reachable from v contains a NUL byte.
// JSON can carry one as \u0000, but Postgres text cannot store it: past this
// check it would surface as SQLSTATE 22021, an opaque 500 (the same
// boundary withoutNUL holds for URLs). A struct field tagged `nul:"allow"`
// is skipped: it is hashed and never stored as text (a password), and
// refusing a NUL there would lock out a credential login still accepts.
func hasNUL(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.IndexByte(v.String(), 0) >= 0
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && hasNUL(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).Tag.Get("nul") == "allow" {
				continue
			}
			if hasNUL(v.Field(i)) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if hasNUL(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if hasNUL(it.Key()) || hasNUL(it.Value()) {
				return true
			}
		}
	}
	return false
}

// isBodyTooLarge writes a 413 and reports true when err is the body-limit
// middleware's overflow (withBodyLimit); any other error is the caller's.
func isBodyTooLarge(w http.ResponseWriter, err error) bool {
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		return false
	}
	writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	return true
}

// writeStoreError maps the store's typed admin errors onto HTTP statuses;
// anything untyped is an internal error (logged, opaque 500).
func writeStoreError(w http.ResponseWriter, what string, err error) {
	var inUse store.InUseError
	switch {
	case errors.As(err, &inUse):
		writeError(w, http.StatusConflict, inUse.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		internalError(w, what, err)
	}
}
