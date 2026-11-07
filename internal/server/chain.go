package server

import (
	"context"
	"errors"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/store"
)

// maxChainRecords is the most records one chain verification request
// checks.
const maxChainRecords = 10_000

// VerifyChain runs chain verification on one of the caller's organization's
// ledgers. A broken chain is a finding, not an error: it gets 200.
func (s *Server) VerifyChain(ctx context.Context, req api.VerifyChainRequestObject) (api.VerifyChainResponseObject, error) {
	p, ok := principalFrom(ctx)
	if !ok {
		return nil, errNoPrincipal
	}
	invalidFrom := api.VerifyChain400ApplicationProblemPlusJSONResponse(newProblem(ctx, problemInvalidRequest, detailInvalidFrom))
	from := int64(1)
	if req.Params.From != nil {
		from = *req.Params.From
	}
	if from < 1 {
		return invalidFrom, nil
	}
	notFound := api.VerifyChain404ApplicationProblemPlusJSONResponse(newProblem(ctx, problemLedgerNotFound, detailLedgerNotFound))
	if !store.ValidLedgerName(req.Name) {
		return notFound, nil // no ledger can have this name
	}
	res, err := s.db.VerifyChain(ctx, p.OrgRowID, req.Name, from, maxChainRecords)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return notFound, nil
	case errors.Is(err, store.ErrPastEnd):
		return invalidFrom, nil
	case err != nil:
		return nil, err
	}
	return api.VerifyChain200JSONResponse(chainJSON(req.Name, res)), nil
}

// chainJSON returns the API form of a chain verification result.
func chainJSON(ledger string, res store.ChainResult) api.ChainVerification {
	optional := func(n int64) *int64 {
		if n == 0 {
			return nil
		}
		return &n
	}
	j := api.ChainVerification{
		Ledger:         ledger,
		Intact:         res.Intact,
		FirstSequence:  optional(res.First),
		LastSequence:   optional(res.Last),
		ReachedLatest:  res.ReachedLatest,
		BrokenSequence: optional(res.Broken),
	}
	if res.LastHash != "" {
		j.LastHash = &res.LastHash
	}
	return j
}
