package server

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/glsmith3/stavewell/internal/api"
)

// jsonBodies maps each route that takes a request body to a function that
// checks the body strictly. TestJSONBodiesMatchSpec checks that every
// operation with a request body in api/openapi.yaml is here.
//
// The generated code decodes bodies with encoding/json, which ignores
// unknown fields, matches field names in any letter case, takes the last of
// repeated fields and stops reading after the first JSON value. Checking the
// body here first means a body is accepted only if it's exactly one JSON
// object with the documented fields.
var jsonBodies = map[string]func([]byte) error{
	"POST /v1/ledgers": strictJSON[api.CreateLedgerRequest],
}

// strictJSON checks that body is a single JSON value that decodes into a T
// with no unknown fields. encoding/json/v2 also matches field names exactly,
// rejects repeated fields and rejects invalid UTF-8.
func strictJSON[T any](body []byte) error {
	var v T
	return json.Unmarshal(body, &v, json.RejectUnknownMembers(true))
}

// readBody reads the whole request body, up to maxBodyBytes, and checks it
// with check. If the body is too large or not valid, it sends an error and
// returns false. Otherwise it returns the request with the body ready to be
// read again.
//
// Reading the whole body, rather than limiting what the JSON decoder reads,
// means bytes after the JSON value count toward the limit too.
func readBody(logger *slog.Logger, w http.ResponseWriter, r *http.Request, check func([]byte) error) (*http.Request, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		logInvalid(logger, r, err)
		writeProblem(w, r, problemRequestTooLarge, detailRequestTooLarge)
		return nil, false
	case err != nil:
		logInvalid(logger, r, err)
		writeProblem(w, r, problemInvalidRequest, detailInvalidRequest)
		return nil, false
	}
	if err := check(body); err != nil {
		logInvalid(logger, r, err)
		writeProblem(w, r, problemInvalidRequest, detailInvalidBody)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return r, true
}
