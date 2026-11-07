package server_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// tamperDB runs SQL on the env's database file with the append-only
// triggers removed, as someone editing the file directly could. It opens the
// file separately from the server, with the same driver.
func tamperDB(t *testing.T, e *env, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", e.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range append([]string{"DROP TRIGGER IF EXISTS records_no_update", "DROP TRIGGER IF EXISTS records_no_delete"}, statements...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// chainResult is the body of a chain verification response.
type chainResult struct {
	Ledger         string  `json:"ledger"`
	Intact         bool    `json:"intact"`
	FirstSequence  *int64  `json:"first_sequence"`
	LastSequence   *int64  `json:"last_sequence"`
	LastHash       *string `json:"last_hash"`
	ReachedLatest  bool    `json:"reached_latest"`
	BrokenSequence *int64  `json:"broken_sequence"`
}

func (c chainResult) String() string {
	show := func(p *int64) string {
		if p == nil {
			return "null"
		}
		return fmt.Sprint(*p)
	}
	hash := "null"
	if c.LastHash != nil {
		hash = (*c.LastHash)[:8]
	}
	return fmt.Sprintf("intact=%v first=%s last=%s hash=%s latest=%v broken=%s", c.Intact, show(c.FirstSequence), show(c.LastSequence), hash, c.ReachedLatest, show(c.BrokenSequence))
}

func verifyChain(t *testing.T, e *env, key, query string) chainResult {
	t.Helper()
	var c chainResult
	resp := e.do(t, "GET", "/v1/ledgers/orders/chain-verification"+query, key, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chain verification%s: status %d: %s", query, resp.StatusCode, body(t, resp))
	}
	decode(t, resp, &c)
	return c
}

func TestChainVerification(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")

	// An empty ledger has nothing to check.
	raw := string(body(t, e.do(t, "GET", "/v1/ledgers/orders/chain-verification", key, "")))
	if want := `{"ledger":"orders","intact":true,"first_sequence":null,"last_sequence":null,"last_hash":null,"reached_latest":true,"broken_sequence":null}` + "\n"; raw != want {
		t.Errorf("empty ledger = %s, want %s", raw, want)
	}

	var records []apiRecord
	for i := range 5 {
		records = append(records, mustAppend(t, e, key, "orders", fmt.Sprintf(`{"data":{"i":%d}}`, i)))
	}
	if got := verifyChain(t, e, key, "").String(); got != "intact=true first=1 last=5 hash="+records[4].Hash[:8]+" latest=true broken=null" {
		t.Errorf("whole ledger: %s", got)
	}
	if got := verifyChain(t, e, key, "?from=3").String(); got != "intact=true first=3 last=5 hash="+records[4].Hash[:8]+" latest=true broken=null" {
		t.Errorf("from 3: %s", got)
	}
	if got := verifyChain(t, e, key, "?from=6").String(); got != "intact=true first=null last=null hash=null latest=true broken=null" {
		t.Errorf("from 6: %s", got)
	}
	// The full last_hash is the latest record's hash.
	if c := verifyChain(t, e, key, ""); c.LastHash == nil || *c.LastHash != records[4].Hash {
		t.Errorf("last_hash = %v, want %s", c.LastHash, records[4].Hash)
	}

	for _, q := range []string{"?from=0", "?from=-1", "?from=7", "?from=abc", "?from=1.5", "?from=1&from=2", "?from=99999999999999999999"} {
		p := checkProblem(t, e.do(t, "GET", "/v1/ledgers/orders/chain-verification"+q, key, ""), "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "from parameter") {
			t.Errorf("%s: detail = %q, want it to explain from", q, p.Detail)
		}
	}
	// Names no ledger can have get the same 404 as missing ledgers.
	checkProblem(t, e.do(t, "GET", "/v1/ledgers/Orders/chain-verification", key, ""), "ledger-not-found", http.StatusNotFound)
}

func TestChainVerificationFindsTampering(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for i := range 5 {
		mustAppend(t, e, key, "orders", fmt.Sprintf(`{"data":{"i":%d}}`, i))
	}
	tamperDB(t, e, `UPDATE records SET data = '{"i":99}' WHERE sequence = 3`)

	// A broken chain is a finding, not an error.
	raw := string(body(t, e.do(t, "GET", "/v1/ledgers/orders/chain-verification", key, "")))
	if want := `{"ledger":"orders","intact":false,"first_sequence":1,"last_sequence":3,"last_hash":null,"reached_latest":false,"broken_sequence":3}` + "\n"; raw != want {
		t.Errorf("after changing record 3 = %s, want %s", raw, want)
	}
	// Checking after the broken record finds the rest intact: record 3's
	// stored hash didn't change, so record 4 still links to it.
	if got := verifyChain(t, e, key, "?from=4").String(); !strings.HasPrefix(got, "intact=true first=4 last=5") {
		t.Errorf("from 4: %s", got)
	}
}

func TestChainVerificationIsIsolated(t *testing.T) {
	e := newEnv(t)
	keyA, keyB := e.fullKey(t), e.fullKey(t)
	mustCreate(t, e, keyA, "orders")
	mustAppend(t, e, keyA, "orders", `{"data":{}}`)
	pA := checkProblem(t, e.do(t, "GET", "/v1/ledgers/orders/chain-verification", keyB, ""), "ledger-not-found", http.StatusNotFound)
	pMissing := checkProblem(t, e.do(t, "GET", "/v1/ledgers/never-created/chain-verification", keyB, ""), "ledger-not-found", http.StatusNotFound)
	if pA.Detail != pMissing.Detail || pA.Title != pMissing.Title {
		t.Errorf("another organization's ledger: %+v; a missing ledger: %+v; want the same", pA, pMissing)
	}
	// A from past the end of A's ledger tells B nothing either.
	pA = checkProblem(t, e.do(t, "GET", "/v1/ledgers/orders/chain-verification?from=5", keyB, ""), "ledger-not-found", http.StatusNotFound)
	if pA.Detail != pMissing.Detail {
		t.Errorf("from past the end of another organization's ledger: %+v", pA)
	}
}
