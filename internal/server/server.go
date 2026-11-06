// Package server implements the Stavewell HTTP API defined in
// api/openapi.yaml.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/buildinfo"
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
	h := api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseRouter: http.NewServeMux(),
		// Parameter errors (for example, a malformed query parameter) use the
		// same handler as other invalid requests.
		ErrorHandlerFunc: requestErrorHandler(logger),
	})
	return logRequests(logger, secureHeaders(h))
}

// requestErrorHandler responds to an invalid request without revealing
// internal details to the caller. The details are logged instead.
func requestErrorHandler(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Warn("invalid request", "method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "invalid request", http.StatusBadRequest)
	}
}

// responseErrorHandler responds to a server-side failure without revealing
// internal details to the caller. The details are logged instead.
func responseErrorHandler(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("internal error", "method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
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

// Unwrap lets http.ResponseController reach the underlying ResponseWriter,
// for features such as flushing and per-request deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// logRequests logs one line per request. It logs only the method, path,
// status and duration: never query strings, headers or bodies, which can
// contain credentials.
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
		)
	})
}
