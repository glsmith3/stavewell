package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glsmith3/stavewell/internal/ids"
)

// openTestStore opens a new database in a temporary directory.
func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stavewell.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	_, path := openTestStore(t)
	// Opening ran a migration, which is a write, so the -wal file exists too.
	for _, p := range []string{path, path + "-wal"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(p), err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s permissions = %o, want 600", filepath.Base(p), perm)
		}
	}
}

func TestOpenAppliesMigrationsOnce(t *testing.T) {
	ctx := context.Background()
	s, path := openTestStore(t)
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	latest := ms[len(ms)-1].version

	if v, err := s.SchemaVersion(ctx); err != nil || v != latest {
		t.Fatalf("SchemaVersion() = %d, %v; want %d", v, err, latest)
	}
	wantTables := []string{"api_key_scopes", "api_keys", "daily_usage", "ledgers", "organizations", "records", "schema_migrations"}
	var got []string
	rows, err := s.read.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(wantTables, ",") {
		t.Errorf("tables = %v, want %v", got, wantTables)
	}

	// Opening the same file again applies nothing new.
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open() = %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.read.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(ms) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(ms))
	}
}

func TestOpenConcurrently(t *testing.T) {
	// Two processes, such as the server and an admin command, can open a new
	// database at the same moment. Both must succeed, and the migrations must
	// be applied exactly once.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stavewell.db")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			s, err := Open(ctx, path)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Open() #%d = %v", i, err)
		}
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.read.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("schema_migrations has %d rows, want 1", n)
	}
}

func TestWriteTransactionsLockImmediately(t *testing.T) {
	// A write transaction must hold the write lock from the moment it begins.
	// Otherwise two transactions that read before writing (as an append
	// does) can both hold read locks and deadlock when they try to write.
	ctx := context.Background()
	s, path := openTestStore(t)
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err == nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		t.Error("another connection took the write lock while a write transaction was open; want the lock held from BeginTx")
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	s, path := openTestStore(t)
	if _, err := s.write.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (999, '2026-11-06T00:00:00.000Z')"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err := Open(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("Open() = %v, want an error about a newer schema", err)
	}
}

func TestOpenRejectsSpecialPaths(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"", filepath.Join(dir, "a?b.db"), "file:" + filepath.Join(dir, "a.db"), ":memory:"} {
		if s, err := Open(context.Background(), path); err == nil {
			s.Close()
			t.Errorf("Open(%q) succeeded, want an error", path)
		}
	}
}

func TestConnectionSettings(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	tests := []struct {
		pool   string
		pragma string
		want   string
	}{
		{"write", "journal_mode", "wal"},
		{"write", "synchronous", "2"}, // FULL
		{"write", "foreign_keys", "1"},
		{"write", "busy_timeout", "5000"},
		{"write", "query_only", "0"},
		{"read", "foreign_keys", "1"},
		{"read", "busy_timeout", "5000"},
		{"read", "query_only", "1"},
	}
	for _, tt := range tests {
		db := s.write
		if tt.pool == "read" {
			db = s.read
		}
		var got string
		if err := db.QueryRowContext(ctx, "PRAGMA "+tt.pragma).Scan(&got); err != nil {
			t.Fatalf("%s pool: PRAGMA %s: %v", tt.pool, tt.pragma, err)
		}
		if got != tt.want {
			t.Errorf("%s pool: %s = %s, want %s", tt.pool, tt.pragma, got, tt.want)
		}
	}
}

func TestReadPoolCannotWrite(t *testing.T) {
	s, _ := openTestStore(t)
	_, err := s.read.ExecContext(context.Background(),
		"INSERT INTO organizations (public_id, name, created_at) VALUES ('org_aaaaaaaaaaaaaaaaaaaaaaaaaa', 'x', '2026-11-06T00:00:00.000Z')")
	if err == nil {
		t.Error("insert through the read pool succeeded, want an error")
	}
}

// Valid values for test rows.
const (
	testTime = "2026-11-06T19:22:31.123Z"
	zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"
)

// seed adds two organizations, each with a ledger named "orders", and one
// record in the first organization's ledger. Organization and ledger row
// numbers are 1 and 2.
func seed(t *testing.T, s *Store) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO organizations (id, public_id, name, created_at) VALUES
			(1, 'org_aaaaaaaaaaaaaaaaaaaaaaaaaa', 'Test one', '` + testTime + `'),
			(2, 'org_bbbbbbbbbbbbbbbbbbbbbbbbbb', 'Test two', '` + testTime + `')`,
		`INSERT INTO ledgers (id, organization_id, name, created_at) VALUES
			(1, 1, 'orders', '` + testTime + `'),
			(2, 2, 'orders', '` + testTime + `')`,
		`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash) VALUES
			(1, 1, '` + testTime + `', '{}', '` + zeroHash + `', '` + zeroHash + `', '` + zeroHash + `')`,
		`INSERT INTO api_keys (id, public_id, organization_id, name, key_hash, created_at, expires_at) VALUES
			(1, 'key_aaaaaaaaaaaaaaaaaaaaaaaaaa', 1, 'Test key', '` + zeroHash + `', '` + testTime + `', '` + testTime + `')`,
	} {
		if _, err := s.write.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seeding: %v\n%s", err, stmt)
		}
	}
}

// dbSnapshot returns the contents of every table, as one string.
func dbSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	var tables []string
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, table, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return b.String()
}

func TestProtectedRowsCannotChange(t *testing.T) {
	ctx := context.Background()
	record := func(verb, seq, data, key string) string {
		return verb + ` INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash, idempotency_key)
			VALUES (1, ` + seq + `, '` + testTime + `', '` + data + `', '` + zeroHash + `', '` + zeroHash + `', '` + zeroHash + `', ` + key + `)`
	}
	otherHash := strings.Repeat("9", 64)
	tests := []struct {
		name  string
		setup string // optional, run before the snapshot
		stmt  string
		want  string // part of the expected error message
	}{
		{"record: update data", "", `UPDATE records SET data = '{"changed":1}' WHERE ledger_id = 1 AND sequence = 1`, "append-only"},
		{"record: update idempotency key", "", `UPDATE records SET idempotency_key = 'k' WHERE ledger_id = 1 AND sequence = 1`, "append-only"},
		{"record: update or replace", "", `UPDATE OR REPLACE records SET sequence = 1 WHERE ledger_id = 1 AND sequence = 2`, "append-only"},
		{"record: delete", "", `DELETE FROM records WHERE ledger_id = 1 AND sequence = 1`, "append-only"},
		{"record: insert or replace, same sequence", "", record("INSERT OR REPLACE", "1", `{"forged":true}`, "NULL"), "sequence number already used"},
		{"record: replace, same sequence", "", record("REPLACE", "1", `{"forged":true}`, "NULL"), "sequence number already used"},
		{"record: insert or replace, same idempotency key", "", record("INSERT OR REPLACE", "3", `{"forged":true}`, "'k2'"), "idempotency key already used"},
		{"record: insert or ignore, same idempotency key", "", record("INSERT OR IGNORE", "3", `{}`, "'k2'"), "idempotency key already used"},
		{"record: upsert", "", record("INSERT", "1", `{"forged":true}`, "NULL") + ` ON CONFLICT DO UPDATE SET data = excluded.data`, "sequence number already used"},

		{"ledger: move to another organization", "", `UPDATE ledgers SET organization_id = 2, name = 'moved' WHERE id = 1`, "ledgers never change"},
		{"ledger: rename", "", `UPDATE ledgers SET name = 'renamed' WHERE id = 1`, "ledgers never change"},
		{"ledger: delete", "", `DELETE FROM ledgers WHERE id = 1`, "ledgers are never deleted"},
		{"ledger: replace by row number", "", `INSERT OR REPLACE INTO ledgers (id, organization_id, name, created_at) VALUES (1, 2, 'stolen', '` + testTime + `')`, "ledgers are never replaced"},
		{"ledger: replace by name", "", `INSERT OR REPLACE INTO ledgers (id, organization_id, name, created_at) VALUES (9, 1, 'orders', '` + testTime + `')`, "ledgers are never replaced"},

		{"organization: change public ID", "", `UPDATE organizations SET public_id = 'org_zzzzzzzzzzzzzzzzzzzzzzzzzz' WHERE id = 1`, "never change"},
		{"organization: change row number", "", `UPDATE organizations SET id = 9 WHERE id = 1`, "never change"},
		{"organization: delete", "", `DELETE FROM organizations WHERE id = 1`, "never deleted"},
		{"organization: replace by row number", "", `INSERT OR REPLACE INTO organizations (id, public_id, name, created_at) VALUES (1, 'org_zzzzzzzzzzzzzzzzzzzzzzzzzz', 'Other', '` + testTime + `')`, "never replaced"},
		{"organization: replace by public ID", "", `INSERT OR REPLACE INTO organizations (id, public_id, name, created_at) VALUES (9, 'org_aaaaaaaaaaaaaaaaaaaaaaaaaa', 'Other', '` + testTime + `')`, "never replaced"},

		{"API key: move to another organization", "", `UPDATE api_keys SET organization_id = 2 WHERE id = 1`, "only an API key's revocation can change"},
		{"API key: change key hash", "", `UPDATE api_keys SET key_hash = '` + otherHash + `' WHERE id = 1`, "only an API key's revocation can change"},
		{"API key: extend expiry", "", `UPDATE api_keys SET expires_at = '2099-01-01T00:00:00.000Z' WHERE id = 1`, "only an API key's revocation can change"},
		{"API key: un-revoke", `UPDATE api_keys SET revoked_at = '` + testTime + `' WHERE id = 1`, `UPDATE api_keys SET revoked_at = NULL WHERE id = 1`, "stays revoked"},
		{"API key: change revocation time", `UPDATE api_keys SET revoked_at = '` + testTime + `' WHERE id = 1`, `UPDATE api_keys SET revoked_at = '2099-01-01T00:00:00.000Z' WHERE id = 1`, "stays revoked"},
		{"API key: delete", "", `DELETE FROM api_keys WHERE id = 1`, "never deleted"},
		{"API key: replace by row number", "", `INSERT OR REPLACE INTO api_keys (id, public_id, organization_id, name, key_hash, created_at, expires_at) VALUES (1, 'key_zzzzzzzzzzzzzzzzzzzzzzzzzz', 2, 'k', '` + otherHash + `', '` + testTime + `', '` + testTime + `')`, "never replaced"},
		{"API key: replace by key hash", "", `INSERT OR REPLACE INTO api_keys (id, public_id, organization_id, name, key_hash, created_at, expires_at) VALUES (9, 'key_zzzzzzzzzzzzzzzzzzzzzzzzzz', 2, 'k', '` + zeroHash + `', '` + testTime + `', '` + testTime + `')`, "never replaced"},

		{"API key scope: change", `INSERT INTO api_key_scopes (api_key_id, scope) VALUES (1, 'ledger:read')`, `UPDATE api_key_scopes SET scope = 'ledger:write' WHERE api_key_id = 1`, "scopes never change"},
		{"API key scope: delete", `INSERT INTO api_key_scopes (api_key_id, scope) VALUES (1, 'ledger:read')`, `DELETE FROM api_key_scopes WHERE api_key_id = 1`, "scopes never change"},
		{"API key scope: replace", `INSERT INTO api_key_scopes (api_key_id, scope) VALUES (1, 'ledger:read')`, `REPLACE INTO api_key_scopes (api_key_id, scope) VALUES (1, 'ledger:read')`, "scopes never change"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, path := openTestStore(t)
			seed(t, s)
			for _, stmt := range []string{record("INSERT", "2", "{}", "'k2'"), tt.setup} {
				if stmt == "" {
					continue
				}
				if _, err := s.write.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("setup: %v\n%s", err, stmt)
				}
			}
			before := dbSnapshot(t, s.read)

			// Run the statement on Stavewell's own connection, and on a plain
			// connection with SQLite's default settings (foreign keys off,
			// recursive triggers off), as a tool such as the sqlite3 shell
			// would open.
			plain, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer plain.Close()
			for name, db := range map[string]*sql.DB{"store": s.write, "plain": plain} {
				_, err := db.ExecContext(ctx, tt.stmt)
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("%s connection: err = %v, want an error containing %q", name, err, tt.want)
				}
			}
			if after := dbSnapshot(t, s.read); after != before {
				t.Errorf("database changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestSchemaRejectsInvalidRows(t *testing.T) {
	// Each statement breaks one rule in the schema. Values that are valid
	// are the same as in seed, so each statement fails for one reason only.
	record := func(seq, createdAt, data, hash, key string) string {
		return `INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash, idempotency_key)
			VALUES (1, ` + seq + `, '` + createdAt + `', ` + data + `, '` + zeroHash + `', '` + zeroHash + `', '` + hash + `', ` + key + `)`
	}
	tests := []struct {
		name string
		stmt string
	}{
		{"record: text in an integer column", record("'two'", testTime, "'{}'", zeroHash, "NULL")},
		{"record: sequence 0", record("0", testTime, "'{}'", zeroHash, "NULL")},
		{"record: duplicate sequence", record("1", testTime, "'{}'", zeroHash, "NULL")},
		{"record: unknown ledger", strings.Replace(record("2", testTime, "'{}'", zeroHash, "NULL"), "VALUES (1,", "VALUES (99,", 1)},
		{"record: time without milliseconds", record("2", "2026-11-06T19:22:31Z", "'{}'", zeroHash, "NULL")},
		{"record: time with a space instead of T", record("2", "2026-11-06 19:22:31.123Z", "'{}'", zeroHash, "NULL")},
		{"record: time without a zone", record("2", "2026-11-06T19:22:31.123", "'{}'", zeroHash, "NULL")},
		{"record: impossible time", record("2", "2026-99-99T99:99:99.999Z", "'{}'", zeroHash, "NULL")},
		{"record: February 30", record("2", "2026-02-30T00:00:00.000Z", "'{}'", zeroHash, "NULL")},
		{"record: data that isn't JSON", record("2", testTime, "'{not json'", zeroHash, "NULL")},
		{"record: short hash", record("2", testTime, "'{}'", "abc", "NULL")},
		{"record: uppercase hash", record("2", testTime, "'{}'", strings.Repeat("A", 64), "NULL")},
		{"record: idempotency key with a space", record("2", testTime, "'{}'", zeroHash, "'a b'")},
		{"record: idempotency key too long", record("2", testTime, "'{}'", zeroHash, "'"+strings.Repeat("k", 129)+"'")},
		{"ledger: duplicate name in an organization", `INSERT INTO ledgers (organization_id, name, created_at) VALUES (1, 'orders', '` + testTime + `')`},
		{"ledger: name with an uppercase letter", `INSERT INTO ledgers (organization_id, name, created_at) VALUES (1, 'Orders', '` + testTime + `')`},
		{"ledger: name starting with a digit", `INSERT INTO ledgers (organization_id, name, created_at) VALUES (1, '1orders', '` + testTime + `')`},
		{"ledger: name with an underscore", `INSERT INTO ledgers (organization_id, name, created_at) VALUES (1, 'my_orders', '` + testTime + `')`},
		{"ledger: name too long", `INSERT INTO ledgers (organization_id, name, created_at) VALUES (1, 'a` + strings.Repeat("b", 64) + `', '` + testTime + `')`},
		{"organization: malformed public ID", `INSERT INTO organizations (public_id, name, created_at) VALUES ('org_short', 'x', '` + testTime + `')`},
		{"organization: public ID with other characters", `INSERT INTO organizations (public_id, name, created_at) VALUES ('org_ !!!!!!!!!!!!!!!!!!!!!!!!!', 'x', '` + testTime + `')`},
		{"organization: public ID with uppercase letters", `INSERT INTO organizations (public_id, name, created_at) VALUES ('org_CCCCCCCCCCCCCCCCCCCCCCCCCC', 'x', '` + testTime + `')`},
		{"organization: public ID with digits outside base32", `INSERT INTO organizations (public_id, name, created_at) VALUES ('org_cccccccccccccccccccccccc18', 'x', '` + testTime + `')`},
		{"API key: public ID with other characters", `INSERT INTO api_keys (public_id, organization_id, name, key_hash, created_at, expires_at) VALUES ('key_ !!!!!!!!!!!!!!!!!!!!!!!!!', 1, 'k', '` + strings.Repeat("1", 64) + `', '` + testTime + `', '` + testTime + `')`},
		{"organization: empty name", `INSERT INTO organizations (public_id, name, created_at) VALUES ('org_cccccccccccccccccccccccccc', '', '` + testTime + `')`},
		{"organization: negative stored data", `UPDATE organizations SET stored_data_bytes = -1 WHERE id = 1`},
		{"API key: duplicate key hash", `INSERT INTO api_keys (public_id, organization_id, name, key_hash, created_at, expires_at) VALUES ('key_bbbbbbbbbbbbbbbbbbbbbbbbbb', 1, 'k', '` + zeroHash + `', '` + testTime + `', '` + testTime + `')`},
		{"API key: unknown organization", `INSERT INTO api_keys (public_id, organization_id, name, key_hash, created_at, expires_at) VALUES ('key_bbbbbbbbbbbbbbbbbbbbbbbbbb', 99, 'k', '` + strings.Repeat("1", 64) + `', '` + testTime + `', '` + testTime + `')`},
		{"API key scope: unknown scope", `INSERT INTO api_key_scopes (api_key_id, scope) VALUES (1, 'ledger:admin')`},
		{"daily usage: malformed day", `INSERT INTO daily_usage (organization_id, day) VALUES (1, '2026-11-6')`},
		{"daily usage: impossible day", `INSERT INTO daily_usage (organization_id, day) VALUES (1, '2026-13-01')`},
		{"organization: row number 0", `INSERT INTO organizations (id, public_id, name, created_at) VALUES (0, 'org_cccccccccccccccccccccccccc', 'x', '` + testTime + `')`},
		{"organization: row number -1", `INSERT INTO organizations (id, public_id, name, created_at) VALUES (-1, 'org_cccccccccccccccccccccccccc', 'x', '` + testTime + `')`},
		{"API key: row number -1", `INSERT INTO api_keys (id, public_id, organization_id, name, key_hash, created_at, expires_at) VALUES (-1, 'key_bbbbbbbbbbbbbbbbbbbbbbbbbb', 1, 'k', '` + strings.Repeat("1", 64) + `', '` + testTime + `', '` + testTime + `')`},
		{"ledger: row number -1", `INSERT INTO ledgers (id, organization_id, name, created_at) VALUES (-1, 1, 'other', '` + testTime + `')`},
		{"daily usage: negative count", `INSERT INTO daily_usage (organization_id, day, records_appended) VALUES (1, '2026-11-06', -1)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			seed(t, s)
			if _, err := s.write.ExecContext(context.Background(), tt.stmt); err == nil {
				t.Errorf("statement succeeded, want an error:\n%s", tt.stmt)
			}
		})
	}
}

func TestSchemaAcceptsValidRows(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	seed(t, s)
	for _, stmt := range []string{
		// Any number of records without an idempotency key.
		`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash) VALUES (1, 2, '` + testTime + `', '{}', '` + zeroHash + `', '` + zeroHash + `', '` + zeroHash + `')`,
		`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash) VALUES (1, 3, '` + testTime + `', '{}', '` + zeroHash + `', '` + zeroHash + `', '` + zeroHash + `')`,
		// The same idempotency key in two different ledgers.
		`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash, idempotency_key) VALUES (1, 4, '` + testTime + `', '{}', '` + zeroHash + `', '` + zeroHash + `', '` + zeroHash + `', 'order-1042_shipped')`,
		`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash, idempotency_key) VALUES (2, 1, '` + testTime + `', '{}', '` + zeroHash + `', '` + zeroHash + `', '` + zeroHash + `', 'order-1042_shipped')`,
		// A ledger name with digits and hyphens, at the maximum length.
		`INSERT INTO ledgers (organization_id, name, created_at) VALUES (1, 'a-1` + strings.Repeat("b", 61) + `', '` + testTime + `')`,
		// Both scopes on one key.
		`INSERT INTO api_key_scopes (api_key_id, scope) VALUES (1, 'ledger:read'), (1, 'ledger:write')`,
		// Revoking a key.
		`UPDATE api_keys SET revoked_at = '` + testTime + `' WHERE id = 1`,
		// Counters and an organization's display name can change.
		`INSERT INTO daily_usage (organization_id, day, records_appended) VALUES (1, '2026-11-06', 1)`,
		`UPDATE daily_usage SET records_appended = records_appended + 1 WHERE organization_id = 1 AND day = '2026-11-06'`,
		`UPDATE organizations SET stored_data_bytes = stored_data_bytes + 2 WHERE id = 1`,
		`UPDATE organizations SET name = 'Renamed' WHERE id = 1`,
		// A leap day, and the last millisecond of a day.
		`INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash) VALUES (1, 5, '2028-02-29T23:59:59.999Z', '{}', '` + zeroHash + `', '` + zeroHash + `', '` + zeroHash + `')`,
		// Public IDs exactly as the ID generator makes them.
		`INSERT INTO organizations (id, public_id, name, created_at) VALUES (3, '` + ids.New("org") + `', 'Generated', '` + testTime + `')`,
		`INSERT INTO api_keys (public_id, organization_id, name, key_hash, created_at, expires_at) VALUES ('` + ids.New("key") + `', 3, 'k', '` + strings.Repeat("2", 64) + `', '` + testTime + `', '` + testTime + `')`,
	} {
		if _, err := s.write.ExecContext(ctx, stmt); err != nil {
			t.Errorf("statement failed: %v\n%s", err, stmt)
		}
	}
}

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].version != 1 || ms[0].name != "0001_initial.sql" {
		t.Errorf("migrations = %v, want 0001_initial.sql first", ms)
	}
}

func TestFormatTime(t *testing.T) {
	s, _ := openTestStore(t)
	// FormatTime's output must pass the schema's time format check.
	stmt := `INSERT INTO organizations (public_id, name, created_at) VALUES ('org_cccccccccccccccccccccccccc', 'x', ?)`
	got := FormatTime(mustParse(t, "2026-11-06T14:22:31.1234567-05:00"))
	if got != "2026-11-06T19:22:31.123Z" {
		t.Errorf("FormatTime() = %q, want 2026-11-06T19:22:31.123Z", got)
	}
	if _, err := s.write.ExecContext(context.Background(), stmt, got); err != nil {
		t.Errorf("the schema rejects FormatTime's output %q: %v", got, err)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}
