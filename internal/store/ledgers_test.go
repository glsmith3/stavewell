package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// newOrgs creates n organizations and returns their internal row numbers.
func newOrgs(t *testing.T, s *Store, n int) []int64 {
	t.Helper()
	var rowIDs []int64
	for i := range n {
		org, err := s.CreateOrganization(context.Background(), fmt.Sprintf("Org %d", i), testNow)
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := s.read.QueryRow("SELECT id FROM organizations WHERE public_id = ?", org.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		rowIDs = append(rowIDs, id)
	}
	return rowIDs
}

func TestValidLedgerName(t *testing.T) {
	for _, name := range []string{"a", "orders", "a-1", "x" + strings.Repeat("9", 63)} {
		if !ValidLedgerName(name) {
			t.Errorf("ValidLedgerName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "Orders", "1orders", "-orders", "my_orders", "my orders", "orders\n", "x" + strings.Repeat("9", 64), "ordérs"} {
		if ValidLedgerName(name) {
			t.Errorf("ValidLedgerName(%q) = true, want false", name)
		}
	}
}

func TestCreateAndGetLedger(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	orgs := newOrgs(t, s, 2)

	l, err := s.CreateLedger(ctx, orgs[0], "orders", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if l.Name != "orders" || !l.CreatedAt.Equal(testNow.Truncate(time.Millisecond)) {
		t.Errorf("CreateLedger() = %+v", l)
	}
	got, err := s.GetLedger(ctx, orgs[0], "orders")
	if err != nil || got != l {
		t.Errorf("GetLedger() = %+v, %v; want %+v", got, err, l)
	}

	if _, err := s.CreateLedger(ctx, orgs[0], "orders", testNow); !errors.Is(err, ErrLedgerExists) {
		t.Errorf("creating a duplicate: err = %v, want ErrLedgerExists", err)
	}
	if _, err := s.CreateLedger(ctx, orgs[0], "Bad Name", testNow); err == nil {
		t.Error("creating a ledger with an invalid name succeeded")
	}

	// Another organization can't see the ledger, and can use the same name.
	if _, err := s.GetLedger(ctx, orgs[1], "orders"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another organization's GetLedger: err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateLedger(ctx, orgs[1], "orders", testNow); err != nil {
		t.Errorf("another organization creating the same name: %v", err)
	}
	if _, err := s.GetLedger(ctx, orgs[0], "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetLedger for a missing ledger: err = %v, want ErrNotFound", err)
	}
}

func TestLedgerLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	orgs := newOrgs(t, s, 2)
	for i := range MaxLedgers {
		if _, err := s.CreateLedger(ctx, orgs[0], fmt.Sprintf("l%d", i), testNow); err != nil {
			t.Fatalf("ledger %d: %v", i, err)
		}
	}
	if _, err := s.CreateLedger(ctx, orgs[0], "one-more", testNow); !errors.Is(err, ErrLedgerLimit) {
		t.Errorf("ledger %d: err = %v, want ErrLedgerLimit", MaxLedgers+1, err)
	}
	// The limit is per organization.
	if _, err := s.CreateLedger(ctx, orgs[1], "one-more", testNow); err != nil {
		t.Errorf("another organization: %v", err)
	}
}

// TestLedgerLimitUnderConcurrency checks that the limit holds when many
// requests create ledgers at once. It depends on write transactions taking
// the write lock when they begin.
func TestLedgerLimitUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org := newOrgs(t, s, 1)[0]
	const attempts = MaxLedgers + 50
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			_, errs[i] = s.CreateLedger(ctx, org, fmt.Sprintf("l%d", i), testNow)
		})
	}
	wg.Wait()
	created, limited := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrLedgerLimit):
			limited++
		default:
			t.Errorf("attempt %d: %v", i, err)
		}
	}
	if created != MaxLedgers || limited != attempts-MaxLedgers {
		t.Errorf("%d created and %d refused; want %d and %d", created, limited, MaxLedgers, attempts-MaxLedgers)
	}
	var count int
	if err := s.read.QueryRow("SELECT count(*) FROM ledgers WHERE organization_id = ?", org).Scan(&count); err != nil || count != MaxLedgers {
		t.Errorf("the organization has %d ledgers, %v; want %d", count, err, MaxLedgers)
	}
}

func TestListLedgers(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	orgs := newOrgs(t, s, 2)

	// Names deliberately out of alphabetical order: the list is in creation
	// order.
	names := []string{"zeta", "alpha", "mu", "beta", "omega"}
	for i, name := range names {
		if _, err := s.CreateLedger(ctx, orgs[0], name, testNow.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateLedger(ctx, orgs[1], "other-org", testNow); err != nil {
		t.Fatal(err)
	}

	var got []string
	after := ""
	for page := 0; ; page++ {
		ledgers, more, err := s.ListLedgers(ctx, orgs[0], after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(ledgers) > 2 {
			t.Fatalf("page %d has %d ledgers, want at most 2", page, len(ledgers))
		}
		for _, l := range ledgers {
			got = append(got, l.Name)
		}
		if wantMore := len(got) < len(names); more != wantMore {
			t.Errorf("page %d: more = %v, want %v", page, more, wantMore)
		}
		if !more {
			break
		}
		after = ledgers[len(ledgers)-1].Name
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("listed %v, want %v", got, names)
	}

	// Listing from the last ledger returns nothing, until a ledger is added.
	ledgers, more, err := s.ListLedgers(ctx, orgs[0], "omega", 100)
	if err != nil || len(ledgers) != 0 || more {
		t.Errorf("after the last ledger = %v, %v, %v; want nothing", ledgers, more, err)
	}
	if _, err := s.CreateLedger(ctx, orgs[0], "newest", testNow); err != nil {
		t.Fatal(err)
	}
	ledgers, _, err = s.ListLedgers(ctx, orgs[0], "omega", 100)
	if err != nil || len(ledgers) != 1 || ledgers[0].Name != "newest" {
		t.Errorf("after the last ledger, once one is added = %v, %v; want [newest]", ledgers, err)
	}

	// A position from another organization's list doesn't work.
	if _, _, err := s.ListLedgers(ctx, orgs[0], "other-org", 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("listing after another organization's ledger: err = %v, want ErrNotFound", err)
	}
	if ledgers, _, err := s.ListLedgers(ctx, orgs[1], "", 100); err != nil || len(ledgers) != 1 {
		t.Errorf("other organization's list = %v, %v; want only its own ledger", ledgers, err)
	}
}
