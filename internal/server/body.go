package server

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/record"
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
//
// A check returns a *problemError for a problem that has its own error
// type. Any other error gets the general invalid-request error.
var jsonBodies = map[string]func([]byte) error{
	"POST /v1/ledgers":                strictJSON[api.CreateLedgerRequest],
	"POST /v1/ledgers/{name}/records": checkAppendBody,
}

// problemError is a request error with its own error response.
type problemError struct {
	problem problemType
	detail  string // written for callers in advance, like all detail text
	err     error  // the details, for the log
}

func (e *problemError) Error() string { return e.err.Error() }
func (e *problemError) Unwrap() error { return e.err }

// strictJSON checks that body is a single JSON value that decodes into a T
// with no unknown fields. encoding/json/v2 also matches field names exactly,
// rejects repeated fields and rejects invalid UTF-8.
func strictJSON[T any](body []byte) error {
	var v T
	return json.Unmarshal(body, &v, json.RejectUnknownMembers(true))
}

// Errors from checkAppendBody, for the log. They never quote the body.
var (
	errBodyNotObject = errors.New("request body isn't a JSON object")
	errBodyInvalid   = errors.New("request body isn't valid JSON")
	errBodyField     = errors.New("request body has a field other than data, or more than one data field")
	errBodyTrailing  = errors.New("request body has something after the JSON object")
	errBodyNoData    = errors.New("request body has no data field")
)

// checkAppendBody checks the body of an append: one JSON object with only
// the data field, whose value follows the rules for record data.
//
// It reads the body itself, rather than with strictJSON, for two reasons:
//   - Problems inside data get an error that names the rule they break
//     (invalid-record-data or record-too-large), not invalid-request.
//   - It stops at the first level nested more deeply than data may be.
//     Otherwise the JSON decoder's own limit of 10,000 levels would turn
//     very deeply nested data into a general invalid-request error.
func checkAppendBody(body []byte) error {
	// Repeated keys and invalid strings are allowed while finding data, so
	// that inside data they're reported as breaking the data rules.
	// Outside data, the only field allowed is data, once.
	dec := jsontext.NewDecoder(bytes.NewReader(body),
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	tok, err := dec.ReadToken()
	if err != nil {
		return errBodyInvalid
	}
	if tok.Kind() != '{' {
		return errBodyNotObject
	}
	var data []byte
	seen := false
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return errBodyInvalid
		}
		if name.String() != "data" || seen {
			return errBodyField
		}
		seen = true
		if data, err = readDataValue(dec, body); err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil { // the closing '}'
		return errBodyInvalid
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return errBodyTrailing
	}
	if !seen {
		return &problemError{problemInvalidRequest, detailMissingData, errBodyNoData}
	}
	if _, err := record.ParseData(data); err != nil {
		return dataProblem(err)
	}
	return nil
}

// readDataValue reads the data field's value from dec, just after its name,
// and returns the value's bytes from body, which dec is reading. It stops
// with a too-deep error at the first level nested more deeply than data may
// be.
func readDataValue(dec *jsontext.Decoder, body []byte) ([]byte, error) {
	start := dec.InputOffset()
	base := dec.StackDepth()
	for {
		if _, err := dec.ReadToken(); err != nil {
			return nil, errBodyInvalid
		}
		if dec.StackDepth()-base > record.MaxDepth {
			return nil, dataProblem(record.ErrTooDeep)
		}
		if dec.StackDepth() == base {
			break
		}
	}
	// The bytes read since the name are the colon, any whitespace and the
	// value. The decoder has checked their syntax.
	return bytes.TrimLeft(body[start:dec.InputOffset()], " \t\r\n:"), nil
}

// dataProblem returns the error response for err, an error from
// record.ParseData.
func dataProblem(err error) *problemError {
	invalid := func(detail string) *problemError {
		return &problemError{problemInvalidRecordData, detail, err}
	}
	switch {
	case errors.Is(err, record.ErrNotObject):
		return invalid(detailDataNotObject)
	case errors.Is(err, record.ErrNumber):
		return invalid(detailDataNumber)
	case errors.Is(err, record.ErrDuplicateKey):
		return invalid(detailDataDuplicateKey)
	case errors.Is(err, record.ErrInvalidString):
		return invalid(detailDataInvalidString)
	case errors.Is(err, record.ErrTooDeep):
		return invalid(detailDataTooDeep)
	case errors.Is(err, record.ErrTooLarge):
		return &problemError{problemRecordTooLarge, detailRecordTooLarge, err}
	default:
		// record.ErrInvalidJSON. checkAppendBody has already checked the
		// syntax, so this doesn't happen.
		return &problemError{problemInvalidRequest, detailInvalidJSON, err}
	}
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
		var pe *problemError
		if errors.As(err, &pe) {
			writeProblem(w, r, pe.problem, pe.detail)
		} else {
			writeProblem(w, r, problemInvalidRequest, detailInvalidBody)
		}
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return r, true
}
