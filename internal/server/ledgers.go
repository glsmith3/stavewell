package server

import (
	"context"
	"errors"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/cursor"
	"github.com/glsmith3/stavewell/internal/store"
)

// Page sizes for list endpoints.
const (
	defaultPageSize = 100
	maxPageSize     = 100
)

// errNoPrincipal means a handler that needs an API key ran without one. It
// can only happen if requiredScopes is missing a route, which a test checks.
var errNoPrincipal = errors.New("server: no authenticated caller for a route that needs one")

// ledgerJSON returns the API form of l.
func ledgerJSON(l store.Ledger) api.Ledger {
	j := api.Ledger{Name: l.Name, CreatedAt: store.FormatTime(l.CreatedAt)}
	if l.LatestSequence > 0 {
		j.LatestSequence, j.LatestHash = &l.LatestSequence, &l.LatestHash
	}
	return j
}

// CreateLedger creates a ledger in the caller's organization.
func (s *Server) CreateLedger(ctx context.Context, req api.CreateLedgerRequestObject) (api.CreateLedgerResponseObject, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	if req.Body == nil || !store.ValidLedgerName(req.Body.Name) {
		return api.CreateLedger400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidRequest, detailInvalidLedgerName)), nil
	}
	l, err := s.db.CreateLedger(ctx, p.OrgRowID, req.Body.Name, s.now())
	switch {
	case errors.Is(err, store.ErrLedgerExists):
		return api.CreateLedger409ApplicationProblemPlusJSONResponse(newProblem(ctx, problemLedgerExists, detailLedgerExists)), nil
	case errors.Is(err, store.ErrLedgerLimit):
		return api.CreateLedger409ApplicationProblemPlusJSONResponse(newProblem(ctx, problemOrgLimit, detailOrgLimit)), nil
	case err != nil:
		return nil, err
	}
	return api.CreateLedger201JSONResponse(ledgerJSON(l)), nil
}

// ListLedgers lists the caller's organization's ledgers, a page at a time.
func (s *Server) ListLedgers(ctx context.Context, req api.ListLedgersRequestObject) (api.ListLedgersResponseObject, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	limit := defaultPageSize
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if limit < 1 || limit > maxPageSize {
		return api.ListLedgers400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidRequest, detailInvalidLimit)), nil
	}
	invalidCursor := api.ListLedgers400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidCursor, detailInvalidCursor))

	after := ""
	if req.Params.Cursor != nil {
		pos, err := cursor.Decode(*req.Params.Cursor, cursor.ListLedgers, p.OrganizationID)
		if err != nil {
			return invalidCursor, nil
		}
		after = pos.After
	}
	ledgers, more, err := s.db.ListLedgers(ctx, p.OrgRowID, after, limit)
	if errors.Is(err, store.ErrNotFound) {
		// The cursor names a ledger this organization doesn't have.
		return invalidCursor, nil
	}
	if err != nil {
		return nil, err
	}

	page := api.LedgerList{Ledgers: make([]api.Ledger, 0, len(ledgers)), HasMore: more}
	for _, l := range ledgers {
		page.Ledgers = append(page.Ledgers, ledgerJSON(l))
	}
	// The next page starts after the last ledger on this one. On an empty
	// page, it starts where this one did, so the cursor still works later.
	if len(ledgers) > 0 {
		after = ledgers[len(ledgers)-1].Name
	}
	page.NextCursor = cursor.Encode(cursor.Position{List: cursor.ListLedgers, OrganizationID: p.OrganizationID, After: after})
	return api.ListLedgers200JSONResponse(page), nil
}

// GetLedger returns one of the caller's organization's ledgers. A ledger in
// another organization gets the same 404 as one that doesn't exist.
func (s *Server) GetLedger(ctx context.Context, req api.GetLedgerRequestObject) (api.GetLedgerResponseObject, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	notFound := api.GetLedger404ApplicationProblemPlusJSONResponse(newProblem(ctx, problemLedgerNotFound, detailLedgerNotFound))
	if !store.ValidLedgerName(req.Name) {
		return notFound, nil // no ledger can have this name
	}
	l, err := s.db.GetLedger(ctx, p.OrgRowID, req.Name)
	if errors.Is(err, store.ErrNotFound) {
		return notFound, nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetLedger200JSONResponse(ledgerJSON(l)), nil
}
