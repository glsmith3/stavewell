package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/glsmith3/stavewell/internal/record"
)

func mustData(t *testing.T, s string) record.Data {
	t.Helper()
	d, err := record.ParseData([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// checkChain recomputes both hashes of each record and checks that each
// record links to the one before it, as chain verification does.
func checkChain(t *testing.T, records []Record) {
	t.Helper()
	previous := record.ZeroHash
	for i, r := range records {
		if r.Sequence != int64(i+1) {
			t.Errorf("record %d has sequence number %d", i+1, r.Sequence)
		}
		sum := sha256.Sum256([]byte(r.Data))
		if r.DataHash != hex.EncodeToString(sum[:]) {
			t.Errorf("record %d: data_hash doesn't match its data", r.Sequence)
		}
		if r.PreviousHash != previous {
			t.Errorf("record %d: previous_hash = %s, want %s", r.Sequence, r.PreviousHash, previous)
		}
		want, err := record.Hash(record.Fields{Ledger: r.Ledger, Sequence: r.Sequence, CreatedAt: FormatTime(r.CreatedAt), DataHash: r.DataHash, PreviousHash: r.PreviousHash})
		if err != nil || r.Hash != want {
			t.Errorf("record %d: hash = %s, want %s (%v)", r.Sequence, r.Hash, want, err)
		}
		previous = r.Hash
	}
}

// allRecords returns every record in the ledger.
func allRecords(t *testing.T, s *Store, org int64, ledger string) []Record {
	t.Helper()
	var all []Record
	var after int64
	for {
		page, more, err := s.ListRecords(context.Background(), org, ledger, after, 100)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if !more {
			return all
		}
		after = page[len(page)-1].Sequence
	}
}

func TestAppendRecord(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
		t.Fatal(err)
	}

	var appended []Record
	for i, data := range []string{`{"b": 1, "a": "x"}`, `{}`, `{"n": [1, 2, 3]}`} {
		r, err := s.AppendRecord(ctx, org, "orders", mustData(t, data), testNow.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		appended = append(appended, r)
	}
	if appended[0].Data != `{"a":"x","b":1}` {
		t.Errorf("stored data = %s, want the canonical form", appended[0].Data)
	}
	if !appended[0].CreatedAt.Equal(testNow.Truncate(time.Millisecond)) {
		t.Errorf("created_at = %v, want %v", appended[0].CreatedAt, testNow.Truncate(time.Millisecond))
	}
	checkChain(t, appended)

	// Reading the records back gives the same records.
	got := allRecords(t, s, org, "orders")
	if fmt.Sprint(got) != fmt.Sprint(appended) {
		t.Errorf("listed %+v, want %+v", got, appended)
	}
	for _, want := range appended {
		r, err := s.GetRecord(ctx, org, "orders", want.Sequence)
		if err != nil || r != want {
			t.Errorf("GetRecord(%d) = %+v, %v; want %+v", want.Sequence, r, err, want)
		}
	}

	// The ledger reports its latest record.
	l, err := s.GetLedger(ctx, org, "orders")
	if err != nil || l.LatestSequence != 3 || l.LatestHash != appended[2].Hash {
		t.Errorf("GetLedger() = %+v, %v; want latest record 3 with hash %s", l, err, appended[2].Hash)
	}
}

func TestEmptyLedgerHasNoLatestRecord(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "empty", testNow); err != nil {
		t.Fatal(err)
	}
	l, err := s.GetLedger(ctx, org, "empty")
	if err != nil || l.LatestSequence != 0 || l.LatestHash != "" {
		t.Errorf("GetLedger() = %+v, %v; want no latest record", l, err)
	}
}

func TestListLedgersIncludesLatestRecord(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	for _, name := range []string{"a", "b", "c"} {
		if _, err := s.CreateLedger(ctx, org, name, testNow); err != nil {
			t.Fatal(err)
		}
	}
	// Ledger a gets two records and ledger c one; b stays empty.
	var latest = map[string]Record{}
	for _, name := range []string{"a", "c", "a"} {
		r, err := s.AppendRecord(ctx, org, name, mustData(t, `{}`), testNow)
		if err != nil {
			t.Fatal(err)
		}
		latest[name] = r
	}
	ledgers, _, err := s.ListLedgers(ctx, org, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ledgers {
		want := latest[l.Name] // the zero Record for b
		if l.LatestSequence != want.Sequence || l.LatestHash != want.Hash {
			t.Errorf("ledger %s: latest = %d %q, want %d %q", l.Name, l.LatestSequence, l.LatestHash, want.Sequence, want.Hash)
		}
	}
}

func TestAppendRecordTimesNeverGoBackward(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
		t.Fatal(err)
	}
	later := testNow.Add(time.Hour)
	first, err := s.AppendRecord(ctx, org, "orders", mustData(t, `{}`), later)
	if err != nil {
		t.Fatal(err)
	}
	// The clock goes back an hour.
	second, err := s.AppendRecord(ctx, org, "orders", mustData(t, `{}`), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("second record's created_at = %v, want the first's, %v", second.CreatedAt, first.CreatedAt)
	}
	checkChain(t, allRecords(t, s, org, "orders"))
}

// usage returns the organization's stored data total and its records
// appended on each day.
func usage(t *testing.T, s *Store, org int64) (stored int64, days map[string]int64) {
	t.Helper()
	if err := s.read.QueryRow("SELECT stored_data_bytes FROM organizations WHERE id = ?", org).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	rows, err := s.read.Query("SELECT day, records_appended FROM daily_usage WHERE organization_id = ?", org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	days = map[string]int64{}
	for rows.Next() {
		var day string
		var n int64
		if err := rows.Scan(&day, &n); err != nil {
			t.Fatal(err)
		}
		days[day] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return stored, days
}

func TestAppendRecordCountsUsage(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	orgs := newOrgs(t, s, 2)
	for _, org := range orgs {
		if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
			t.Fatal(err)
		}
	}
	day1 := time.Date(2026, 11, 6, 23, 59, 59, 999e6, time.UTC)
	day2 := day1.Add(time.Millisecond)
	for _, c := range []struct {
		data string
		now  time.Time
	}{
		{`{"a": 1}`, day1},      // canonical size 7
		{`{ "bb" : 22 }`, day1}, // canonical size 9
		{`{}`, day2},            // canonical size 2
	} {
		if _, err := s.AppendRecord(ctx, orgs[0], "orders", mustData(t, c.data), c.now); err != nil {
			t.Fatal(err)
		}
	}
	stored, days := usage(t, s, orgs[0])
	if stored != 7+9+2 {
		t.Errorf("stored data = %d bytes, want %d", stored, 7+9+2)
	}
	if fmt.Sprint(days) != "map[2026-11-06:2 2026-11-07:1]" {
		t.Errorf("records appended by day = %v", days)
	}

	// A failed append counts nothing.
	if _, err := s.AppendRecord(ctx, orgs[0], "missing", mustData(t, `{"a": 1}`), day2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("appending to a missing ledger: err = %v, want ErrNotFound", err)
	}
	if again, againDays := usage(t, s, orgs[0]); again != stored || fmt.Sprint(againDays) != fmt.Sprint(days) {
		t.Errorf("after a failed append, usage = %d %v; want %d %v", again, againDays, stored, days)
	}
	// The other organization's usage is separate.
	if stored, days := usage(t, s, orgs[1]); stored != 0 || len(days) != 0 {
		t.Errorf("other organization's usage = %d %v; want none", stored, days)
	}
}

func TestRecordsAreIsolated(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	orgs := newOrgs(t, s, 2)
	if _, err := s.CreateLedger(ctx, orgs[0], "orders", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendRecord(ctx, orgs[0], "orders", mustData(t, `{"owner": 0}`), testNow); err != nil {
		t.Fatal(err)
	}

	// The other organization can't reach the ledger at all.
	if _, err := s.AppendRecord(ctx, orgs[1], "orders", mustData(t, `{}`), testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("AppendRecord from another organization: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetRecord(ctx, orgs[1], "orders", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRecord from another organization: err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.ListRecords(ctx, orgs[1], "orders", 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListRecords from another organization: err = %v, want ErrNotFound", err)
	}

	// A ledger with the same name in the other organization is separate.
	if _, err := s.CreateLedger(ctx, orgs[1], "orders", testNow); err != nil {
		t.Fatal(err)
	}
	r, err := s.AppendRecord(ctx, orgs[1], "orders", mustData(t, `{"owner": 1}`), testNow)
	if err != nil || r.Sequence != 1 || r.PreviousHash != record.ZeroHash {
		t.Errorf("first record in the other organization's ledger = %+v, %v; want record 1", r, err)
	}
	for i, org := range orgs {
		records := allRecords(t, s, org, "orders")
		if len(records) != 1 || records[0].Data != fmt.Sprintf(`{"owner":%d}`, i) {
			t.Errorf("organization %d's records = %+v; want only its own", i, records)
		}
	}
}

// TestAppendRecordUnderConcurrency checks that appends sent at the same time
// get consecutive sequence numbers and form one chain. It depends on each
// append's read of the latest record and its insert running in one write
// transaction that no other write can interleave with.
func TestAppendRecordUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
		t.Fatal(err)
	}
	const n = 100
	data := make([]record.Data, n)
	for i := range n {
		data[i] = mustData(t, fmt.Sprintf(`{"i": %d}`, i))
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = s.AppendRecord(ctx, org, "orders", data[i], testNow)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("append %d: %v", i, err)
		}
	}
	records := allRecords(t, s, org, "orders")
	if len(records) != n {
		t.Fatalf("the ledger has %d records, want %d", len(records), n)
	}
	checkChain(t, records)
	if _, days := usage(t, s, org); days[testNow.UTC().Format(time.DateOnly)] != n {
		t.Errorf("records appended = %v, want %d today", days, n)
	}
}

func TestListRecords(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
		t.Fatal(err)
	}

	// An empty ledger has an empty list.
	records, more, err := s.ListRecords(ctx, org, "orders", 0, 100)
	if err != nil || len(records) != 0 || more {
		t.Errorf("empty ledger: %v, %v, %v; want nothing", records, more, err)
	}

	for i := range 5 {
		if _, err := s.AppendRecord(ctx, org, "orders", mustData(t, fmt.Sprintf(`{"i": %d}`, i)), testNow); err != nil {
			t.Fatal(err)
		}
	}
	var got []int64
	var after int64
	for page := 0; ; page++ {
		records, more, err := s.ListRecords(ctx, org, "orders", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) > 2 {
			t.Fatalf("page %d has %d records, want at most 2", page, len(records))
		}
		for _, r := range records {
			got = append(got, r.Sequence)
		}
		if wantMore := len(got) < 5; more != wantMore {
			t.Errorf("page %d: more = %v, want %v", page, more, wantMore)
		}
		if !more {
			break
		}
		after = records[len(records)-1].Sequence
	}
	if fmt.Sprint(got) != "[1 2 3 4 5]" {
		t.Errorf("listed %v, want [1 2 3 4 5]", got)
	}

	// Listing after the last record returns nothing, until one is appended.
	records, more, err = s.ListRecords(ctx, org, "orders", 5, 100)
	if err != nil || len(records) != 0 || more {
		t.Errorf("after the last record: %v, %v, %v; want nothing", records, more, err)
	}
	if _, err := s.AppendRecord(ctx, org, "orders", mustData(t, `{}`), testNow); err != nil {
		t.Fatal(err)
	}
	records, _, err = s.ListRecords(ctx, org, "orders", 5, 100)
	if err != nil || len(records) != 1 || records[0].Sequence != 6 {
		t.Errorf("after the last record, once one is appended: %v, %v; want [6]", records, err)
	}

	// A position past the end, or before the start, isn't one the list gave.
	for _, after := range []int64{7, -1} {
		if _, _, err := s.ListRecords(ctx, org, "orders", after, 100); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("listing after %d: err = %v, want ErrRecordNotFound", after, err)
		}
	}
	if _, _, err := s.ListRecords(ctx, org, "missing", 0, 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("listing a missing ledger: err = %v, want ErrNotFound", err)
	}
}

func TestGetRecordErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendRecord(ctx, org, "orders", mustData(t, `{}`), testNow); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{0, -1, 2} {
		if _, err := s.GetRecord(ctx, org, "orders", seq); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("GetRecord(%d): err = %v, want ErrRecordNotFound", seq, err)
		}
	}
	if _, err := s.GetRecord(ctx, org, "missing", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRecord on a missing ledger: err = %v, want ErrNotFound", err)
	}
}

func TestAppendRecordRejectsZeroData(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	if _, err := s.CreateLedger(ctx, org, "orders", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendRecord(ctx, org, "orders", record.Data{}, testNow); err == nil {
		t.Error("appending the zero Data succeeded")
	}
	if records := allRecords(t, s, org, "orders"); len(records) != 0 {
		t.Errorf("the ledger has %d records, want 0", len(records))
	}
}
