package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"

	"github.com/glsmith3/stavewell/internal/apikey"
	"github.com/glsmith3/stavewell/internal/server"
	"github.com/glsmith3/stavewell/internal/store"
)

const specPath = "../../api/openapi.yaml"

// lockedBuffer is a bytes.Buffer that's safe to write from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// env is a server with its own database.
type env struct {
	db      *store.Store
	handler http.Handler
	srv     *httptest.Server
	logs    *lockedBuffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "stavewell.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logs := &lockedBuffer{}
	h := server.Handler(slog.New(slog.NewJSONHandler(logs, nil)), db)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{db: db, handler: h, srv: srv, logs: logs}
}

// newOrg creates an organization and returns its public ID.
func (e *env) newOrg(t *testing.T) string {
	t.Helper()
	org, err := e.db.CreateOrganization(context.Background(), "Test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return org.ID
}

// newKey creates an API key with the scopes in the organization and returns
// the key.
func (e *env) newKey(t *testing.T, orgID string, scopes ...string) string {
	t.Helper()
	_, secret, err := e.db.CreateAPIKey(context.Background(),
		store.NewAPIKey{OrganizationID: orgID, Name: "Test", Scopes: scopes, Days: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// fullKey creates an organization with a key that has both scopes.
func (e *env) fullKey(t *testing.T) string {
	t.Helper()
	return e.newKey(t, e.newOrg(t), apikey.ScopeLedgerRead, apikey.ScopeLedgerWrite)
}

// do sends a request with the key (if any) and returns the response.
func (e *env) do(t *testing.T, method, path, key, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decode reads a JSON response body into v.
func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decoding %q: %v", b, err)
	}
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

// specCase is one request whose response TestResponsesMatchSpec checks.
type specCase struct {
	name       string
	setup      func(t *testing.T, e *env) (key string)
	method     string
	path       string
	body       string
	wantStatus int
	wantType   string // for errors, the error type's name
}

// createLedger creates a ledger through the store, for a key's organization.
func createLedger(t *testing.T, e *env, key, name string) {
	t.Helper()
	p, err := e.db.Authenticate(context.Background(), key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.CreateLedger(context.Background(), p.OrgRowID, name, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func specCases() []specCase {
	full := func(t *testing.T, e *env) string { return e.fullKey(t) }
	none := func(t *testing.T, e *env) string { return "" }
	readOnly := func(t *testing.T, e *env) string { return e.newKey(t, e.newOrg(t), apikey.ScopeLedgerRead) }
	writeOnly := func(t *testing.T, e *env) string { return e.newKey(t, e.newOrg(t), apikey.ScopeLedgerWrite) }
	withOrders := func(t *testing.T, e *env) string {
		key := e.fullKey(t)
		createLedger(t, e, key, "orders")
		return key
	}
	ordersWriteOnly := func(t *testing.T, e *env) string {
		org := e.newOrg(t)
		createLedger(t, e, e.newKey(t, org, apikey.ScopeLedgerRead, apikey.ScopeLedgerWrite), "orders")
		return e.newKey(t, org, apikey.ScopeLedgerWrite)
	}
	atLimit := func(t *testing.T, e *env) string {
		key := e.fullKey(t)
		for i := range store.MaxLedgers {
			createLedger(t, e, key, fmt.Sprintf("l%d", i))
		}
		return key
	}
	big := `{"name":"orders","padding":"` + strings.Repeat("x", 64<<10) + `"}`
	return []specCase{
		{"health", none, "GET", "/health", "", 200, ""},
		{"version", none, "GET", "/version", "", 200, ""},
		{"create", full, "POST", "/v1/ledgers", `{"name":"orders"}`, 201, ""},
		{"create with invalid JSON", full, "POST", "/v1/ledgers", `{"name":`, 400, "invalid-request"},
		{"create with an invalid name", full, "POST", "/v1/ledgers", `{"name":"Orders"}`, 400, "invalid-request"},
		{"create without a key", none, "POST", "/v1/ledgers", `{"name":"orders"}`, 401, "unauthorized"},
		{"create with a read-only key", readOnly, "POST", "/v1/ledgers", `{"name":"orders"}`, 403, "insufficient-scope"},
		{"create a duplicate", withOrders, "POST", "/v1/ledgers", `{"name":"orders"}`, 409, "ledger-already-exists"},
		{"create past the limit", atLimit, "POST", "/v1/ledgers", `{"name":"one-more"}`, 409, "organization-limit-reached"},
		{"create with a large body", full, "POST", "/v1/ledgers", big, 413, "request-too-large"},
		{"list", withOrders, "GET", "/v1/ledgers", "", 200, ""},
		{"list with limit 0", full, "GET", "/v1/ledgers?limit=0", "", 400, "invalid-request"},
		{"list with a bad cursor", full, "GET", "/v1/ledgers?cursor=nope", "", 400, "invalid-cursor"},
		{"list without a key", none, "GET", "/v1/ledgers", "", 401, "unauthorized"},
		{"list with a write-only key", writeOnly, "GET", "/v1/ledgers", "", 403, "insufficient-scope"},
		{"get", withOrders, "GET", "/v1/ledgers/orders", "", 200, ""},
		{"get a missing ledger", full, "GET", "/v1/ledgers/missing", "", 404, "ledger-not-found"},
		{"get without a key", none, "GET", "/v1/ledgers/orders", "", 401, "unauthorized"},
		{"get with a write-only key", ordersWriteOnly, "GET", "/v1/ledgers/orders", "", 403, "insufficient-scope"},
	}
}

// TestResponsesMatchSpec sends one request for every response the spec
// declares, and checks each real response against the spec: its status, its
// headers and its body. It fails if a declared response has no case here, so
// a new endpoint or error can't go unchecked. The only exception is 500,
// which can't be caused on purpose.
func TestResponsesMatchSpec(t *testing.T) {
	ctx := context.Background()
	doc := loadSpec(t)
	router, err := legacyrouter.NewRouter(doc)
	if err != nil {
		t.Fatalf("building router: %v", err)
	}
	covered := make(map[string]bool) // "METHOD /path STATUS"

	for _, c := range specCases() {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			resp := e.do(t, c.method, c.path, c.setup(t, e), c.body)
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, c.wantStatus, body)
			}

			// Match the request against the spec's server URL so the router
			// can find the operation.
			req, err := http.NewRequest(c.method, doc.Servers[0].URL+c.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			route, pathParams, err := router.FindRoute(req)
			if err != nil {
				t.Fatalf("finding route in spec: %v", err)
			}
			covered[c.method+" "+route.Path+" "+strconv.Itoa(resp.StatusCode)] = true
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
				t.Errorf("response doesn't match spec: %v\nbody: %s", err, body)
			}
			if c.wantType != "" {
				var p struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(body, &p); err != nil || p.Type != "https://docs.stavewell.io/errors/"+c.wantType {
					t.Errorf("error type = %q, want %s", p.Type, c.wantType)
				}
			}
		})
	}

	for _, path := range slices.Sorted(maps.Keys(doc.Paths.Map())) {
		for method, op := range doc.Paths.Map()[path].Operations() {
			for status := range op.Responses.Map() {
				if status == "500" {
					continue
				}
				if !covered[method+" "+path+" "+status] {
					t.Errorf("no case checks %s %s %s; add one to specCases", method, path, status)
				}
			}
		}
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	var got struct {
		Status string `json:"status"`
	}
	decode(t, e.do(t, "GET", "/health", "", ""), &got)
	if got.Status != "ok" {
		t.Errorf("status = %q, want %q", got.Status, "ok")
	}
}

func TestVersion(t *testing.T) {
	e := newEnv(t)
	var got struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	decode(t, e.do(t, "GET", "/version", "", ""), &got)
	// Test binaries aren't built with a release version or commit info.
	if got.Version != "dev" {
		t.Errorf("version = %q, want %q", got.Version, "dev")
	}
	if got.Commit == "" {
		t.Error("commit is empty, want a commit or \"unknown\"")
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/health", "", "")
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

// problem is the body of an error response.
type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id"`
}

// checkProblem checks that resp is a Problem Details error of the named type
// and status, and that its body matches the spec's Problem schema. It returns
// the decoded body.
func checkProblem(t *testing.T, resp *http.Response, wantName string, wantStatus int) problem {
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
	var p problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if want := "https://docs.stavewell.io/errors/" + wantName; p.Type != want {
		t.Errorf("type = %q, want %q", p.Type, want)
	}
	if p.Status != wantStatus {
		t.Errorf("status field = %d, want %d", p.Status, wantStatus)
	}
	if !requestIDFormat.MatchString(p.RequestID) {
		t.Errorf("request_id = %q, want a match for %s", p.RequestID, requestIDFormat)
	}
	return p
}

func TestUnknownPathReturnsProblem(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/no-such-path", "", "")
	checkProblem(t, resp, "not-found", http.StatusNotFound)
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestWrongMethodReturnsProblem(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "POST", "/health", "", "{}")
	checkProblem(t, resp, "method-not-allowed", http.StatusMethodNotAllowed)
	allow := resp.Header.Get("Allow")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if !strings.Contains(allow, method) {
			t.Errorf("Allow = %q, want it to include %s", allow, method)
		}
	}
}

func TestHeadOnUnknownPathReturnsProblemHeaders(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "HEAD", "/no-such-path", "", "")
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
	e := newEnv(t)
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "*", nil))
	resp := w.Result()
	defer resp.Body.Close()
	checkProblem(t, resp, "invalid-request", http.StatusBadRequest)
	if got := resp.Header.Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}
}

// logLines decodes the JSON log, one object per line.
func logLines(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(logs) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

// serve runs one request through the handler directly, so that every log
// line is written by the time it returns.
func (e *env) serve(t *testing.T, method, path, key, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return w.Result()
}

// TestErrorRequestIDMatchesLog checks that the request ID in an error
// response is the one the server logged for that request.
func TestErrorRequestIDMatchesLog(t *testing.T) {
	e := newEnv(t)
	var body problem
	decode(t, e.serve(t, "GET", "/no-such-path", "", ""), &body)
	lines := logLines(t, e.logs.String())
	last := lines[len(lines)-1]
	if body.RequestID == "" || last["request_id"] != body.RequestID {
		t.Errorf("logged request_id = %v, response request_id = %q; want the same, not empty", last["request_id"], body.RequestID)
	}
	if last["status"] != float64(http.StatusNotFound) {
		t.Errorf("logged status = %v, want 404", last["status"])
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	e := newEnv(t)
	seen := make(map[string]bool)
	for range 100 {
		var body problem
		decode(t, e.serve(t, "GET", "/no-such-path", "", ""), &body)
		if seen[body.RequestID] {
			t.Fatalf("request ID %q was used twice", body.RequestID)
		}
		seen[body.RequestID] = true
	}
}

// ledgerList is the body of a list response.
type ledgerList struct {
	Ledgers []struct {
		Name      string `json:"name"`
		CreatedAt string `json:"created_at"`
	} `json:"ledgers"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

func (l ledgerList) names() []string {
	var names []string
	for _, ledger := range l.Ledgers {
		names = append(names, ledger.Name)
	}
	return names
}

func TestCreateAndGetLedger(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	resp := e.do(t, "POST", "/v1/ledgers", key, `{"name":"orders"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}
	var created struct {
		Name      string `json:"name"`
		CreatedAt string `json:"created_at"`
	}
	decode(t, resp, &created)
	if created.Name != "orders" || !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(created.CreatedAt) {
		t.Errorf("created = %+v; want name orders and a time with three fractional digits", created)
	}
	var got struct {
		Name      string `json:"name"`
		CreatedAt string `json:"created_at"`
	}
	decode(t, e.do(t, "GET", "/v1/ledgers/orders", key, ""), &got)
	if got != created {
		t.Errorf("get = %+v, want %+v", got, created)
	}
	// Names no ledger can have get the same 404 as missing ones.
	for _, path := range []string{"/v1/ledgers/Orders", "/v1/ledgers/1x", "/v1/ledgers/" + strings.Repeat("a", 65)} {
		checkProblem(t, e.do(t, "GET", path, key, ""), "ledger-not-found", http.StatusNotFound)
	}
}

// TestLedgersAreIsolated checks that one organization can never see or
// affect another organization's ledgers.
func TestLedgersAreIsolated(t *testing.T) {
	e := newEnv(t)
	keyA, keyB := e.fullKey(t), e.fullKey(t)
	if resp := e.do(t, "POST", "/v1/ledgers", keyA, `{"name":"orders"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("A create: status %d", resp.StatusCode)
	}

	// B gets the same 404 for A's ledger as for one that doesn't exist.
	pA := checkProblem(t, e.do(t, "GET", "/v1/ledgers/orders", keyB, ""), "ledger-not-found", http.StatusNotFound)
	pMissing := checkProblem(t, e.do(t, "GET", "/v1/ledgers/never-created", keyB, ""), "ledger-not-found", http.StatusNotFound)
	if pA.Title != pMissing.Title || pA.Detail != pMissing.Detail {
		t.Errorf("another organization's ledger: %+v; a missing ledger: %+v; want the same", pA, pMissing)
	}
	var listB ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers", keyB, ""), &listB)
	if len(listB.Ledgers) != 0 {
		t.Errorf("B's list = %v, want empty", listB.names())
	}

	// B can use the same name, and each sees only its own ledger.
	if resp := e.do(t, "POST", "/v1/ledgers", keyB, `{"name":"orders"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("B create: status %d", resp.StatusCode)
	}
	var listA ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers", keyA, ""), &listA)
	decode(t, e.do(t, "GET", "/v1/ledgers", keyB, ""), &listB)
	if len(listA.Ledgers) != 1 || len(listB.Ledgers) != 1 || listA.Ledgers[0].CreatedAt == "" {
		t.Errorf("lists = %v and %v, want one ledger each", listA.names(), listB.names())
	}

	// A's cursor doesn't work for B.
	checkProblem(t, e.do(t, "GET", "/v1/ledgers?cursor="+url.QueryEscape(listA.NextCursor), keyB, ""), "invalid-cursor", http.StatusBadRequest)
}

func TestListLedgersPages(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)

	// A new organization's list is empty, not null, and its cursor works
	// later.
	resp := e.do(t, "GET", "/v1/ledgers", key, "")
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"ledgers":[]`) {
		t.Errorf("empty list body = %s, want \"ledgers\":[]", raw)
	}
	var empty ledgerList
	if err := json.Unmarshal(raw, &empty); err != nil || empty.HasMore || empty.NextCursor == "" {
		t.Fatalf("empty list = %+v, %v", empty, err)
	}

	// Created out of alphabetical order: the list is in creation order.
	names := []string{"zeta", "alpha", "mu", "beta", "omega"}
	for _, name := range names {
		if resp := e.do(t, "POST", "/v1/ledgers", key, `{"name":"`+name+`"}`); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: status %d", name, resp.StatusCode)
		}
	}
	var fromEmpty ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers?cursor="+url.QueryEscape(empty.NextCursor), key, ""), &fromEmpty)
	if strings.Join(fromEmpty.names(), ",") != strings.Join(names, ",") {
		t.Errorf("from the empty list's cursor = %v, want %v", fromEmpty.names(), names)
	}

	var got []string
	path := "/v1/ledgers?limit=2"
	var page ledgerList
	for i := 0; ; i++ {
		page = ledgerList{}
		decode(t, e.do(t, "GET", path, key, ""), &page)
		got = append(got, page.names()...)
		if wantMore := len(got) < len(names); page.HasMore != wantMore {
			t.Errorf("page %d: has_more = %v, want %v", i, page.HasMore, wantMore)
		}
		if !page.HasMore || i > 10 {
			break
		}
		path = "/v1/ledgers?limit=2&cursor=" + url.QueryEscape(page.NextCursor)
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("paged through %v, want %v", got, names)
	}

	// The last page's cursor returns only ledgers added since.
	var after ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers?cursor="+url.QueryEscape(page.NextCursor), key, ""), &after)
	if len(after.Ledgers) != 0 || after.HasMore || after.NextCursor != page.NextCursor {
		t.Errorf("after the last page = %+v, want nothing and the same cursor", after)
	}
	e.do(t, "POST", "/v1/ledgers", key, `{"name":"newest"}`)
	decode(t, e.do(t, "GET", "/v1/ledgers?cursor="+url.QueryEscape(page.NextCursor), key, ""), &after)
	if strings.Join(after.names(), ",") != "newest" {
		t.Errorf("after the last page, once a ledger is added = %v, want [newest]", after.names())
	}
}

func TestListLedgersLimit(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	for _, limit := range []string{"1", "100"} {
		if resp := e.do(t, "GET", "/v1/ledgers?limit="+limit, key, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("limit=%s: status %d, want 200", limit, resp.StatusCode)
		}
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=abc", "limit=1.5", "limit=1&limit=2"} {
		p := checkProblem(t, e.do(t, "GET", "/v1/ledgers?"+query, key, ""), "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "from 1 to 100") {
			t.Errorf("%s: detail = %q, want it to give the range", query, p.Detail)
		}
	}
	for _, query := range []string{"cursor=", "cursor=nope", "cursor=a&cursor=b"} {
		checkProblem(t, e.do(t, "GET", "/v1/ledgers?"+query, key, ""), "invalid-cursor", http.StatusBadRequest)
	}
}

func TestCreateLedgerValidation(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	for _, body := range []string{`{"name":""}`, `{}`, `{"name":"My Ledger"}`, `{"name":"a_b"}`, `{"name":"` + strings.Repeat("a", 65) + `"}`} {
		p := checkProblem(t, e.do(t, "POST", "/v1/ledgers", key, body), "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "ledger name") {
			t.Errorf("%s: detail = %q, want it to explain the name rule", body, p.Detail)
		}
	}
	for _, body := range []string{``, `not json`, `{"name":5}`, `["orders"]`, `{"name":null}`} {
		checkProblem(t, e.do(t, "POST", "/v1/ledgers", key, body), "invalid-request", http.StatusBadRequest)
	}
	var list ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers", key, ""), &list)
	if len(list.Ledgers) != 0 {
		t.Errorf("invalid requests created %v", list.names())
	}
}

func TestRequestBodyLimit(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	// A body of exactly 64 KiB is accepted; one byte more isn't. Whitespace
	// pads the body, so it stays a valid JSON object.
	pad := func(name string, size int, after bool) string {
		obj := `{"name":"` + name + `"}`
		if after {
			return obj + strings.Repeat(" ", size-len(obj))
		}
		return `{"name":"` + name + `"` + strings.Repeat(" ", size-len(obj)) + `}`
	}
	for _, after := range []bool{false, true} {
		name := fmt.Sprintf("exact-%v", after)
		if body := pad(name, 64<<10, after); len(body) != 64<<10 {
			t.Fatalf("test body is %d bytes", len(body))
		}
		if resp := e.do(t, "POST", "/v1/ledgers", key, pad(name, 64<<10, after)); resp.StatusCode != http.StatusCreated {
			t.Errorf("64 KiB body, padding after the JSON %v: status %d, want 201", after, resp.StatusCode)
		}
		// Bytes after the JSON object count toward the limit too.
		checkProblem(t, e.do(t, "POST", "/v1/ledgers", key, pad("over", 64<<10+1, after)), "request-too-large", http.StatusRequestEntityTooLarge)
		checkProblem(t, e.do(t, "POST", "/v1/ledgers", key, pad("far-over", 70_000, after)), "request-too-large", http.StatusRequestEntityTooLarge)
	}
	var list ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers", key, ""), &list)
	if strings.Join(list.names(), ",") != "exact-false,exact-true" {
		t.Errorf("ledgers = %v, want only the two 64 KiB requests", list.names())
	}
}

// TestRequestBodyIsStrict checks that a body is accepted only if it's one
// JSON object with only the documented fields.
func TestRequestBodyIsStrict(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	for _, body := range []string{
		`{"name":"trail"} garbage`,
		`{"name":"one"}{"name":"two"}`,
		`{"NAME":"upper"}`,
		`{"Name":"title"}`,
		`{"name":"first","name":"second"}`,
		`{"name":"extra","ignored":true}`,
		"{\"name\":\"bad-utf8-\xff\"}",
		`["orders"]`,
		`"orders"`,
		`{"name":["orders"]}`,
	} {
		p := checkProblem(t, e.do(t, "POST", "/v1/ledgers", key, body), "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "only the documented fields") {
			t.Errorf("%q: detail = %q, want it to explain the body rule", body, p.Detail)
		}
	}
	var list ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers", key, ""), &list)
	if len(list.Ledgers) != 0 {
		t.Errorf("rejected bodies created %v", list.names())
	}

	// Whitespace around the object is fine, and Content-Type isn't checked.
	if resp := e.do(t, "POST", "/v1/ledgers", key, "  \n {\"name\":\"spaced\"} \n "); resp.StatusCode != http.StatusCreated {
		t.Errorf("whitespace around the body: status %d, want 201", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/ledgers", strings.NewReader(`{"name":"plain"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("Content-Type text/plain: status %d, want 201", resp.StatusCode)
	}
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	org := e.newOrg(t)
	good := e.newKey(t, org, apikey.ScopeLedgerRead)

	_, expired, err := e.db.CreateAPIKey(context.Background(),
		store.NewAPIKey{OrganizationID: org, Name: "old", Scopes: []string{apikey.ScopeLedgerRead}, Days: 1}, time.Now().AddDate(0, 0, -2))
	if err != nil {
		t.Fatal(err)
	}
	revokedKey, revoked, err := e.db.CreateAPIKey(context.Background(),
		store.NewAPIKey{OrganizationID: org, Name: "gone", Scopes: []string{apikey.ScopeLedgerRead}, Days: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.RevokeAPIKey(context.Background(), revokedKey.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Every kind of bad credential gets the same response.
	var first problem
	for i, header := range [][]string{
		nil,
		{"Basic dXNlcjpwYXNz"},
		{"Bearer"},
		{"Bearer "},
		{"Bearer sw_short"},
		{"Bearer " + apikey.Generate()},
		{"Bearer " + apikey.ExampleKey},
		{"Bearer " + expired},
		{"Bearer " + revoked},
		{"Bearer " + good, "Bearer " + good},
		{good},
	} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/ledgers", nil)
		for _, h := range header {
			req.Header.Add("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		p := checkProblem(t, resp, "unauthorized", http.StatusUnauthorized)
		resp.Body.Close()
		if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("case %d: WWW-Authenticate = %q, want Bearer", i, got)
		}
		if i == 0 {
			first = p
		} else if p.Title != first.Title || p.Detail != first.Detail {
			t.Errorf("case %d: %+v differs from %+v", i, p, first)
		}
	}

	// The scheme isn't case-sensitive, and extra spaces are allowed.
	for _, header := range []string{"bearer " + good, "BEARER " + good, "Bearer   " + good} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/ledgers", nil)
		req.Header.Set("Authorization", header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%q: status %d, want 200", header[:7], resp.StatusCode)
		}
	}

	// Endpoints outside /v1/ don't need a key.
	if resp := e.do(t, "GET", "/health", "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/health without a key: status %d", resp.StatusCode)
	}
}

// TestAuthenticationComesFirst checks that a request without a valid key
// gets 401, even if it's wrong in other ways too.
func TestAuthenticationComesFirst(t *testing.T) {
	e := newEnv(t)
	big := `{"name":"` + strings.Repeat("x", 70<<10) + `"}`
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/ledgers?limit=abc", ""},
		{"GET", "/v1/ledgers?cursor=nope", ""},
		{"POST", "/v1/ledgers", "not json"},
		{"POST", "/v1/ledgers", big},
		{"GET", "/v1/ledgers/Not-A-Name", ""},
	} {
		checkProblem(t, e.do(t, c.method, c.path, "", c.body), "unauthorized", http.StatusUnauthorized)
	}
}

func TestInsufficientScopeNamesTheScope(t *testing.T) {
	e := newEnv(t)
	org := e.newOrg(t)
	p := checkProblem(t, e.do(t, "POST", "/v1/ledgers", e.newKey(t, org, apikey.ScopeLedgerRead), `{"name":"x"}`), "insufficient-scope", http.StatusForbidden)
	if !strings.Contains(p.Detail, "ledger:write") {
		t.Errorf("detail = %q, want it to name ledger:write", p.Detail)
	}
	p = checkProblem(t, e.do(t, "GET", "/v1/ledgers", e.newKey(t, org, apikey.ScopeLedgerWrite), ""), "insufficient-scope", http.StatusForbidden)
	if !strings.Contains(p.Detail, "ledger:read") {
		t.Errorf("detail = %q, want it to name ledger:read", p.Detail)
	}
}

// TestLogsRecordKeyIDsNotKeys checks what the log says about API keys.
func TestLogsRecordKeyIDsNotKeys(t *testing.T) {
	e := newEnv(t)
	org := e.newOrg(t)
	created, key, err := e.db.CreateAPIKey(context.Background(),
		store.NewAPIKey{OrganizationID: org, Name: "k", Scopes: []string{apikey.ScopeLedgerRead}, Days: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	e.serve(t, "GET", "/v1/ledgers", key, "")
	e.serve(t, "GET", "/v1/ledgers", apikey.Generate(), "")
	e.serve(t, "POST", "/v1/ledgers", key, `{"name":"x"}`)
	e.serve(t, "GET", "/v1/ledgers/"+key, key, "")

	logs := e.logs.String()
	if strings.Contains(logs, key[3:19]) {
		t.Error("the log contains part of an API key")
	}
	var requests, failures, scopes int
	for _, line := range logLines(t, logs) {
		switch line["msg"] {
		case "request":
			requests++
			if line["status"] != float64(http.StatusUnauthorized) && line["key_id"] != created.ID {
				t.Errorf("request line %v: want key_id %s", line, created.ID)
			}
		case "authentication failed":
			failures++
			if line["reason"] != "unknown" {
				t.Errorf("authentication failure %v: want reason unknown", line)
			}
		case "insufficient scope":
			scopes++
			if line["key_id"] != created.ID || line["scope"] != "ledger:write" {
				t.Errorf("insufficient scope line %v", line)
			}
		}
	}
	if requests != 4 || failures != 1 || scopes != 1 {
		t.Errorf("log has %d request, %d authentication failure and %d scope lines; want 4, 1 and 1:\n%s", requests, failures, scopes, logs)
	}
}
