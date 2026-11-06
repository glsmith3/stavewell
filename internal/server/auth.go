package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/glsmith3/stavewell/internal/apikey"
	"github.com/glsmith3/stavewell/internal/store"
)

// requiredScopes maps each route that needs an API key to the scope it needs.
// The keys are ServeMux patterns, as the generated code registers them.
// TestRequiredScopesMatchSpec checks this table against the security
// requirements in api/openapi.yaml, so the two can't drift apart.
//
// The generated code doesn't enforce the spec's security requirements, and
// it reads parameters before any of its middleware runs. Checking the key
// here, before the generated code, means a request without a valid key gets
// 401 even if its parameters are also wrong.
var requiredScopes = map[string]string{
	"POST /v1/ledgers":       apikey.ScopeLedgerWrite,
	"GET /v1/ledgers":        apikey.ScopeLedgerRead,
	"GET /v1/ledgers/{name}": apikey.ScopeLedgerRead,

	"POST /v1/ledgers/{name}/records":           apikey.ScopeLedgerWrite,
	"GET /v1/ledgers/{name}/records":            apikey.ScopeLedgerRead,
	"GET /v1/ledgers/{name}/records/{sequence}": apikey.ScopeLedgerRead,
	"GET /v1/ledgers/{name}/chain-verification": apikey.ScopeLedgerRead,
}

// principalKey is the context key for the authenticated caller.
type principalKey struct{}

// principalFrom returns the caller that authenticate stored in ctx.
func principalFrom(ctx context.Context) (store.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(store.Principal)
	return p, ok
}

// authenticate checks the request's API key and that it has scope. If it
// does, authenticate returns the request with the caller in its context.
// Otherwise it sends a 401 or 403 error and returns false.
//
// Every kind of bad key gets the same 401 response. The log records which
// kind it was, and the key ID if the key exists, but never the key.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, scope string) (*http.Request, bool) {
	reject := func(reason, keyID string) {
		attrs := []any{"reason", reason, "method", r.Method, "path", logPath(r), "request_id", requestID(r.Context())}
		if keyID != "" {
			attrs = append(attrs, "key_id", keyID)
		}
		s.logger.Warn("authentication failed", attrs...)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, r, problemUnauthorized, detailUnauthorized)
	}

	values := r.Header.Values("Authorization")
	switch {
	case len(values) == 0:
		reject("missing", "")
		return nil, false
	case len(values) > 1:
		reject("more than one Authorization header", "")
		return nil, false
	}
	scheme, key, _ := strings.Cut(values[0], " ")
	if !strings.EqualFold(scheme, "Bearer") {
		reject("not a Bearer credential", "")
		return nil, false
	}

	p, err := s.db.Authenticate(r.Context(), strings.TrimSpace(key), s.now())
	var inactive *store.InactiveKeyError
	switch {
	case errors.Is(err, store.ErrMalformedKey):
		reject("malformed", "")
		return nil, false
	case errors.Is(err, store.ErrUnknownKey):
		reject("unknown", "")
		return nil, false
	case errors.As(err, &inactive):
		reason := "expired"
		if inactive.Revoked {
			reason = "revoked"
		}
		reject(reason, inactive.KeyID)
		return nil, false
	case err != nil:
		s.logger.Error("internal error", "method", r.Method, "path", logPath(r), "request_id", requestID(r.Context()), "error", err)
		writeProblem(w, r, problemInternal, detailInternal)
		return nil, false
	}

	if info := infoFrom(r.Context()); info != nil {
		info.keyID = p.KeyID
	}
	if !p.HasScope(scope) {
		s.logger.Warn("insufficient scope",
			"scope", scope, "key_id", p.KeyID, "method", r.Method, "path", logPath(r), "request_id", requestID(r.Context()))
		writeProblem(w, r, problemInsufficientScope, detailInsufficientScope(scope))
		return nil, false
	}
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p)), true
}
