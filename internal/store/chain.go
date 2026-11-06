package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/glsmith3/stavewell/internal/record"
)

// ErrPastEnd means a chain verification would start more than one past the
// ledger's latest record.
var ErrPastEnd = errors.New("start is past the end of the ledger")

// ChainResult is the outcome of chain verification over part of a ledger.
type ChainResult struct {
	// Intact reports whether every record checked passed.
	Intact bool
	// First and Last are the sequence numbers of the first and last records
	// checked, or 0 if nothing was checked. If the chain is broken, Last is
	// the record that failed.
	First, Last int64
	// LastHash is the record hash of the last record checked, or empty if
	// the chain is broken or nothing was checked.
	LastHash string
	// ReachedLatest reports whether the check reached the ledger's latest
	// record.
	ReachedLatest bool
	// Broken is the sequence number of the first record that failed, or 0
	// if the chain is intact.
	Broken int64
}

// VerifyChain runs chain verification on up to limit records of the
// organization's ledger with the name, starting at sequence number from.
// When from is more than 1, the check of record from includes its link to
// record from − 1. Checking stops at the first record that fails.
//
// It returns ErrNotFound if the organization has no ledger with the name,
// and ErrPastEnd if from is more than one past the ledger's latest record.
// From exactly one past it, there's nothing to check, and the result is
// intact.
//
// It reads the stored values, not the parsed ones: a record hash covers
// created_at exactly as stored.
func (s *Store) VerifyChain(ctx context.Context, orgRowID int64, ledger string, from int64, limit int) (ChainResult, error) {
	if from < 1 || limit < 1 {
		return ChainResult{}, fmt.Errorf("store: invalid chain verification range from %d, limit %d", from, limit)
	}
	// One read transaction, so every query sees the same snapshot of the
	// ledger, even while records are appended.
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return ChainResult{}, fmt.Errorf("store: verifying chain: %w", err)
	}
	defer tx.Rollback()

	lid, err := ledgerRowID(ctx, tx, orgRowID, ledger)
	if errors.Is(err, ErrNotFound) {
		return ChainResult{}, ErrNotFound
	}
	if err != nil {
		return ChainResult{}, fmt.Errorf("store: verifying chain: %w", err)
	}
	var latest int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(sequence), 0) FROM records WHERE ledger_id = ?", lid).Scan(&latest); err != nil {
		return ChainResult{}, fmt.Errorf("store: verifying chain: %w", err)
	}
	switch {
	case from > latest+1:
		return ChainResult{}, ErrPastEnd
	case from == latest+1:
		return ChainResult{Intact: true, ReachedLatest: true}, nil
	}

	previous := record.ZeroHash
	if from > 1 {
		err := tx.QueryRowContext(ctx,
			"SELECT hash FROM records WHERE ledger_id = ? AND sequence = ?", lid, from-1).Scan(&previous)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The record before is missing, so record from can't link to
			// it.
			previous = ""
		case err != nil:
			return ChainResult{}, fmt.Errorf("store: verifying chain: %w", err)
		}
	}

	rows, err := tx.QueryContext(ctx,
		"SELECT "+recordColumns+" FROM records WHERE ledger_id = ? AND sequence >= ? ORDER BY sequence LIMIT ?",
		lid, from, limit)
	if err != nil {
		return ChainResult{}, fmt.Errorf("store: verifying chain: %w", err)
	}
	defer rows.Close()
	res := ChainResult{Intact: true}
	// data and v reuse their memory from one record to the next. data is
	// valid only until the next call to rows.Next, which is long enough.
	var data sql.RawBytes
	var v record.Verifier
	for rows.Next() {
		r := record.Stored{Ledger: ledger}
		if err := rows.Scan(&r.Sequence, &r.CreatedAt, &data, &r.DataHash, &r.PreviousHash, &r.Hash); err != nil {
			return ChainResult{}, fmt.Errorf("store: verifying chain: %w", err)
		}
		r.Data = data
		if res.First == 0 {
			res.First = r.Sequence
		}
		res.Last = r.Sequence
		if !v.Verify(r, previous) {
			res.Intact, res.Broken = false, r.Sequence
			break
		}
		previous = r.Hash
	}
	if err := rows.Err(); err != nil {
		return ChainResult{}, fmt.Errorf("store: verifying chain: %w", err)
	}
	if res.First == 0 {
		// The ledger's latest record is at or after from, but no record
		// was found there: the records from there on are missing.
		return ChainResult{First: from, Last: from, Broken: from}, nil
	}
	if res.Intact {
		res.LastHash = previous
	}
	res.ReachedLatest = res.Last == latest
	return res, nil
}
