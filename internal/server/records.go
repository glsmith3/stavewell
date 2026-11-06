package server

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/cursor"
	"github.com/glsmith3/stavewell/internal/record"
	"github.com/glsmith3/stavewell/internal/store"
)

// recordJSON returns the API form of r.
func recordJSON(r store.Record) api.Record {
	return api.Record{
		Ledger:       r.Ledger,
		Sequence:     r.Sequence,
		CreatedAt:    store.FormatTime(r.CreatedAt),
		Data:         jsonv1.RawMessage(r.Data),
		DataHash:     r.DataHash,
		PreviousHash: r.PreviousHash,
		Hash:         r.Hash,
	}
}

// recordResponse is a successful response from a record endpoint. It's
// encoded with encoding/json/v2, because the generated code's encoding/json
// escapes <, >, &, U+2028 and U+2029 in strings, so record data wouldn't
// appear in its canonical form.
type recordResponse struct {
	status int
	body   any // an api.Record or api.RecordList
}

func (resp recordResponse) write(w http.ResponseWriter) error {
	var b bytes.Buffer
	if err := json.MarshalWrite(&b, resp.body); err != nil {
		return err
	}
	b.WriteByte('\n') // as the generated code's responses end
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.status)
	// If the client has gone away, there's no one to report a write error to.
	_, _ = w.Write(b.Bytes())
	return nil
}

func (resp recordResponse) VisitAppendRecordResponse(w http.ResponseWriter) error {
	return resp.write(w)
}

func (resp recordResponse) VisitListRecordsResponse(w http.ResponseWriter) error {
	return resp.write(w)
}

func (resp recordResponse) VisitGetRecordResponse(w http.ResponseWriter) error {
	return resp.write(w)
}

// AppendRecord appends a record to one of the caller's organization's
// ledgers. checkAppendBody has already checked the body.
func (s *Server) AppendRecord(ctx context.Context, req api.AppendRecordRequestObject) (api.AppendRecordResponseObject, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if req.Body == nil {
		return api.AppendRecord400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidRequest, detailMissingData)), nil
	}
	// checkAppendBody has already checked data the same way, so through HTTP
	// this never fails. It stays as a backstop, because the store accepts
	// only a parsed record.Data. TestAppendRecordHandlerChecksData covers it.
	data, err := record.ParseData(req.Body.Data)
	if err != nil {
		pe := dataProblem(err)
		if pe.problem.status == http.StatusRequestEntityTooLarge {
			return api.AppendRecord413ApplicationProblemPlusJSONResponse(newProblem(ctx, pe.problem, pe.detail)), nil
		}
		return api.AppendRecord400ApplicationProblemPlusJSONResponse(newProblem(ctx, pe.problem, pe.detail)), nil
	}
	notFound := api.AppendRecord404ApplicationProblemPlusJSONResponse(newProblem(ctx, problemLedgerNotFound, detailLedgerNotFound))
	if !store.ValidLedgerName(req.Name) {
		return notFound, nil // no ledger can have this name
	}
	r, err := s.db.AppendRecord(ctx, p.OrgRowID, req.Name, data, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return notFound, nil
	}
	if err != nil {
		return nil, err
	}
	return recordResponse{http.StatusCreated, recordJSON(r)}, nil
}

// ListRecords lists the records in one of the caller's organization's
// ledgers, a page at a time.
func (s *Server) ListRecords(ctx context.Context, req api.ListRecordsRequestObject) (api.ListRecordsResponseObject, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	limit := defaultPageSize
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if limit < 1 || limit > maxPageSize {
		return api.ListRecords400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidRequest, detailInvalidLimit)), nil
	}
	invalidCursor := api.ListRecords400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidCursor, detailInvalidCursor))
	list := cursor.ListRecords(req.Name)

	var after int64
	if req.Params.Cursor != nil {
		pos, err := cursor.Decode(*req.Params.Cursor, list, p.OrganizationID)
		if err != nil {
			return invalidCursor, nil
		}
		if after, ok = parseAfter(pos.After); !ok {
			return invalidCursor, nil
		}
	}
	notFound := api.ListRecords404ApplicationProblemPlusJSONResponse(newProblem(ctx, problemLedgerNotFound, detailLedgerNotFound))
	if !store.ValidLedgerName(req.Name) {
		return notFound, nil // no ledger can have this name
	}
	records, more, err := s.db.ListRecords(ctx, p.OrgRowID, req.Name, after, limit)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return notFound, nil
	case errors.Is(err, store.ErrRecordNotFound):
		// The cursor names a record this ledger doesn't have.
		return invalidCursor, nil
	case err != nil:
		return nil, err
	}

	page := api.RecordList{Records: make([]api.Record, 0, len(records)), HasMore: more}
	for _, r := range records {
		page.Records = append(page.Records, recordJSON(r))
	}
	// The next page starts after the last record on this one. On an empty
	// page, it starts where this one did, so the cursor still works later.
	if len(records) > 0 {
		after = records[len(records)-1].Sequence
	}
	page.NextCursor = cursor.Encode(cursor.Position{List: list, OrganizationID: p.OrganizationID, After: formatAfter(after)})
	return recordResponse{http.StatusOK, page}, nil
}

// formatAfter returns the cursor form of the sequence number a page starts
// after: empty for the start of the ledger.
func formatAfter(after int64) string {
	if after == 0 {
		return ""
	}
	return strconv.FormatInt(after, 10)
}

// parseAfter is the reverse of formatAfter. It accepts only what formatAfter
// returns.
func parseAfter(s string) (int64, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

// GetRecord returns one record from one of the caller's organization's
// ledgers.
func (s *Server) GetRecord(ctx context.Context, req api.GetRecordRequestObject) (api.GetRecordResponseObject, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if req.Sequence < 1 {
		return api.GetRecord400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidRequest, detailInvalidSequence)), nil
	}
	ledgerNotFound := api.GetRecord404ApplicationProblemPlusJSONResponse(newProblem(ctx, problemLedgerNotFound, detailLedgerNotFound))
	if !store.ValidLedgerName(req.Name) {
		return ledgerNotFound, nil // no ledger can have this name
	}
	r, err := s.db.GetRecord(ctx, p.OrgRowID, req.Name, req.Sequence)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ledgerNotFound, nil
	case errors.Is(err, store.ErrRecordNotFound):
		return api.GetRecord404ApplicationProblemPlusJSONResponse(newProblem(ctx, problemRecordNotFound, detailRecordNotFound)), nil
	case err != nil:
		return nil, err
	}
	return recordResponse{http.StatusOK, recordJSON(r)}, nil
}
