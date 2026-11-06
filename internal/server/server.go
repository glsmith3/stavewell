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
		// Never send internal error details to the caller. Log them instead.
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("invalid request", "method", r.Method, "path", r.URL.Path, "error", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("internal error", "method", r.Method, "path", r.URL.Path, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		},
	})
	h := api.HandlerFromMux(strict, http.NewServeMux())
	return logRequests(logger, secureHeaders(h))
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

// statusRecorder captures the response status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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
