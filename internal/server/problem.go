package server

import (
	"bytes"
	"encoding/json"
	"net/http"
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
	problemInvalidRequest   = problemType{"invalid-request", "Invalid request", http.StatusBadRequest}
	problemNotFound         = problemType{"not-found", "Not found", http.StatusNotFound}
	problemMethodNotAllowed = problemType{"method-not-allowed", "Method not allowed", http.StatusMethodNotAllowed}
	problemInternal         = problemType{"internal-error", "Internal error", http.StatusInternalServerError}
)

// Detail text for the error types above. Detail text is always written for
// callers in advance, never built from an internal error message.
const (
	detailInvalidRequest   = "The request isn't valid."
	detailNotFound         = "No endpoint matches this path."
	detailMethodNotAllowed = "This endpoint doesn't accept the request's method. The Allow header lists the methods it accepts."
	detailInternal         = "The server couldn't complete the request."
)

// problemBody is the body of an error response. It matches the Problem
// schema in api/openapi.yaml; tests check every error response against that
// schema. It's declared here, rather than using the generated api.Problem,
// so that the fields appear in the order RFC 9457 lists them, which is easier
// to read.
type problemBody struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id"`
}

// writeProblem sends an error response in the Problem Details format
// (RFC 9457). detail must be text written for callers, never an internal
// error message.
func writeProblem(w http.ResponseWriter, r *http.Request, p problemType, detail string) {
	var body bytes.Buffer
	// Encoding can't fail: every field is a string or an int.
	_ = json.NewEncoder(&body).Encode(problemBody{
		Type:      typeURLBase + p.name,
		Title:     p.title,
		Status:    p.status,
		Detail:    detail,
		RequestID: requestID(r.Context()),
	})
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Del("Content-Length")
	w.WriteHeader(p.status)
	// If the client has gone away, there's no one to report a write error to.
	_, _ = w.Write(body.Bytes())
}
