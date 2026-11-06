// Package server implements the Stavewell HTTP API defined in
// api/openapi.yaml.
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/apikey"
	"github.com/glsmith3/stavewell/internal/buildinfo"
	"github.com/glsmith3/stavewell/internal/ids"
	"github.com/glsmith3/stavewell/internal/store"
)

// maxBodyBytes is the largest request body accepted: 64 KiB.
const maxBodyBytes = 64 << 10

// Server implements api.StrictServerInterface.
type Server struct {
	db     *store.Store
	logger *slog.Logger
	now    func() time.Time
}

var _ api.StrictServerInterface = (*Server)(nil)

// GetHealth reports that the server is running.
func (*Server) GetHealth(context.Context, api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	return api.GetHealth200JSONResponse{Status: api.Ok}, nil
}

// GetVersion reports the running version and source commit.
func (*Server) GetVersion(context.Context, api.GetVersionRequestObject) (api.GetVersionResponseObject, error) {
	return api.GetVersion200JSONResponse{
		Version: buildinfo.Version,
		Commit:  buildinfo.Commit(),
	}, nil
}

// Handler returns the HTTP handler for the API, with middleware applied.
func Handler(logger *slog.Logger, db *store.Store) http.Handler {
	s := &Server{db: db, logger: logger, now: time.Now}
	strict := api.NewStrictHandlerWithOptions(s, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  bodyErrorHandler(logger),
		ResponseErrorHandlerFunc: responseErrorHandler(logger),
	})
	mux := http.NewServeMux()
	api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseRouter:       mux,
		ErrorHandlerFunc: parameterErrorHandler(logger),
	})
	return withMiddleware(logger, mux, s.authenticate)
}

// authFunc checks a request's API key for scope. See Server.authenticate.
type authFunc func(w http.ResponseWriter, r *http.Request, scope string) (*http.Request, bool)

// withMiddleware wraps mux in the middleware that every request passes
// through, outermost first.
func withMiddleware(logger *slog.Logger, mux *http.ServeMux, auth authFunc) http.Handler {
	return withRequestID(
		logRequests(logger,
			secureHeaders(
				recoverPanics(logger,
					route(logger, mux, auth)))))
}

// logPath returns the request's path for a log line. A path that looks like
// it contains an API key, such as one pasted into a URL by mistake, isn't
// logged.
func logPath(r *http.Request) string {
	if apikey.LooksLikeKey(r.URL.Path) {
		return "(not logged: looks like it contains an API key)"
	}
	return r.URL.Path
}

// logInvalid logs why a request was invalid. The details stay in the log.
func logInvalid(logger *slog.Logger, r *http.Request, err error) {
	logger.Warn("invalid request",
		"method", r.Method,
		"path", logPath(r),
		"request_id", requestID(r.Context()),
		"error", err,
	)
}

// parameterErrorHandler responds to a path or query parameter that couldn't
// be read, without revealing internal details to the caller.
func parameterErrorHandler(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logInvalid(logger, r, err)
		detail := detailInvalidParameter
		var format *api.InvalidParamFormatError
		var many *api.TooManyValuesForParamError
		// The runtime library reports a repeated parameter as a format
		// error, so both kinds are checked.
		switch {
		case errors.As(err, &format) && format.ParamName == "limit",
			errors.As(err, &many) && many.ParamName == "limit":
			detail = detailInvalidLimit
		case errors.As(err, &format) && format.ParamName == "cursor",
			errors.As(err, &many) && many.ParamName == "cursor":
			writeProblem(w, r, problemInvalidCursor, detailInvalidCursor)
			return
		case errors.As(err, &format) && format.ParamName == "sequence":
			detail = detailInvalidSequence
		}
		writeProblem(w, r, problemInvalidRequest, detail)
	}
}

// bodyErrorHandler responds to a request body that couldn't be read, without
// revealing internal details to the caller.
func bodyErrorHandler(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logInvalid(logger, r, err)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, r, problemRequestTooLarge, detailRequestTooLarge)
			return
		}
		writeProblem(w, r, problemInvalidRequest, detailInvalidJSON)
	}
}

// responseErrorHandler responds to a server-side failure without revealing
// internal details to the caller. The details are logged instead.
func responseErrorHandler(logger *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("internal error",
			"method", r.Method,
			"path", logPath(r),
			"request_id", requestID(r.Context()),
			"error", err,
		)
		writeProblem(w, r, problemInternal, detailInternal)
	}
}

// requestInfo holds what the request's log line needs from inner handlers.
type requestInfo struct {
	id    string // the request ID
	keyID string // the API key's public ID, once the key is checked
}

// requestInfoKey is the context key for the request's *requestInfo.
type requestInfoKey struct{}

// withRequestID gives every request a new random ID. Error responses include
// it, and the request's log lines record it, so an error a caller sees can be
// matched with the server log.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), requestInfoKey{}, &requestInfo{id: ids.New("req")})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// infoFrom returns the request's *requestInfo, or nil if there isn't one.
func infoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

// requestID returns the request ID stored by withRequestID, or an empty
// string if there isn't one.
func requestID(ctx context.Context) string {
	if info := infoFrom(ctx); info != nil {
		return info.id
	}
	return ""
}

// route serves requests through mux. For a route in requiredScopes, it first
// checks the API key with auth. For a route in jsonBodies, it then reads and
// checks the request body. It answers requests that match no route with
// Problem Details responses; on its own, ServeMux answers them in plain text.
func route(logger *slog.Logger, mux *http.ServeMux, auth authFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "*" {
			// "*" is a valid request target only for OPTIONS, which
			// net/http answers before any handler runs. ServeMux.ServeHTTP
			// rejects it for other methods, but mux.Handler doesn't, so
			// reject it here.
			w.Header().Set("Connection", "close")
			writeProblem(w, r, problemInvalidRequest, detailInvalidRequest)
			return
		}
		h, pattern := mux.Handler(r)
		if pattern != "" {
			if scope, ok := requiredScopes[pattern]; ok {
				if r, ok = auth(w, r, scope); !ok {
					return
				}
			}
			if check, ok := jsonBodies[pattern]; ok {
				if r, ok = readBody(logger, w, r, check); !ok {
					return
				}
			}
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
				"path", logPath(r),
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

// FlushError sends buffered response data to the client. It's defined here,
// rather than reached through Unwrap, because flushing sends the headers, so
// the response has started.
func (r *statusRecorder) FlushError() error {
	if err := http.NewResponseController(r.ResponseWriter).Flush(); err != nil {
		return err
	}
	r.wroteHeader = true
	return nil
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
// status, duration, request ID and, once checked, the API key's public ID:
// never query strings, headers, bodies or keys, which can contain
// credentials.
//
// If the request is aborted by a panic, the line also has "aborted": true,
// and the status is the one sent before the abort, or 0 if none was sent.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		completed := false
		// Log from a defer so that the line is written even when a panic
		// aborts the request.
		defer func() {
			status := rec.status
			if !completed && !rec.wroteHeader {
				status = 0
			}
			attrs := []any{
				"method", r.Method,
				"path", logPath(r),
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", requestID(r.Context()),
			}
			if info := infoFrom(r.Context()); info != nil && info.keyID != "" {
				attrs = append(attrs, "key_id", info.keyID)
			}
			if !completed {
				attrs = append(attrs, "aborted", true)
			}
			logger.Info("request", attrs...)
		}()
		next.ServeHTTP(rec, r)
		completed = true
	})
}
