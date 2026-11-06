package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/glsmith3/stavewell/internal/record"
)

// ErrRecordNotFound means the ledger has no record with the given sequence
// number.
var ErrRecordNotFound = errors.New("record not found")

// Record is one item appended to a ledger. It never changes after it's
// written.
type Record struct {
	Ledger       string
	Sequence     int64
	CreatedAt    time.Time
	Data         string // canonical JSON
	DataHash     string
	PreviousHash string
	Hash         string
}

// queryRower is a *sql.DB or *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ledgerRowID returns the internal row number of the organization's ledger
// with the name, or ErrNotFound.
func ledgerRowID(ctx context.Context, q queryRower, orgRowID int64, name string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		"SELECT id FROM ledgers WHERE organization_id = ? AND name = ?", orgRowID, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// AppendRecord appends a record with data to the organization's ledger with
// the name, and returns the record. It returns ErrNotFound if the
// organization has no ledger with the name.
//
// The record's sequence number is one more than the ledger's latest record's,
// or 1 for the first record. Its created_at is now, or the latest record's
// created_at if that's later, so times never go backward in a ledger.
//
// In the same transaction, AppendRecord adds the data's canonical size to the
// organization's stored data total, and counts the record in the
// organization's usage for the UTC day of its created_at.
func (s *Store) AppendRecord(ctx context.Context, orgRowID int64, ledger string, data record.Data, now time.Time) (Record, error) {
	if data.IsZero() {
		return Record{}, errors.New("store: appending a record without data")
	}
	// The write transaction holds the write lock from the start, so the
	// latest record can't change before the insert.
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	}
	defer tx.Rollback()

	lid, err := ledgerRowID(ctx, tx, orgRowID, ledger)
	if errors.Is(err, ErrNotFound) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	}

	r := Record{
		Ledger:       ledger,
		Sequence:     1,
		CreatedAt:    truncate(now),
		Data:         data.Canonical(),
		DataHash:     data.Hash(),
		PreviousHash: record.ZeroHash,
	}
	var latestSeq int64
	var latestCreated, latestHash string
	err = tx.QueryRowContext(ctx,
		"SELECT sequence, created_at, hash FROM records WHERE ledger_id = ? ORDER BY sequence DESC LIMIT 1",
		lid).Scan(&latestSeq, &latestCreated, &latestHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// This is the ledger's first record.
	case err != nil:
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	default:
		latestTime, err := parseTime(latestCreated)
		if err != nil {
			return Record{}, err
		}
		r.Sequence = latestSeq + 1
		r.PreviousHash = latestHash
		if r.CreatedAt.Before(latestTime) {
			r.CreatedAt = latestTime
		}
	}
	created := FormatTime(r.CreatedAt)
	r.Hash, err = record.Hash(record.Fields{
		Ledger:       r.Ledger,
		Sequence:     r.Sequence,
		CreatedAt:    created,
		DataHash:     r.DataHash,
		PreviousHash: r.PreviousHash,
	})
	if err != nil {
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		lid, r.Sequence, created, r.Data, r.DataHash, r.PreviousHash, r.Hash); err != nil {
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE organizations SET stored_data_bytes = stored_data_bytes + ? WHERE id = ?",
		len(r.Data), orgRowID); err != nil {
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO daily_usage (organization_id, day, records_appended) VALUES (?, ?, 1)
		 ON CONFLICT (organization_id, day) DO UPDATE SET records_appended = records_appended + 1`,
		orgRowID, r.CreatedAt.Format(time.DateOnly)); err != nil {
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("store: appending record: %w", err)
	}
	return r, nil
}

// recordColumns are the columns scanRecord reads, in order.
const recordColumns = "sequence, created_at, data, data_hash, previous_hash, hash"

// scanRecord reads a row selected with recordColumns into a record of the
// ledger.
func scanRecord(row interface{ Scan(...any) error }, ledger string) (Record, error) {
	r := Record{Ledger: ledger}
	var created string
	if err := row.Scan(&r.Sequence, &created, &r.Data, &r.DataHash, &r.PreviousHash, &r.Hash); err != nil {
		return Record{}, err
	}
	t, err := parseTime(created)
	if err != nil {
		return Record{}, err
	}
	r.CreatedAt = t
	return r, nil
}

// GetRecord returns the record with the sequence number from the
// organization's ledger with the name. It returns ErrNotFound if the
// organization has no ledger with the name, and ErrRecordNotFound if the
// ledger has no record with the sequence number.
func (s *Store) GetRecord(ctx context.Context, orgRowID int64, ledger string, sequence int64) (Record, error) {
	lid, err := ledgerRowID(ctx, s.read, orgRowID, ledger)
	if errors.Is(err, ErrNotFound) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("store: getting record: %w", err)
	}
	r, err := scanRecord(s.read.QueryRowContext(ctx,
		"SELECT "+recordColumns+" FROM records WHERE ledger_id = ? AND sequence = ?", lid, sequence), ledger)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrRecordNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("store: getting record: %w", err)
	}
	return r, nil
}

// ListRecords returns up to limit records from the organization's ledger with
// the name, in sequence order, starting after sequence number after (or at
// record 1, if after is 0). more reports whether further records exist.
//
// It returns ErrNotFound if the organization has no ledger with the name,
// and ErrRecordNotFound if after isn't 0 and the ledger has no record with
// that sequence number.
func (s *Store) ListRecords(ctx context.Context, orgRowID int64, ledger string, after int64, limit int) (records []Record, more bool, err error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("store: limit %d is less than 1", limit)
	}
	if after < 0 {
		return nil, false, ErrRecordNotFound
	}
	lid, err := ledgerRowID(ctx, s.read, orgRowID, ledger)
	if errors.Is(err, ErrNotFound) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: listing records: %w", err)
	}
	if after > 0 {
		var exists bool
		err := s.read.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM records WHERE ledger_id = ? AND sequence = ?)", lid, after).Scan(&exists)
		if err != nil {
			return nil, false, fmt.Errorf("store: listing records: %w", err)
		}
		if !exists {
			return nil, false, ErrRecordNotFound
		}
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT "+recordColumns+" FROM records WHERE ledger_id = ? AND sequence > ? ORDER BY sequence LIMIT ?",
		lid, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: listing records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRecord(rows, ledger)
		if err != nil {
			return nil, false, fmt.Errorf("store: listing records: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: listing records: %w", err)
	}
	if len(records) > limit {
		return records[:limit], true, nil
	}
	return records, false, nil
}
