package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"

	"github.com/glsmith3/stavewell/internal/server"
)

const specPath = "../../api/openapi.yaml"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(server.Handler(logger))
	t.Cleanup(srv.Close)
	return srv
}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specPath)
	if err != nil {
		t.Fatalf("loading spec: %v", err)
	}
	return doc
}

// TestSpecIsValid checks that api/openapi.yaml is a valid OpenAPI document.
func TestSpecIsValid(t *testing.T) {
	doc := loadSpec(t)
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("spec is invalid: %v", err)
	}
}

// TestResponsesMatchSpec calls every operation in api/openapi.yaml and checks
// the real response against the spec. Operations it can't call automatically
// (anything other than a GET without parameters) fail the test, so a new
// operation can't go unchecked: give it its own test case instead.
func TestResponsesMatchSpec(t *testing.T) {
	ctx := context.Background()
	doc := loadSpec(t)
	router, err := legacyrouter.NewRouter(doc)
	if err != nil {
		t.Fatalf("building router: %v", err)
	}
	srv := newTestServer(t)

	paths := doc.Paths.Map()
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		item := paths[path]
		for method, op := range item.Operations() {
			if method != http.MethodGet || len(op.Parameters) > 0 || len(item.Parameters) > 0 {
				t.Errorf("%s %s can't be checked automatically; add a test case for it", method, path)
			}
		}
		if item.Get == nil {
			continue
		}
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
			}

			// Match the request against the spec's server URL so the router
			// can find the operation.
			req, err := http.NewRequest(http.MethodGet, doc.Servers[0].URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			route, pathParams, err := router.FindRoute(req)
			if err != nil {
				t.Fatalf("finding route in spec: %v", err)
			}
			input := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{
					Request:    req,
					PathParams: pathParams,
					Route:      route,
				},
				Status: resp.StatusCode,
				Header: resp.Header,
			}
			input.SetBodyBytes(body)
			if err := openapi3filter.ValidateResponse(ctx, input); err != nil {
				t.Errorf("response doesn't match spec: %v", err)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want %q", got.Status, "ok")
	}
}

func TestVersion(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	// Test binaries aren't built with a release version or commit info.
	if got.Version != "dev" {
		t.Errorf("version = %q, want %q", got.Version, "dev")
	}
	if got.Commit == "" {
		t.Error("commit is empty, want a commit or \"unknown\"")
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
		"Content-Type":           "application/json",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

// requestIDFormat is the format of request IDs in error responses.
var requestIDFormat = regexp.MustCompile(`^req_[a-z2-7]{26}$`)

// checkProblem checks that resp is a Problem Details error of the named type
// and status, and that its body matches the spec's Problem schema. It returns
// the decoded body.
func checkProblem(t *testing.T, resp *http.Response, wantName string, wantStatus int) map[string]any {
	t.Helper()
	if resp.StatusCode != wantStatus {
		t.Errorf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decoding body %q: %v", body, err)
	}
	schema := loadSpec(t).Components.Schemas["Problem"].Value
	if err := schema.VisitJSON(v); err != nil {
		t.Errorf("body doesn't match the Problem schema: %v\nbody: %s", err, body)
	}
	m, _ := v.(map[string]any)
	if got, want := m["type"], "https://docs.stavewell.io/errors/"+wantName; got != want {
		t.Errorf("type = %v, want %q", got, want)
	}
	if got := m["status"]; got != float64(wantStatus) {
		t.Errorf("status field = %v, want %d", got, wantStatus)
	}
	if id, _ := m["request_id"].(string); !requestIDFormat.MatchString(id) {
		t.Errorf("request_id = %q, want a match for %s", id, requestIDFormat)
	}
	return m
}

func TestUnknownPathReturnsProblem(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/no-such-path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	checkProblem(t, resp, "not-found", http.StatusNotFound)
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestWrongMethodReturnsProblem(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Post(srv.URL+"/health", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	checkProblem(t, resp, "method-not-allowed", http.StatusMethodNotAllowed)
	allow := resp.Header.Get("Allow")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if !strings.Contains(allow, method) {
			t.Errorf("Allow = %q, want it to include %s", allow, method)
		}
	}
}

func TestHeadOnUnknownPathReturnsProblemHeaders(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Head(srv.URL + "/no-such-path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if body, _ := io.ReadAll(resp.Body); len(body) != 0 {
		t.Errorf("HEAD response has a body: %q", body)
	}
}

// TestAsteriskRequestTargetReturnsProblem checks the request target "*",
// which is valid only for OPTIONS.
func TestAsteriskRequestTargetReturnsProblem(t *testing.T) {
	h := server.Handler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "*", nil))

	resp := w.Result()
	defer resp.Body.Close()
	checkProblem(t, resp, "invalid-request", http.StatusBadRequest)
	if got := resp.Header.Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}
}

// TestErrorRequestIDMatchesLog checks that the request ID in an error
// response is the one the server logged for that request.
func TestErrorRequestIDMatchesLog(t *testing.T) {
	var logs bytes.Buffer
	h := server.Handler(slog.New(slog.NewJSONHandler(&logs, nil)))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/no-such-path", nil))

	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body %q: %v", w.Body.String(), err)
	}
	var entry struct {
		Status    int    `json:"status"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("decoding log line %q: %v", logs.String(), err)
	}
	if body.RequestID == "" || entry.RequestID != body.RequestID {
		t.Errorf("logged request_id = %q, response request_id = %q; want the same, not empty", entry.RequestID, body.RequestID)
	}
	if entry.Status != http.StatusNotFound {
		t.Errorf("logged status = %d, want 404", entry.Status)
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	h := server.Handler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	seen := make(map[string]bool)
	for range 100 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/no-such-path", nil))
		var body struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding body %q: %v", w.Body.String(), err)
		}
		if seen[body.RequestID] {
			t.Fatalf("request ID %q was used twice", body.RequestID)
		}
		seen[body.RequestID] = true
	}
}
