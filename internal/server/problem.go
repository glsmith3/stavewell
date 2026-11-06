package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/glsmith3/stavewell/internal/api"
)

// typeURLBase is the start of every error type URL. The URLs are permanent:
// clients use them to tell error types apart.
const typeURLBase = "https://docs.stavewell.io/errors/"

// problemType is one kind of error response. Its name is permanent, because
// it forms the error type URL.
type problemType struct {
	name   string
	title  string
	status int
}

// The error types the server can return. Each one is listed in
// api/openapi.yaml.
var (
	problemInvalidRequest    = problemType{"invalid-request", "Invalid request", http.StatusBadRequest}
	problemInvalidCursor     = problemType{"invalid-cursor", "Invalid cursor", http.StatusBadRequest}
	problemUnauthorized      = problemType{"unauthorized", "Unauthorized", http.StatusUnauthorized}
	problemInsufficientScope = problemType{"insufficient-scope", "Insufficient scope", http.StatusForbidden}
	problemNotFound          = problemType{"not-found", "Not found", http.StatusNotFound}
	problemLedgerNotFound    = problemType{"ledger-not-found", "Ledger not found", http.StatusNotFound}
	problemMethodNotAllowed  = problemType{"method-not-allowed", "Method not allowed", http.StatusMethodNotAllowed}
	problemLedgerExists      = problemType{"ledger-already-exists", "Ledger already exists", http.StatusConflict}
	problemOrgLimit          = problemType{"organization-limit-reached", "Organization limit reached", http.StatusConflict}
	problemRequestTooLarge   = problemType{"request-too-large", "Request too large", http.StatusRequestEntityTooLarge}
	problemInternal          = problemType{"internal-error", "Internal error", http.StatusInternalServerError}
)

// Detail text for the error types above. Detail text is always written for
// callers in advance, never built from an internal error message.
const (
	detailInvalidRequest    = "The request isn't valid."
	detailInvalidJSON       = "The request body isn't valid JSON, or a field has the wrong type."
	detailInvalidBody       = "The request body must be one JSON object with only the documented fields, each of the documented type."
	detailInvalidParameter  = "A parameter isn't valid."
	detailInvalidLimit      = "The limit parameter must be a whole number from 1 to 100."
	detailInvalidCursor     = "This cursor didn't come from this list. Use the next_cursor from an earlier page of the same list."
	detailInvalidLedgerName = "The ledger name must be 1 to 64 lowercase letters, digits and hyphens, starting with a letter."
	detailUnauthorized      = "This request needs a valid API key in the Authorization header."
	detailNotFound          = "No endpoint matches this path."
	detailLedgerNotFound    = "Your organization has no ledger with this name."
	detailMethodNotAllowed  = "This endpoint doesn't accept the request's method. The Allow header lists the methods it accepts."
	detailLedgerExists      = "Your organization already has a ledger with this name."
	detailOrgLimit          = "Your organization already has 100 ledgers, the most it can have."
	detailRequestTooLarge   = "The request body is larger than 64 KiB."
	detailInternal          = "The server couldn't complete the request."
)

// detailInsufficientScope returns the detail text for a key that lacks
// scope. scope always comes from requiredScopes, never from the request.
func detailInsufficientScope(scope string) string {
	return "This API key doesn't have the " + scope + " scope."
}

// newProblem returns the body of an error response of type p.
func newProblem(ctx context.Context, p problemType, detail string) api.Problem {
	return api.Problem{
		Type:      typeURLBase + p.name,
		Title:     p.title,
		Status:    p.status,
		Detail:    detail,
		RequestId: requestID(ctx),
	}
}

// writeProblem sends an error response in the Problem Details format
// (RFC 9457). detail must be text written for callers, never an internal
// error message.
func writeProblem(w http.ResponseWriter, r *http.Request, p problemType, detail string) {
	var body bytes.Buffer
	// Encoding can't fail: every field is a string or an int.
	_ = json.NewEncoder(&body).Encode(newProblem(r.Context(), p, detail))
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Del("Content-Length")
	w.WriteHeader(p.status)
	// If the client has gone away, there's no one to report a write error to.
	_, _ = w.Write(body.Bytes())
}
