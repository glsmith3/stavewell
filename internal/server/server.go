// Package server implements the Stavewell HTTP API defined in
// api/openapi.yaml.
package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/buildinfo"
	"github.com/glsmith3/stavewell/internal/ids"
)

// Server implements api.StrictServerInterface.
type Server struct{}

var _ api.StrictServerInterface = Server{}

// GetHealth reports that the server is running.
func (Server) GetHealth(context.Context, api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	return api.GetHealth200JSONResponse{Status: api.Ok}, nil
}

// GetVersion reports the running version and source commit.
func (Server) GetVersion(context.Context, api.GetVersionRequestObject) (api.GetVersionResponseObject, error) {
	return api.GetVersion200JSONResponse{
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit(),
	}, nil
}

// Handler returns the HTTP handler for the API, with middleware applied.
func Handler(logger *slog.Logger) http.Handler {
	strict := api.NewStrictHandlerWithOptions(Server{}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  requestErrorHandler(logger),
		ResponseErrorHandlerFunc: responseErrorHandler(logger),
	})
	mux := http.NewServeMux()
	api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseRouter: mux,
		// Parameter errors (for example, a malformed query parameter) use the
		// same handler as other invalid requests.
		ErrorHandlerFunc: requestErrorHandler(logger),
	})
	return withRequestID(
		logRequests(logger,
			secureHeaders(
				recoverPanics(logger,
					routeProblems(mux)))))
}

// requestErrorHandler responds to an invalid request without revealing
// internal details to the caller. The details are logged instead.
func requestErrorHandler(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Warn("invalid request",
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", requestID(r.Context()),
			"error", err,
		)
		writeProblem(w, r, problemInvalidRequest, detailInvalidRequest)
	}
}

// responseErrorHandler responds to a server-side failure without revealing
// internal details to the caller. The details are logged instead.
func responseErrorHandler(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("internal error",
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", requestID(r.Context()),
			"error", err,
		)
		writeProblem(w, r, problemInternal, detailInternal)
	}
}

// requestIDKey is the context key for the request ID.
type requestIDKey struct{}

// withRequestID gives every request a new random ID. Error responses include
// it, and the request's log lines record it, so an error a caller sees can be
// matched with the server log.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), requestIDKey{}, ids.New("req"))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestID returns the request ID stored by withRequestID, or an empty
// string if there isn't one.
func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// routeProblems serves requests through mux, but answers requests that match
// no route with Problem Details responses. On its own, ServeMux answers them
// in plain text.
func routeProblems(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			// ServeHTTP, not h.ServeHTTP, so that path values are set.
			mux.ServeHTTP(w, r)
			return
		}
		// No route matched. Run the mux's own handler against a capture to
		// learn whether this is a 404, a 405 (with its Allow header) or a
		// redirect to a cleaned-up path.
		c := &captureWriter{header: make(http.Header)}
		h.ServeHTTP(c, r)
		switch c.status {
		case http.StatusNotFound:
			writeProblem(w, r, problemNotFound, detailNotFound)
		case http.StatusMethodNotAllowed:
			w.Header().Set("Allow", c.header.Get("Allow"))
			writeProblem(w, r, problemMethodNotAllowed, detailMethodNotAllowed)
		default:
			// Anything else, such as a redirect, is passed on unchanged.
			for k, v := range c.header {
				w.Header()[k] = v
			}
			w.WriteHeader(c.status)
			_, _ = w.Write(c.body.Bytes())
		}
	})
}

// captureWriter records a response instead of sending it.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

// recoverPanics turns a panic in a handler into an internal-error response,
// so the caller gets a Problem Details response rather than a dropped
// connection. The panic and its stack trace are logged, never sent.
//
// If the response has already started, a Problem Details response can't be
// sent, so the connection is aborted instead.
func recoverPanics(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			logger.Error("panic",
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", requestID(r.Context()),
				"panic", fmt.Sprint(v),
				"stack", string(debug.Stack()),
			)
			if t, ok := w.(interface{ headerWritten() bool }); ok && t.headerWritten() {
				panic(http.ErrAbortHandler)
			}
			writeProblem(w, r, problemInternal, detailInternal)
		}()
		next.ServeHTTP(w, r)
	})
}

// secureHeaders sets headers that every API response carries.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code actually sent to the client, for
// logging. Only the first status counts: once headers are sent, later calls
// to WriteHeader can't change what the client received.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// Writing a body without calling WriteHeader sends 200.
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// headerWritten reports whether the response has started. recoverPanics uses
// it to decide whether an error response can still be sent.
func (r *statusRecorder) headerWritten() bool {
	return r.wroteHeader
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter,
// for features such as flushing and per-request deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// logRequests logs one line per request. It logs only the method, path,
// status, duration and request ID: never query strings, headers or bodies,
// which can contain credentials.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", requestID(r.Context()),
		)
	})
}
