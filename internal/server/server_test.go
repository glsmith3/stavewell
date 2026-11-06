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

func TestUnknownPathReturns404(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/no-such-path")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestWrongMethodReturns405(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Post(srv.URL+"/health", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want it to include GET", allow)
	}
}
