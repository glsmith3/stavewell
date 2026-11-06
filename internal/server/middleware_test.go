package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glsmith3/stavewell/internal/api"
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
		{"request error", requestErrorHandler, http.StatusBadRequest, typeURLBase + "invalid-request"},
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

func TestRecoverPanicsReturnsProblem(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := withRequestID(logRequests(logger, recoverPanics(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("lost connection to /var/lib/stavewell/data.db")
	}))))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

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
	if !strings.Contains(logs.String(), "lost connection") {
		t.Errorf("panic wasn't logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"status":500`) {
		t.Errorf("request log doesn't record status 500: %s", logs.String())
	}
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

func TestRouteProblemsPassesOnRedirects(t *testing.T) {
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
	routeProblems(mux).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/a/../b", nil))

	if w.Code != plain.Code {
		t.Errorf("status = %d, want %d", w.Code, plain.Code)
	}
	if got, want := w.Header().Get("Location"), plain.Header().Get("Location"); got != want || got != "/b" {
		t.Errorf("Location = %q, want %q", got, want)
	}
}
