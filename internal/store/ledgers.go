package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Errors returned by the ledger functions. ErrNotFound means no ledger has
// the given name in the organization.
var (
	// ErrLedgerExists means the organization already has a ledger with the
	// name.
	ErrLedgerExists = errors.New("ledger already exists")
	// ErrLedgerLimit means the organization already has MaxLedgers ledgers.
	ErrLedgerLimit = errors.New("organization has the most ledgers allowed")
)

// MaxLedgers is the most ledgers an organization can have.
const MaxLedgers = 100

// ledgerName is the form of a ledger name: 1 to 64 lowercase letters, digits
// and hyphens, starting with a letter. The schema checks the same rule.
var ledgerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// ValidLedgerName reports whether name is a valid ledger name.
func ValidLedgerName(name string) bool {
	return ledgerName.MatchString(name)
}

// Ledger is a named, append-only sequence of records in an organization.
type Ledger struct {
	Name      string
	CreatedAt time.Time
	// LatestSequence is the sequence number of the ledger's latest record,
	// or 0 if the ledger has no records.
	LatestSequence int64
	// LatestHash is the record hash of the ledger's latest record, or empty
	// if the ledger has no records.
	LatestHash string
}

// ledgerColumns selects a ledger's columns, including its latest record's
// sequence number and hash, from the ledgers table as l. The latest record
// is found through the records table's primary key.
const ledgerColumns = `l.name, l.created_at, r.sequence, r.hash
	FROM ledgers l
	LEFT JOIN records r ON r.ledger_id = l.id
	 AND r.sequence = (SELECT MAX(sequence) FROM records WHERE ledger_id = l.id)`

// scanLedger reads a row selected with ledgerColumns.
func scanLedger(row interface{ Scan(...any) error }) (Ledger, error) {
	var l Ledger
	var created string
	var seq sql.NullInt64
	var hash sql.NullString
	if err := row.Scan(&l.Name, &created, &seq, &hash); err != nil {
		return Ledger{}, err
	}
	t, err := parseTime(created)
	if err != nil {
		return Ledger{}, err
	}
	l.CreatedAt, l.LatestSequence, l.LatestHash = t, seq.Int64, hash.String
	return l, nil
}

// CreateLedger adds an empty ledger to the organization with internal row
// number orgRowID. It returns ErrLedgerExists if the organization already
// has a ledger with the name, and ErrLedgerLimit if the organization already
// has MaxLedgers ledgers.
func (s *Store) CreateLedger(ctx context.Context, orgRowID int64, name string, now time.Time) (Ledger, error) {
	if !ValidLedgerName(name) {
		return Ledger{}, fmt.Errorf("store: invalid ledger name %q", name)
	}
	// The write transaction holds the write lock from the start, so the
	// checks below and the insert can't race with another request.
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return Ledger{}, fmt.Errorf("store: creating ledger: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	err = tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM ledgers WHERE organization_id = ? AND name = ?)", orgRowID, name).Scan(&exists)
	if err != nil {
		return Ledger{}, fmt.Errorf("store: creating ledger: %w", err)
	}
	if exists {
		return Ledger{}, ErrLedgerExists
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ledgers WHERE organization_id = ?", orgRowID).Scan(&count); err != nil {
		return Ledger{}, fmt.Errorf("store: creating ledger: %w", err)
	}
	if count >= MaxLedgers {
		return Ledger{}, ErrLedgerLimit
	}

	l := Ledger{Name: name, CreatedAt: truncate(now)}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO ledgers (organization_id, name, created_at) VALUES (?, ?, ?)",
		orgRowID, l.Name, FormatTime(l.CreatedAt)); err != nil {
		return Ledger{}, fmt.Errorf("store: creating ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Ledger{}, fmt.Errorf("store: creating ledger: %w", err)
	}
	return l, nil
}

// GetLedger returns the organization's ledger with the name, or ErrNotFound.
func (s *Store) GetLedger(ctx context.Context, orgRowID int64, name string) (Ledger, error) {
	l, err := scanLedger(s.read.QueryRowContext(ctx,
		"SELECT "+ledgerColumns+" WHERE l.organization_id = ? AND l.name = ?", orgRowID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Ledger{}, ErrNotFound
	}
	if err != nil {
		return Ledger{}, fmt.Errorf("store: getting ledger: %w", err)
	}
	return l, nil
}

// ListLedgers returns up to limit of the organization's ledgers, in the order
// they were created, starting after the ledger named after (or at the start,
// if after is empty). more reports whether further ledgers exist. It returns
// ErrNotFound if after isn't one of the organization's ledgers.
func (s *Store) ListLedgers(ctx context.Context, orgRowID int64, after string, limit int) (ledgers []Ledger, more bool, err error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("store: limit %d is less than 1", limit)
	}
	// Ledgers are never deleted, so row numbers only grow: ordering by them
	// gives creation order, and positions in the list never move.
	var afterRowID int64
	if after != "" {
		err := s.read.QueryRowContext(ctx,
			"SELECT id FROM ledgers WHERE organization_id = ? AND name = ?", orgRowID, after).Scan(&afterRowID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		if err != nil {
			return nil, false, fmt.Errorf("store: listing ledgers: %w", err)
		}
	}
	rows, err := s.read.QueryContext(ctx,
		"SELECT "+ledgerColumns+" WHERE l.organization_id = ? AND l.id > ? ORDER BY l.id LIMIT ?",
		orgRowID, afterRowID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: listing ledgers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanLedger(rows)
		if err != nil {
			return nil, false, fmt.Errorf("store: listing ledgers: %w", err)
		}
		ledgers = append(ledgers, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: listing ledgers: %w", err)
	}
	if len(ledgers) > limit {
		return ledgers[:limit], true, nil
	}
	return ledgers, false, nil
}
