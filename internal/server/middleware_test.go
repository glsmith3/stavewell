package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestErrorHandlersHideDetails(t *testing.T) {
	const detail = "open /var/lib/stavewell/data.db: permission denied"
	tests := []struct {
		name       string
		handler    func(*slog.Logger) func(http.ResponseWriter, *http.Request, error)
		wantStatus int
		wantBody   string
	}{
		{"request error", requestErrorHandler, http.StatusBadRequest, "invalid request"},
		{"response error", responseErrorHandler, http.StatusInternalServerError, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			w := httptest.NewRecorder()

			tt.handler(logger)(w, httptest.NewRequest(http.MethodGet, "/x", nil), errors.New(detail))

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			body := w.Body.String()
			if strings.Contains(body, "permission denied") || strings.Contains(body, "/var/lib") {
				t.Errorf("response body reveals internal details: %q", body)
			}
			if !strings.Contains(body, tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", body, tt.wantBody)
			}
			if !strings.Contains(logs.String(), "permission denied") {
				t.Errorf("details weren't logged: %s", logs.String())
			}
		})
	}
}
