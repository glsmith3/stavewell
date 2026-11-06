package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/apikey"
)

func TestLogRequestsRecordsFirstStatus(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := logRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		// A second WriteHeader can't change what the client receives, so it
		// must not change what's logged either.
		w.WriteHeader(http.StatusInternalServerError)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x?token=secret", nil))

	var entry struct {
		Status int    `json:"status"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("decoding log line %q: %v", logs.String(), err)
	}
	if entry.Status != http.StatusCreated {
		t.Errorf("logged status = %d, want %d", entry.Status, http.StatusCreated)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Errorf("log contains the query string: %s", logs.String())
	}
}

func TestLogRequestsRecordsImplicit200(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := logRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
		w.WriteHeader(http.StatusInternalServerError) // too late; 200 was sent
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if !strings.Contains(logs.String(), `"status":200`) {
		t.Errorf("log = %s, want status 200", logs.String())
	}
}

func TestStatusRecorderSupportsResponseController(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: underlying, status: http.StatusOK}

	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush() through statusRecorder: %v", err)
	}
	if !underlying.Flushed {
		t.Error("underlying ResponseWriter wasn't flushed")
	}
}

// decodeProblem checks that a response is a Problem Details error and returns
// its body.
func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) api.Problem {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding body %q: %v", w.Body.String(), err)
	}
	return p
}

func TestErrorHandlersHideDetails(t *testing.T) {
	const detail = "open /var/lib/stavewell/data.db: permission denied"
	tests := []struct {
		name       string
		handler    func(*slog.Logger) func(http.ResponseWriter, *http.Request, error)
		wantStatus int
		wantType   string
	}{
		{"parameter error", parameterErrorHandler, http.StatusBadRequest, typeURLBase + "invalid-request"},
		{"body error", bodyErrorHandler, http.StatusBadRequest, typeURLBase + "invalid-request"},
		{"response error", responseErrorHandler, http.StatusInternalServerError, typeURLBase + "internal-error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			w := httptest.NewRecorder()
			h := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.handler(logger)(w, r, errors.New(detail))
			}))

			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			body := w.Body.String()
			if strings.Contains(body, "permission denied") || strings.Contains(body, "/var/lib") {
				t.Errorf("response body reveals internal details: %q", body)
			}
			p := decodeProblem(t, w)
			if p.Type != tt.wantType || p.Status != tt.wantStatus {
				t.Errorf("type, status = %q, %d; want %q, %d", p.Type, p.Status, tt.wantType, tt.wantStatus)
			}
			if p.RequestId == "" {
				t.Error("request_id is empty")
			}
			if !strings.Contains(logs.String(), "permission denied") {
				t.Errorf("details weren't logged: %s", logs.String())
			}
			if !strings.Contains(logs.String(), p.RequestId) {
				t.Errorf("log doesn't contain request ID %q: %s", p.RequestId, logs.String())
			}
		})
	}
}

// TestMiddlewareTurnsPanicIntoProblem runs a panicking handler through the
// full middleware chain, so a wrong middleware order is caught too.
func TestMiddlewareTurnsPanicIntoProblem(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) {
		panic("lost connection to /var/lib/stavewell/data.db")
	})
	w := httptest.NewRecorder()

	withMiddleware(logger, mux, noAuth(t)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "/var/lib") {
		t.Errorf("response body reveals internal details: %q", w.Body.String())
	}
	p := decodeProblem(t, w)
	if p.Type != typeURLBase+"internal-error" {
		t.Errorf("type = %q, want internal-error", p.Type)
	}
	for header, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store"} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	lines := logLines(t, &logs)
	if len(lines) != 2 || lines[0]["msg"] != "panic" || lines[1]["msg"] != "request" {
		t.Fatalf("log lines = %v, want a panic line, then a request line", lines)
	}
	if !strings.Contains(fmt.Sprint(lines[0]["panic"]), "lost connection") {
		t.Errorf("panic line doesn't record the panic: %v", lines[0])
	}
	if lines[1]["status"] != float64(http.StatusInternalServerError) || lines[1]["request_id"] != p.RequestId {
		t.Errorf("request line = %v, want status 500 and request_id %q", lines[1], p.RequestId)
	}
}

// logLines decodes JSON log output, one object per line.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(logs.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestRecoverPanicsAbortsStartedResponse(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := logRequests(logger, recoverPanics(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("failed halfway through")
	})))

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	t.Error("ServeHTTP returned normally, want a panic with http.ErrAbortHandler")
}

func TestRecoverPanicsAbortsAfterFlush(t *testing.T) {
	// Flushing sends the headers, so after a flush an error response can't be
	// sent either.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := logRequests(logger, recoverPanics(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush() = %v", err)
		}
		panic("failed after flushing")
	})))

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	t.Error("ServeHTTP returned normally, want a panic with http.ErrAbortHandler")
}

func TestLogRequestsLogsAbortedRequests(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus float64
	}{
		{"after the response started", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			panic("failed halfway through")
		}, http.StatusOK},
		{"before any response", func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			h := logRequests(logger, recoverPanics(logger, tt.handler))

			func() {
				defer func() { _ = recover() }()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
			}()

			var request map[string]any
			for _, line := range logLines(t, &logs) {
				if line["msg"] == "request" {
					request = line
				}
			}
			if request == nil {
				t.Fatalf("no request log line; log: %s", logs.String())
			}
			if request["aborted"] != true || request["status"] != tt.wantStatus {
				t.Errorf("request line = %v, want aborted true and status %v", request, tt.wantStatus)
			}
		})
	}
}

func TestLogRequestsOmitsAbortedForCompletedRequests(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := logRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if strings.Contains(logs.String(), "aborted") {
		t.Errorf("log line for a completed request mentions aborted: %s", logs.String())
	}
}

func TestStatusRecorderFlushMarksResponseStarted(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	if !rec.headerWritten() {
		t.Error("headerWritten() = false after a flush, want true")
	}
}

func TestRecoverPanicsPassesOnAbortHandler(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := recoverPanics(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	t.Error("ServeHTTP returned normally, want a panic with http.ErrAbortHandler")
}

// noAuth returns an authFunc for routes that don't need an API key. It fails
// the test if it's called.
func noAuth(t *testing.T) authFunc {
	return func(w http.ResponseWriter, r *http.Request, scope string) (*http.Request, bool) {
		t.Errorf("auth called for %s %s, which needs no API key", r.Method, r.URL.Path)
		return r, true
	}
}

func TestRoutePassesOnRedirects(t *testing.T) {
	// A path that needs cleaning matches no route, but ServeMux answers it
	// with a redirect to the cleaned path. That redirect must reach the client
	// unchanged.
	mux := http.NewServeMux()
	plain := httptest.NewRecorder()
	mux.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/a/../b", nil))
	if plain.Code < 300 || plain.Code > 399 {
		t.Fatalf("ServeMux status = %d, want a redirect; test assumption is wrong", plain.Code)
	}

	w := httptest.NewRecorder()
	route(mux, noAuth(t)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/a/../b", nil))

	if w.Code != plain.Code {
		t.Errorf("status = %d, want %d", w.Code, plain.Code)
	}
	if got, want := w.Header().Get("Location"), plain.Header().Get("Location"); got != want || got != "/b" {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// TestRequiredScopesMatchSpec checks requiredScopes against the security
// requirements in api/openapi.yaml: every operation that needs an API key has
// the same scope in both, and nothing else needs one.
func TestRequiredScopesMatchSpec(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fromSpec := make(map[string]string)
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security == nil || len(*op.Security) == 0 {
				continue
			}
			reqs := *op.Security
			if len(reqs) != 1 || len(reqs[0]) != 1 || len(reqs[0]["apiKey"]) != 1 {
				t.Errorf("%s %s: want exactly one apiKey requirement with one scope, got %v", method, path, reqs)
				continue
			}
			fromSpec[method+" "+path] = reqs[0]["apiKey"][0]
		}
	}
	if len(fromSpec) == 0 {
		t.Fatal("no operation in the spec needs an API key; the test isn't reading the spec")
	}
	for pattern, scope := range fromSpec {
		if got := requiredScopes[pattern]; got != scope {
			t.Errorf("%s: requiredScopes has %q, spec has %q", pattern, got, scope)
		}
	}
	for pattern := range requiredScopes {
		if _, ok := fromSpec[pattern]; !ok {
			t.Errorf("requiredScopes has %s, which the spec doesn't list as needing an API key", pattern)
		}
	}
}

// TestRequiredScopesMatchRoutes checks that every route the generated code
// registers under /v1/ is in requiredScopes, so none is left without a key
// check.
func TestRequiredScopesMatchRoutes(t *testing.T) {
	mux := http.NewServeMux()
	api.HandlerWithOptions(api.NewStrictHandler(&Server{}, nil), api.StdHTTPServerOptions{BaseRouter: mux})
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			// Build a request that matches the route, with a placeholder
			// for any path parameter.
			target := strings.NewReplacer("{name}", "x").Replace(path)
			_, pattern := mux.Handler(httptest.NewRequest(method, target, nil))
			if pattern == "" {
				t.Errorf("%s %s isn't registered", method, path)
				continue
			}
			_, needsKey := requiredScopes[pattern]
			if strings.HasPrefix(path, "/v1/") != needsKey {
				t.Errorf("%s: under /v1/ = %v, in requiredScopes = %v", pattern, strings.HasPrefix(path, "/v1/"), needsKey)
			}
		}
	}
}

func TestLogPathHidesKeys(t *testing.T) {
	key := apikey.Generate()
	for _, path := range []string{"/v1/ledgers/" + key, "/v1/ledgers/" + key[3:]} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if got := logPath(r); strings.Contains(got, key[3:19]) {
			t.Errorf("logPath(%q) = %q, which contains part of the key", path, got)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/ledgers/orders", nil)
	if got := logPath(r); got != "/v1/ledgers/orders" {
		t.Errorf("logPath() = %q, want the path", got)
	}
}
