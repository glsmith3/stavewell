// Package store keeps Stavewell's data in an SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	// A pure-Go SQLite driver, so the server still builds to a single file
	// without a C compiler. It registers the "sqlite" driver name.
	"modernc.org/sqlite"
)

// Store is Stavewell's database.
//
// Writes go through a pool of exactly one connection, so they happen one at a
// time, and every write transaction takes the database's write lock as soon
// as it begins. Reads use a separate pool of read-only connections, which
// WAL mode lets run while a write is in progress.
type Store struct {
	write *sql.DB
	read  *sql.DB

	// afterLatest, if set, runs inside VerifyChain just after it reads the
	// ledger's latest sequence number. Tests use it to append a record at
	// that moment.
	afterLatest func()
}

// maxReadConns is the most read connections open at once.
const maxReadConns = 4

// Connection settings, applied to every new connection. The driver runs
// busy_timeout first, then the rest in alphabetical order.
//
//   - busy_timeout: wait up to 5 seconds for a lock held by another
//     connection or process (for example, an admin command) before failing.
//   - foreign_keys: enforce links between tables. SQLite doesn't by default.
//   - journal_mode=WAL: let reads run while a write is in progress. The
//     setting is stored in the database file.
//   - synchronous=FULL: a committed write is on disk before the commit
//     returns, so a record reported as stored survives a power cut.
//   - query_only: read connections can't change anything.
//   - _txlock=immediate: write transactions take the write lock when they
//     begin, so two writers never deadlock trying to upgrade a read lock.
const (
	writeParams = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	readParams  = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=query_only(1)"
)

// TimeFormat is the format of every time stored in the database: UTC, with
// exactly three fractional digits, for example 2026-11-06T19:22:31.123Z. It
// matches the format the API returns.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// FormatTime formats t in TimeFormat, in UTC.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeFormat)
}

// Open opens the database file at path, creating it if it doesn't exist, and
// applies any pending schema migrations. A new database file is readable and
// writable only by its owner.
//
// Open fails if the database was created by a newer version of Stavewell,
// with schema changes this version doesn't know about.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if err := createPrivateFile(path); err != nil {
		return nil, fmt.Errorf("store: creating database file: %w", err)
	}

	write, err := openWriter(ctx, path)
	if err != nil {
		return nil, err
	}

	read, err := sql.Open("sqlite", path+"?"+readParams)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("store: opening database: %w", err)
	}
	read.SetMaxOpenConns(maxReadConns)
	if err := read.PingContext(ctx); err != nil {
		read.Close()
		write.Close()
		return nil, fmt.Errorf("store: opening database: %w", err)
	}

	return &Store{write: write, read: read}, nil
}

// Retrying a busy database while opening it: how many attempts, and the
// wait before the second attempt. Each later wait is longer by the same
// amount, so the attempts span about two seconds in all.
const (
	openAttempts  = 20
	openRetryStep = 10 * time.Millisecond
)

// openWriter opens the write pool and applies any pending migrations.
//
// A new database file starts in SQLite's default journal mode, and switching
// it to WAL needs an exclusive lock. If two processes switch it at the same
// moment, SQLite reports the database as busy at once, without waiting for
// busy_timeout, to avoid a deadlock. So a busy error here is retried.
func openWriter(ctx context.Context, path string) (*sql.DB, error) {
	for attempt := 1; ; attempt++ {
		db, err := sql.Open("sqlite", path+"?"+writeParams)
		if err != nil {
			return nil, fmt.Errorf("store: opening database: %w", err)
		}
		db.SetMaxOpenConns(1)
		err = migrate(ctx, db)
		if err == nil {
			return db, nil
		}
		db.Close()
		if !isBusy(err) || attempt == openAttempts {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * openRetryStep):
		}
	}
}

// sqliteBusy is SQLite's result code for a database locked by another
// connection (SQLITE_BUSY).
const sqliteBusy = 5

// isBusy reports whether err is SQLite's busy error. Extended result codes
// keep the primary code in their low byte.
func isBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqliteBusy
}

// checkPath rejects database paths that the driver or SQLite would read as
// something other than a plain file name.
func checkPath(path string) error {
	switch {
	case path == "":
		return errors.New("store: database path is empty")
	case strings.Contains(path, "?"):
		// The driver treats everything after the first "?" as settings.
		return errors.New(`store: database path can't contain "?"`)
	case strings.HasPrefix(path, "file:"):
		// SQLite reads a "file:" name as a URI.
		return errors.New(`store: database path can't start with "file:"`)
	case strings.HasPrefix(path, ":"):
		// SQLite reads names such as ":memory:" specially.
		return errors.New(`store: database path can't start with ":"`)
	}
	return nil
}

// createPrivateFile creates an empty file at path, readable and writable
// only by its owner, unless a file is already there. SQLite gives the -wal
// and -shm files it creates beside the database the same permissions.
func createPrivateFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Close closes the database.
func (s *Store) Close() error {
	return errors.Join(s.read.Close(), s.write.Close())
}

// SchemaVersion returns the version of the latest schema migration applied
// to the database.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.read.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("store: reading schema version: %w", err)
	}
	return v, nil
}
