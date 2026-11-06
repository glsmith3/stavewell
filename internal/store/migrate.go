package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// migrationFiles holds the schema migrations, one SQL file per version,
// named NNNN_description.sql. Never edit a migration that has been merged:
// add a new one instead, because existing databases have already applied
// the old one.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one schema change.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the embedded migrations in version order. It fails
// unless the versions run 1, 2, 3 and so on, with no gaps or duplicates.
func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, name := range names {
		base := path.Base(name)
		prefix, _, ok := strings.Cut(base, "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil || len(prefix) != 4 {
			return nil, fmt.Errorf("store: migration %s isn't named NNNN_description.sql", base)
		}
		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: version, name: base, sql: string(body)})
	}
	slices.SortFunc(ms, func(a, b migration) int { return a.version - b.version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration %s has version %d, want %d", m.name, m.version, i+1)
		}
	}
	return ms, nil
}

// migrate applies any pending migrations to db, all in one transaction.
//
// The transaction takes the write lock when it begins (the write pool uses
// _txlock=immediate), so if two processes open a new database at once, one
// waits for the other and then finds nothing left to apply.
func migrate(ctx context.Context, db *sql.DB) (err error) {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	latest := ms[len(ms)-1].version

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: starting migration: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT    NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	var current int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("store: reading schema version: %w", err)
	}
	if current > latest {
		return fmt.Errorf("store: database schema version %d is newer than this version of Stavewell supports (%d)", current, latest)
	}

	now := FormatTime(time.Now())
	for _, m := range ms[current:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("store: applying migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", m.version, now); err != nil {
			return fmt.Errorf("store: recording migration %s: %w", m.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing migrations: %w", err)
	}
	return nil
}
