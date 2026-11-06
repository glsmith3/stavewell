package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/glsmith3/stavewell/internal/record"
)

// chainLedger creates an organization with a ledger named "orders" holding
// n records, and returns the organization's row number and the records.
func chainLedger(t *testing.T, s *Store, n int) (int64, []Record) {
	t.Helper()
	ctx := context.Background()
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
		t.Fatal(err)
	}
	var records []Record
	for i := range n {
		r, _, err := s.AppendRecord(ctx, org, "orders", mustData(t, fmt.Sprintf(`{"i": %d}`, i)), "", testNow.Add(time.Duration(i)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return org, records
}

func verify(t *testing.T, s *Store, org int64, from int64, limit int) ChainResult {
	t.Helper()
	res, err := s.VerifyChain(context.Background(), org, "orders", from, limit)
	if err != nil {
		t.Fatalf("VerifyChain(from %d, limit %d): %v", from, limit, err)
	}
	return res
}

func TestVerifyChain(t *testing.T) {
	s, _ := openTestStore(t)
	org, records := chainLedger(t, s, 5)

	if got, want := verify(t, s, org, 1, 100), (ChainResult{Intact: true, First: 1, Last: 5, LastHash: records[4].Hash, ReachedLatest: true}); got != want {
		t.Errorf("whole ledger = %+v, want %+v", got, want)
	}
	// In pieces of 2, continuing from the last record checked plus 1.
	for _, c := range []struct {
		from int64
		want ChainResult
	}{
		{1, ChainResult{Intact: true, First: 1, Last: 2, LastHash: records[1].Hash}},
		{3, ChainResult{Intact: true, First: 3, Last: 4, LastHash: records[3].Hash}},
		{5, ChainResult{Intact: true, First: 5, Last: 5, LastHash: records[4].Hash, ReachedLatest: true}},
		{6, ChainResult{Intact: true, ReachedLatest: true}},
	} {
		if got := verify(t, s, org, c.from, 2); got != c.want {
			t.Errorf("from %d = %+v, want %+v", c.from, got, c.want)
		}
	}
	ctx := context.Background()
	if _, err := s.VerifyChain(ctx, org, "orders", 7, 100); !errors.Is(err, ErrPastEnd) {
		t.Errorf("from 7: err = %v, want ErrPastEnd", err)
	}
	if _, err := s.VerifyChain(ctx, org, "orders", 0, 100); err == nil {
		t.Error("from 0 succeeded")
	}
	if _, err := s.VerifyChain(ctx, org, "missing", 1, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing ledger: err = %v, want ErrNotFound", err)
	}
	// Another organization can't verify the ledger.
	other := newOrgs(t, s, 1)[0]
	if _, err := s.VerifyChain(ctx, other, "orders", 1, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("another organization: err = %v, want ErrNotFound", err)
	}
}

func TestVerifyChainEmptyLedger(t *testing.T) {
	s, _ := openTestStore(t)
	org, _ := chainLedger(t, s, 0)
	if got, want := verify(t, s, org, 1, 100), (ChainResult{Intact: true, ReachedLatest: true}); got != want {
		t.Errorf("empty ledger = %+v, want %+v", got, want)
	}
	if _, err := s.VerifyChain(context.Background(), org, "orders", 2, 100); !errors.Is(err, ErrPastEnd) {
		t.Errorf("from 2 on an empty ledger: err = %v, want ErrPastEnd", err)
	}
}

// tamper runs SQL on the database with the append-only triggers removed, as
// someone editing the database file could.
func tamper(t *testing.T, s *Store, statements ...string) {
	t.Helper()
	for _, q := range append([]string{"DROP TRIGGER IF EXISTS records_no_update", "DROP TRIGGER IF EXISTS records_no_delete"}, statements...) {
		if _, err := s.write.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rehash recomputes the hashes of records from sequence from onward, in the
// database, as someone with full control of the server could.
func rehash(t *testing.T, s *Store, org int64, from int64) {
	t.Helper()
	previous := record.ZeroHash
	if from > 1 {
		if err := s.read.QueryRow("SELECT hash FROM records WHERE sequence = ?", from-1).Scan(&previous); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.read.Query("SELECT sequence, created_at, data FROM records WHERE sequence >= ? ORDER BY sequence", from)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		seq           int64
		created, data string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.created, &r.data); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		d := mustData(t, r.data)
		h, err := record.Hash(record.Fields{Ledger: "orders", Sequence: r.seq, CreatedAt: r.created, DataHash: d.Hash(), PreviousHash: previous})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.write.Exec("UPDATE records SET data_hash = ?, previous_hash = ?, hash = ? WHERE sequence = ?", d.Hash(), previous, h, r.seq); err != nil {
			t.Fatal(err)
		}
		previous = h
	}
}

func TestVerifyChainFindsTampering(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, s *Store, org int64)
		want   ChainResult
	}{
		{"data changed", func(t *testing.T, s *Store, org int64) {
			tamper(t, s, `UPDATE records SET data = '{"i":99}' WHERE sequence = 3`)
		}, ChainResult{First: 1, Last: 3, Broken: 3}},
		{"data_hash changed", func(t *testing.T, s *Store, org int64) {
			tamper(t, s, `UPDATE records SET data_hash = '`+record.ZeroHash+`' WHERE sequence = 3`)
		}, ChainResult{First: 1, Last: 3, Broken: 3}},
		{"created_at changed", func(t *testing.T, s *Store, org int64) {
			tamper(t, s, `UPDATE records SET created_at = '2030-01-01T00:00:00.000Z' WHERE sequence = 2`)
		}, ChainResult{First: 1, Last: 2, Broken: 2}},
		{"first record changed", func(t *testing.T, s *Store, org int64) {
			tamper(t, s, `UPDATE records SET data = '{"i":99}' WHERE sequence = 1`)
		}, ChainResult{First: 1, Last: 1, Broken: 1}},
		{"latest record changed", func(t *testing.T, s *Store, org int64) {
			tamper(t, s, `UPDATE records SET data = '{"i":99}' WHERE sequence = 5`)
		}, ChainResult{First: 1, Last: 5, Broken: 5, ReachedLatest: true}},
		{"data and its hashes changed, later hashes not", func(t *testing.T, s *Store, org int64) {
			tamper(t, s, `UPDATE records SET data = '{"i":99}' WHERE sequence = 3`)
			// Recompute record 3's hashes only. Record 4 still links to the
			// old hash of record 3.
			tamper(t, s, "CREATE TEMP TABLE keep AS SELECT sequence, data_hash, previous_hash, hash FROM records WHERE sequence > 3")
			rehash(t, s, org, 3)
			tamper(t, s, "UPDATE records SET data_hash = (SELECT data_hash FROM keep WHERE keep.sequence = records.sequence), previous_hash = (SELECT previous_hash FROM keep WHERE keep.sequence = records.sequence), hash = (SELECT hash FROM keep WHERE keep.sequence = records.sequence) WHERE sequence > 3")
		}, ChainResult{First: 1, Last: 4, Broken: 4}},
		{"record deleted", func(t *testing.T, s *Store, org int64) {
			tamper(t, s, "DELETE FROM records WHERE sequence = 3")
		}, ChainResult{First: 1, Last: 4, Broken: 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			org, _ := chainLedger(t, s, 5)
			c.change(t, s, org)
			if got := verify(t, s, org, 1, 100); got != c.want {
				t.Errorf("after the change: %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestVerifyChainLimits checks what chain verification can't catch, as the
// docs say: a change followed by recomputing every later hash, and records
// removed from the end.
func TestVerifyChainLimits(t *testing.T) {
	t.Run("every later hash recomputed", func(t *testing.T) {
		s, _ := openTestStore(t)
		org, records := chainLedger(t, s, 5)
		tamper(t, s, `UPDATE records SET data = '{"i":99}' WHERE sequence = 3`)
		rehash(t, s, org, 3)
		res := verify(t, s, org, 1, 100)
		if !res.Intact {
			t.Fatalf("result = %+v, want intact", res)
		}
		// But the latest hash no longer matches one saved earlier.
		if res.LastHash == records[4].Hash {
			t.Error("the latest hash didn't change")
		}
	})
	t.Run("latest records removed", func(t *testing.T) {
		s, _ := openTestStore(t)
		org, records := chainLedger(t, s, 5)
		tamper(t, s, "DELETE FROM records WHERE sequence >= 4")
		if got, want := verify(t, s, org, 1, 100), (ChainResult{Intact: true, First: 1, Last: 3, LastHash: records[2].Hash, ReachedLatest: true}); got != want {
			t.Errorf("result = %+v, want %+v", got, want)
		}
	})
}

func TestVerifyChainFromChecksTheLink(t *testing.T) {
	s, _ := openTestStore(t)
	org, _ := chainLedger(t, s, 5)
	// Change record 3's stored hash. Checking from 4 must notice that
	// record 4 no longer links to it, even though record 3 isn't checked.
	tamper(t, s, `UPDATE records SET hash = '`+record.ZeroHash+`' WHERE sequence = 3`)
	if got, want := verify(t, s, org, 4, 100), (ChainResult{First: 4, Last: 4, Broken: 4}); got != want {
		t.Errorf("from 4 = %+v, want %+v", got, want)
	}
	// And with record 3 gone, record 4 can't link to anything.
	tamper(t, s, "DELETE FROM records WHERE sequence = 3")
	if got, want := verify(t, s, org, 4, 100), (ChainResult{First: 4, Last: 4, Broken: 4}); got != want {
		t.Errorf("from 4 with record 3 missing = %+v, want %+v", got, want)
	}
	// Records missing from the start of the range on.
	tamper(t, s, "DELETE FROM records WHERE sequence IN (4, 5)")
	tamper(t, s, `INSERT INTO records (ledger_id, sequence, created_at, data, data_hash, previous_hash, hash) SELECT ledger_id, 9, created_at, data, data_hash, previous_hash, hash FROM records WHERE sequence = 2`)
	// Record 9 is now the latest record.
	if got, want := verify(t, s, org, 4, 100), (ChainResult{First: 9, Last: 9, Broken: 9, ReachedLatest: true}); got != want {
		t.Errorf("from 4 with records 4 to 8 missing = %+v, want %+v", got, want)
	}
}

// TestVerifyChainWhileAppending checks that chain verification sees a
// consistent ledger while records are being appended.
func TestVerifyChainWhileAppending(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org, _ := chainLedger(t, s, 1)
	data := mustData(t, `{"x": 1}`)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			if _, _, err := s.AppendRecord(ctx, org, "orders", data, "", testNow); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 50 {
		res, err := s.VerifyChain(ctx, org, "orders", 1, 10_000)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Intact || !res.ReachedLatest || res.First != 1 {
			t.Errorf("while appending: %+v, want intact to the latest record", res)
		}
	}
	wg.Wait()
}
