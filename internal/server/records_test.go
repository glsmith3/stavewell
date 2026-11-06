package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glsmith3/stavewell/internal/cursor"
	"github.com/glsmith3/stavewell/internal/record"
	"github.com/glsmith3/stavewell/internal/store"
)

// appendRecord appends a record through the store, for a key's organization.
func appendRecord(t *testing.T, e *env, key, ledger, data string) store.Record {
	t.Helper()
	return appendKeyedRecord(t, e, key, ledger, data, "")
}

// appendKeyedRecord is appendRecord with an idempotency key.
func appendKeyedRecord(t *testing.T, e *env, key, ledger, data, idempotencyKey string) store.Record {
	t.Helper()
	p, err := e.db.Authenticate(context.Background(), key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d, err := record.ParseData([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := e.db.AppendRecord(context.Background(), p.OrgRowID, ledger, d, idempotencyKey, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// apiRecord is a record as the API returns it.
type apiRecord struct {
	Ledger       string          `json:"ledger"`
	Sequence     int64           `json:"sequence"`
	CreatedAt    string          `json:"created_at"`
	Data         json.RawMessage `json:"data"`
	DataHash     string          `json:"data_hash"`
	PreviousHash string          `json:"previous_hash"`
	Hash         string          `json:"hash"`
}

// recordList is the body of a list of records.
type recordList struct {
	Records    []apiRecord `json:"records"`
	NextCursor string      `json:"next_cursor"`
	HasMore    bool        `json:"has_more"`
}

func (l recordList) sequences() []int64 {
	var seqs []int64
	for _, r := range l.Records {
		seqs = append(seqs, r.Sequence)
	}
	return seqs
}

// body reads a whole response body.
func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mustCreate creates a ledger through the API.
func mustCreate(t *testing.T, e *env, key, name string) {
	t.Helper()
	if resp := e.do(t, "POST", "/v1/ledgers", key, `{"name":"`+name+`"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating ledger %s: status %d", name, resp.StatusCode)
	}
}

// mustAppend appends a record through the API and returns it.
func mustAppend(t *testing.T, e *env, key, ledger, reqBody string) apiRecord {
	t.Helper()
	resp := e.do(t, "POST", "/v1/ledgers/"+ledger+"/records", key, reqBody)
	b := body(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("appending %s: status %d: %s", reqBody, resp.StatusCode, b)
	}
	var r apiRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

var timeFormat = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

// checkRecords checks the records of one ledger, from record 1: their
// sequence numbers, both hashes and the links between them.
func checkRecords(t *testing.T, records []apiRecord) {
	t.Helper()
	previous := record.ZeroHash
	for i, r := range records {
		if r.Sequence != int64(i+1) {
			t.Errorf("record %d has sequence number %d", i+1, r.Sequence)
		}
		if !timeFormat.MatchString(r.CreatedAt) {
			t.Errorf("record %d: created_at = %q", r.Sequence, r.CreatedAt)
		}
		sum := sha256.Sum256(r.Data)
		if r.DataHash != hex.EncodeToString(sum[:]) {
			t.Errorf("record %d: data_hash isn't the SHA-256 hash of data %s", r.Sequence, r.Data)
		}
		if r.PreviousHash != previous {
			t.Errorf("record %d: previous_hash = %s, want %s", r.Sequence, r.PreviousHash, previous)
		}
		want, err := record.Hash(record.Fields{Ledger: r.Ledger, Sequence: r.Sequence, CreatedAt: r.CreatedAt, DataHash: r.DataHash, PreviousHash: r.PreviousHash})
		if err != nil || r.Hash != want {
			t.Errorf("record %d: hash = %s, want %s (%v)", r.Sequence, r.Hash, want, err)
		}
		previous = r.Hash
	}
}

func TestAppendAndReadRecords(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")

	// An empty ledger has no latest record.
	if b := body(t, e.do(t, "GET", "/v1/ledgers/orders", key, "")); !strings.Contains(string(b), `"latest_sequence":null,"latest_hash":null`) {
		t.Errorf("empty ledger = %s, want null latest_sequence and latest_hash", b)
	}

	var appended []apiRecord
	for _, c := range []struct{ body, want string }{
		{`{"data": {"order_id": "A-1001", "amount": "12.50", "quantity": 2}}`, `{"amount":"12.50","order_id":"A-1001","quantity":2}`},
		{`{"data":{}}`, `{}`},
		{" \n{\"data\" :\n { \"b\" : [ 1, -0, true, null ], \"a\" : { } } }\n", `{"a":{},"b":[1,0,true,null]}`},
	} {
		r := mustAppend(t, e, key, "orders", c.body)
		if r.Ledger != "orders" || string(r.Data) != c.want {
			t.Errorf("appended %s: ledger %q, data %s; want orders and %s", c.body, r.Ledger, r.Data, c.want)
		}
		appended = append(appended, r)
	}
	checkRecords(t, appended)

	// Fields appear in the documented order.
	raw := string(body(t, e.do(t, "GET", "/v1/ledgers/orders/records/1", key, "")))
	if !regexp.MustCompile(`^\{"ledger":.*,"sequence":.*,"created_at":.*,"data":.*,"data_hash":.*,"previous_hash":.*,"hash":"[0-9a-f]{64}"\}\n$`).MatchString(raw) {
		t.Errorf("record body = %s, want fields in order", raw)
	}

	// Reading the records back gives the same records.
	for _, want := range appended {
		var got apiRecord
		decode(t, e.do(t, "GET", fmt.Sprintf("/v1/ledgers/orders/records/%d", want.Sequence), key, ""), &got)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("get record %d = %+v, want %+v", want.Sequence, got, want)
		}
	}
	var list recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", key, ""), &list)
	if fmt.Sprint(list.Records) != fmt.Sprint(appended) || list.HasMore {
		t.Errorf("list = %+v, want %+v and no more", list, appended)
	}

	// The ledger reports its latest record, in get and in list.
	var l struct {
		LatestSequence int64  `json:"latest_sequence"`
		LatestHash     string `json:"latest_hash"`
	}
	decode(t, e.do(t, "GET", "/v1/ledgers/orders", key, ""), &l)
	if l.LatestSequence != 3 || l.LatestHash != appended[2].Hash {
		t.Errorf("ledger latest = %+v, want 3 and %s", l, appended[2].Hash)
	}
	var ledgers struct {
		Ledgers []struct {
			LatestSequence int64  `json:"latest_sequence"`
			LatestHash     string `json:"latest_hash"`
		} `json:"ledgers"`
	}
	decode(t, e.do(t, "GET", "/v1/ledgers", key, ""), &ledgers)
	if len(ledgers.Ledgers) != 1 || ledgers.Ledgers[0].LatestSequence != 3 || ledgers.Ledgers[0].LatestHash != appended[2].Hash {
		t.Errorf("ledger list = %+v, want the latest record", ledgers)
	}
}

// TestRecordDataIsReturnedCanonically checks that data comes back exactly in
// canonical form, including characters that encoding/json would escape.
func TestRecordDataIsReturnedCanonically(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	req := `{"data": {"z": "<&>\u2028\u2029", "a": -0, "m": {"y": 1, "x": "\u00e9\ud83d\ude00\/"}}}`
	want := "{\"a\":0,\"m\":{\"x\":\"é😀/\",\"y\":1},\"z\":\"<&>\u2028\u2029\"}"
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/ledgers/orders/records", req},
		{"GET", "/v1/ledgers/orders/records/1", ""},
		{"GET", "/v1/ledgers/orders/records", ""},
	} {
		resp := e.do(t, c.method, c.path, key, c.body)
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: Content-Type = %q", c.method, c.path, ct)
		}
		if b := body(t, resp); !strings.Contains(string(b), `"data":`+want+`,`) {
			t.Errorf("%s %s = %s, want data exactly %s", c.method, c.path, b, want)
		}
	}
}

// nestedData returns a data object nested depth levels deep.
func nestedData(depth int) string {
	return strings.Repeat(`{"a":`, depth-1) + `{}` + strings.Repeat(`}`, depth-1)
}

// ledgerIsEmpty checks, through the API, that a ledger has no records.
func ledgerIsEmpty(t *testing.T, e *env, key, ledger string) {
	t.Helper()
	var list recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/"+ledger+"/records", key, ""), &list)
	if len(list.Records) != 0 {
		t.Errorf("ledger %s has records %v, want none", ledger, list.sequences())
	}
}

func TestRecordDataRules(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for _, c := range []struct{ data, wantDetail string }{
		{`[]`, "must be a JSON object"},
		{`"text"`, "must be a JSON object"},
		{`null`, "must be a JSON object"},
		{`{"amount":12.50}`, "whole numbers"},
		{`{"n":9007199254740992}`, "whole numbers"},
		{`{"n":-9007199254740992}`, "whole numbers"},
		{`{"n":1e3}`, "whole numbers"},
		{`{"list":[1,2,0.5]}`, "whole numbers"},
		{`{"a":1,"a":2}`, "same key"},
		{`{"a":1,"\u0061":2}`, "same key"},
		{`{"a":{"b":1,"b":1}}`, "same key"},
		{`{"a":"\ud800"}`, "UTF-8"},
		{"{\"a\":\"\xff\"}", "UTF-8"},
		{"{\"\xff\":1}", "UTF-8"},
		{nestedData(33), "32 levels"},
		// Deeper than the JSON decoder's own limit, which is 10,000, but
		// within the 64 KiB body limit.
		{nestedData(10200), "32 levels"},
		{`{"a":` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}`, "32 levels"},
	} {
		p := checkProblem(t, e.do(t, "POST", "/v1/ledgers/orders/records", key, `{"data":`+c.data+`}`), "invalid-record-data", http.StatusBadRequest)
		if !strings.Contains(p.Detail, c.wantDetail) {
			t.Errorf("data %.60s: detail = %q, want it to mention %q", c.data, p.Detail, c.wantDetail)
		}
	}
	ledgerIsEmpty(t, e, key, "orders")

	// The limits themselves are allowed.
	for _, data := range []string{nestedData(32), `{"max":9007199254740991,"min":-9007199254740991}`} {
		mustAppend(t, e, key, "orders", `{"data":`+data+`}`)
	}
}

// TestAppendBodyRules checks that an append's body is accepted only if it's
// one JSON object with only the data field.
func TestAppendBodyRules(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for _, b := range []string{
		``,
		`not json`,
		`[]`,
		`{"data":{},"extra":1}`,
		`{"extra":1,"data":{}}`,
		`{"data":{},"data":{}}`,
		`{"DATA":{}}`,
		`{"Data":{}}`,
		`{"data":{}} {}`,
		`{"data":{}} x`,
		`{"data":{},}`,
		`{"data":{}`,
		"\xef\xbb\xbf{\"data\":{}}",
	} {
		p := checkProblem(t, e.do(t, "POST", "/v1/ledgers/orders/records", key, b), "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "only the documented fields") {
			t.Errorf("%q: detail = %q, want it to explain the body rule", b, p.Detail)
		}
	}
	p := checkProblem(t, e.do(t, "POST", "/v1/ledgers/orders/records", key, `{}`), "invalid-request", http.StatusBadRequest)
	if !strings.Contains(p.Detail, "data field") {
		t.Errorf("no data field: detail = %q, want it to mention the data field", p.Detail)
	}
	ledgerIsEmpty(t, e, key, "orders")

	// Whitespace around the object, and an escaped field name, are fine.
	mustAppend(t, e, key, "orders", " \n {\"data\":{}} \n ")
	mustAppend(t, e, key, "orders", `{"d\u0061ta":{"escaped":true}}`)
}

func TestRecordSizeLimits(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	// padded returns data whose canonical form is exactly n bytes.
	padded := func(n int) string {
		return `{"p":"` + strings.Repeat("x", n-len(`{"p":""}`)) + `"}`
	}
	mustAppend(t, e, key, "orders", `{"data":`+padded(16<<10)+`}`)
	p := checkProblem(t, e.do(t, "POST", "/v1/ledgers/orders/records", key, `{"data":`+padded(16<<10+1)+`}`), "record-too-large", http.StatusRequestEntityTooLarge)
	if !strings.Contains(p.Detail, "16 KiB") {
		t.Errorf("detail = %q, want it to give the limit", p.Detail)
	}

	// Only the canonical form counts: whitespace in data doesn't.
	spaced := `{"p" :` + strings.Repeat(" ", 30_000) + `"` + strings.Repeat("x", 16<<10-len(`{"p":""}`)) + `"}`
	mustAppend(t, e, key, "orders", `{"data":`+spaced+`}`)

	// The whole body can be up to 64 KiB, including whitespace after it.
	small := `{"data":{}}`
	mustAppend(t, e, key, "orders", small+strings.Repeat(" ", 64<<10-len(small)))
	checkProblem(t, e.do(t, "POST", "/v1/ledgers/orders/records", key, small+strings.Repeat(" ", 64<<10+1-len(small))), "request-too-large", http.StatusRequestEntityTooLarge)

	var list recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", key, ""), &list)
	if fmt.Sprint(list.sequences()) != "[1 2 3]" {
		t.Errorf("records = %v, want only the three accepted appends", list.sequences())
	}
}

// TestRecordsAreIsolated checks that one organization can never see or
// affect another organization's records.
func TestRecordsAreIsolated(t *testing.T) {
	e := newEnv(t)
	keyA, keyB := e.fullKey(t), e.fullKey(t)
	mustCreate(t, e, keyA, "orders")
	mustAppend(t, e, keyA, "orders", `{"data":{"owner":"a"}}`)
	var listA recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", keyA, ""), &listA)

	// B gets the same 404 for A's ledger as for one that doesn't exist.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/records", `{"data":{"owner":"b"}}`},
		{"GET", "/records", ""},
		{"GET", "/records/1", ""},
	} {
		pA := checkProblem(t, e.do(t, c.method, "/v1/ledgers/orders"+c.path, keyB, c.body), "ledger-not-found", http.StatusNotFound)
		pMissing := checkProblem(t, e.do(t, c.method, "/v1/ledgers/never-created"+c.path, keyB, c.body), "ledger-not-found", http.StatusNotFound)
		if pA.Title != pMissing.Title || pA.Detail != pMissing.Detail {
			t.Errorf("%s %s: another organization's ledger: %+v; a missing ledger: %+v; want the same", c.method, c.path, pA, pMissing)
		}
	}

	// B's ledger with the same name is separate.
	mustCreate(t, e, keyB, "orders")
	r := mustAppend(t, e, keyB, "orders", `{"data":{"owner":"b"}}`)
	if r.Sequence != 1 || r.PreviousHash != record.ZeroHash {
		t.Errorf("B's first record = %+v, want record 1", r)
	}
	var listB recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", keyB, ""), &listB)
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", keyA, ""), &listA)
	if len(listA.Records) != 1 || string(listA.Records[0].Data) != `{"owner":"a"}` || len(listB.Records) != 1 || string(listB.Records[0].Data) != `{"owner":"b"}` {
		t.Errorf("A's records = %+v, B's = %+v; want only their own", listA.Records, listB.Records)
	}
	// Each organization's record 1 is its own.
	for key, want := range map[string]string{keyA: `{"owner":"a"}`, keyB: `{"owner":"b"}`} {
		var r apiRecord
		decode(t, e.do(t, "GET", "/v1/ledgers/orders/records/1", key, ""), &r)
		if string(r.Data) != want {
			t.Errorf("record 1 = %s, want the organization's own, %s", r.Data, want)
		}
	}

	// A's cursor doesn't work for B, even on B's ledger with the same name.
	checkProblem(t, e.do(t, "GET", "/v1/ledgers/orders/records?cursor="+url.QueryEscape(listA.NextCursor), keyB, ""), "invalid-cursor", http.StatusBadRequest)
}

func TestListRecordsPages(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")

	// An empty ledger's list is empty, not null, and its cursor works later.
	raw := body(t, e.do(t, "GET", "/v1/ledgers/orders/records", key, ""))
	if !strings.Contains(string(raw), `"records":[]`) {
		t.Errorf("empty list body = %s, want \"records\":[]", raw)
	}
	var empty recordList
	if err := json.Unmarshal(raw, &empty); err != nil || empty.HasMore || empty.NextCursor == "" {
		t.Fatalf("empty list = %+v, %v", empty, err)
	}
	for i := range 5 {
		mustAppend(t, e, key, "orders", fmt.Sprintf(`{"data":{"i":%d}}`, i))
	}
	var fromEmpty recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records?cursor="+url.QueryEscape(empty.NextCursor), key, ""), &fromEmpty)
	if fmt.Sprint(fromEmpty.sequences()) != "[1 2 3 4 5]" {
		t.Errorf("from the empty list's cursor = %v, want [1 2 3 4 5]", fromEmpty.sequences())
	}

	var all []apiRecord
	path := "/v1/ledgers/orders/records?limit=2"
	var page recordList
	for i := 0; ; i++ {
		page = recordList{}
		decode(t, e.do(t, "GET", path, key, ""), &page)
		all = append(all, page.Records...)
		if wantMore := len(all) < 5; page.HasMore != wantMore {
			t.Errorf("page %d: has_more = %v, want %v", i, page.HasMore, wantMore)
		}
		if !page.HasMore || i > 10 {
			break
		}
		path = "/v1/ledgers/orders/records?limit=2&cursor=" + url.QueryEscape(page.NextCursor)
	}
	checkRecords(t, all)
	if len(all) != 5 {
		t.Errorf("paged through %d records, want 5", len(all))
	}

	// The last page's cursor returns only records appended since, which is
	// how a client follows a ledger.
	var after recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records?cursor="+url.QueryEscape(page.NextCursor), key, ""), &after)
	if len(after.Records) != 0 || after.HasMore || after.NextCursor != page.NextCursor {
		t.Errorf("after the last page = %+v, want nothing and the same cursor", after)
	}
	mustAppend(t, e, key, "orders", `{"data":{"i":5}}`)
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records?cursor="+url.QueryEscape(page.NextCursor), key, ""), &after)
	if fmt.Sprint(after.sequences()) != "[6]" {
		t.Errorf("after the last page, once a record is appended = %v, want [6]", after.sequences())
	}
}

func TestRecordCursors(t *testing.T) {
	e := newEnv(t)
	org := e.newOrg(t)
	key := e.newKey(t, org, "ledger:read", "ledger:write")
	mustCreate(t, e, key, "a")
	mustCreate(t, e, key, "b")
	for range 2 {
		mustAppend(t, e, key, "a", `{"data":{}}`)
	}
	var fromA recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/a/records?limit=1", key, ""), &fromA)
	var ledgers ledgerList
	decode(t, e.do(t, "GET", "/v1/ledgers", key, ""), &ledgers)

	invalid := func(path, c string) {
		t.Helper()
		checkProblem(t, e.do(t, "GET", path+"?cursor="+url.QueryEscape(c), key, ""), "invalid-cursor", http.StatusBadRequest)
	}
	// A cursor works only on the list it came from.
	invalid("/v1/ledgers/b/records", fromA.NextCursor)
	invalid("/v1/ledgers/a/records", ledgers.NextCursor)
	invalid("/v1/ledgers", fromA.NextCursor)

	// Cursors that the server didn't make, even for the right list.
	forged := func(after string) string {
		return cursor.Encode(cursor.Position{List: cursor.ListRecords("a"), OrganizationID: org, After: after})
	}
	for _, after := range []string{"0", "007", "+1", "-1", "x", "1.0", "3"} {
		invalid("/v1/ledgers/a/records", forged(after))
	}
	for _, c := range []struct {
		after string
		want  string
	}{{"", "[1 2]"}, {"1", "[2]"}, {"2", "[]"}} {
		var list recordList
		decode(t, e.do(t, "GET", "/v1/ledgers/a/records?cursor="+url.QueryEscape(forged(c.after)), key, ""), &list)
		if got := fmt.Sprint(list.sequences()); got != c.want {
			t.Errorf("cursor after %q = %v, want %s", c.after, got, c.want)
		}
	}

	// Bad limits get the same error as for ledgers.
	for _, query := range []string{"limit=0", "limit=101", "limit=abc"} {
		p := checkProblem(t, e.do(t, "GET", "/v1/ledgers/a/records?"+query, key, ""), "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "from 1 to 100") {
			t.Errorf("%s: detail = %q, want it to give the range", query, p.Detail)
		}
	}
}

func TestGetRecordPaths(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	mustAppend(t, e, key, "orders", `{"data":{}}`)

	if resp := e.do(t, "GET", "/v1/ledgers/orders/records/1", key, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("record 1: status %d, want 200", resp.StatusCode)
	}
	for _, seq := range []string{"0", "-1", "abc", "1.5", "1e2", "99999999999999999999"} {
		p := checkProblem(t, e.do(t, "GET", "/v1/ledgers/orders/records/"+seq, key, ""), "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "whole number from 1") {
			t.Errorf("sequence %s: detail = %q, want it to explain sequence numbers", seq, p.Detail)
		}
	}
	checkProblem(t, e.do(t, "GET", "/v1/ledgers/orders/records/2", key, ""), "record-not-found", http.StatusNotFound)

	// Names no ledger can have get the same 404 as missing ledgers.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/ledgers/Orders/records", `{"data":{}}`},
		{"GET", "/v1/ledgers/Orders/records", ""},
		{"GET", "/v1/ledgers/Orders/records/1", ""},
		{"GET", "/v1/ledgers/" + strings.Repeat("a", 65) + "/records", ""},
	} {
		checkProblem(t, e.do(t, c.method, c.path, key, c.body), "ledger-not-found", http.StatusNotFound)
	}
}

// TestRecordLogsHideData checks that neither accepted nor rejected data
// reaches the log.
func TestRecordLogsHideData(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for _, b := range []string{
		`{"data":{"secret-key":"secret-value"}}`,
		`{"data":{"secret-key":1.5}}`,
		`{"data":{"secret-key":1,"secret-key":2}}`,
		`{"data":{"secret-key":"\ud800"}}`,
		`{"data":{"secret-key":}}`,
		`{"secret-key":{}}`,
		`{"data":{},"secret-key":1}`,
	} {
		e.serve(t, "POST", "/v1/ledgers/orders/records", key, b)
	}
	if logs := e.logs.String(); strings.Contains(logs, "secret") {
		t.Errorf("the log contains record data:\n%s", logs)
	}
}

// appendWithKey appends through the API with an Idempotency-Key header.
func appendWithKey(t *testing.T, e *env, key, ledger, idempotencyKey, reqBody string) (*http.Response, []byte) {
	t.Helper()
	resp := e.doWith(t, "POST", "/v1/ledgers/"+ledger+"/records", key, reqBody, map[string]string{"Idempotency-Key": idempotencyKey})
	b := body(t, resp)
	resp.Body = io.NopCloser(strings.NewReader(string(b))) // so checkProblem can read it
	return resp, b
}

func TestIdempotentAppend(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	mustCreate(t, e, key, "refunds")

	resp, first := appendWithKey(t, e, key, "orders", "order-A-1001", `{"data":{"order_id":"A-1001","quantity":2}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first append: status %d: %s", resp.StatusCode, first)
	}
	// A repeat with the same data, written differently, returns the same
	// record with 200 and appends nothing.
	resp, again := appendWithKey(t, e, key, "orders", "order-A-1001", "{ \"data\" : { \"quantity\" : 2, \"order_id\" : \"A-1001\" } }")
	if resp.StatusCode != http.StatusOK || string(again) != string(first) {
		t.Errorf("repeat: status %d, body %s; want 200 and %s", resp.StatusCode, again, first)
	}
	// Different data gets an error and appends nothing.
	resp, _ = appendWithKey(t, e, key, "orders", "order-A-1001", `{"data":{"order_id":"A-1002"}}`)
	p := checkProblem(t, resp, "idempotency-key-reused", http.StatusUnprocessableEntity)
	if !strings.Contains(p.Detail, "different data") {
		t.Errorf("detail = %q", p.Detail)
	}
	var list recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", key, ""), &list)
	if fmt.Sprint(list.sequences()) != "[1]" {
		t.Errorf("records = %v, want [1]", list.sequences())
	}

	// Keys are case-sensitive and belong to one ledger.
	for _, c := range []struct{ ledger, idemKey string }{{"orders", "ORDER-A-1001"}, {"orders", "order-A-1002"}, {"refunds", "order-A-1001"}} {
		if resp, b := appendWithKey(t, e, key, c.ledger, c.idemKey, `{"data":{"order_id":"A-1001","quantity":2}}`); resp.StatusCode != http.StatusCreated {
			t.Errorf("key %s in %s: status %d: %s; want a new record", c.idemKey, c.ledger, resp.StatusCode, b)
		}
	}
	// Without the header, every append is new.
	mustAppend(t, e, key, "orders", `{"data":{"order_id":"A-1001","quantity":2}}`)
	mustAppend(t, e, key, "orders", `{"data":{"order_id":"A-1001","quantity":2}}`)
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", key, ""), &list)
	if fmt.Sprint(list.sequences()) != "[1 2 3 4 5]" {
		t.Errorf("records = %v, want [1 2 3 4 5]", list.sequences())
	}
	checkRecords(t, list.Records)

	// The key isn't part of the record, and reading doesn't return it.
	for _, path := range []string{"/v1/ledgers/orders/records", "/v1/ledgers/orders/records/1"} {
		if b := body(t, e.do(t, "GET", path, key, "")); strings.Contains(string(b), "order-A-1001") || strings.Contains(strings.ToLower(string(b)), "idempotency") {
			t.Errorf("GET %s returns the key: %s", path, b)
		}
	}
}

func TestIdempotencyKeyFormat(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for _, k := range []string{"", "a b", "a.b", "a%41", "a,b", "a;b", "ключ", strings.Repeat("k", 129)} {
		resp, _ := appendWithKey(t, e, key, "orders", k, `{"data":{}}`)
		p := checkProblem(t, resp, "invalid-request", http.StatusBadRequest)
		if !strings.Contains(p.Detail, "Idempotency-Key") {
			t.Errorf("key %q: detail = %q, want it to explain the key rule", k, p.Detail)
		}
	}
	// Two headers.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/ledgers/orders/records", strings.NewReader(`{"data":{}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Add("Idempotency-Key", "one")
	req.Header.Add("Idempotency-Key", "two")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	p := checkProblem(t, resp, "invalid-request", http.StatusBadRequest)
	resp.Body.Close()
	if !strings.Contains(p.Detail, "Idempotency-Key") {
		t.Errorf("two headers: detail = %q, want it to explain the key rule", p.Detail)
	}
	ledgerIsEmpty(t, e, key, "orders")

	for _, k := range []string{"a", strings.Repeat("k", 128), "550e8400-e29b-41d4-a716-446655440000", "A_b-9"} {
		if resp, b := appendWithKey(t, e, key, "orders", k, `{"data":{}}`); resp.StatusCode != http.StatusCreated {
			t.Errorf("key %q: status %d: %s; want 201", k, resp.StatusCode, b)
		}
	}
}

// TestIdempotencyKeysAreIsolated checks that a key can't reach another
// organization's records.
func TestIdempotencyKeysAreIsolated(t *testing.T) {
	e := newEnv(t)
	keyA, keyB := e.fullKey(t), e.fullKey(t)
	mustCreate(t, e, keyA, "orders")
	if resp, b := appendWithKey(t, e, keyA, "orders", "shared-key", `{"data":{"owner":"a"}}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("A's append: status %d: %s", resp.StatusCode, b)
	}
	// B's repeat of A's append on A's ledger gets the same 404 as a missing
	// ledger, not A's record.
	resp, b := appendWithKey(t, e, keyB, "orders", "shared-key", `{"data":{"owner":"a"}}`)
	if strings.Contains(string(b), `"owner":"a"`) {
		t.Errorf("B got A's record: %s", b)
	}
	checkProblem(t, resp, "ledger-not-found", http.StatusNotFound)

	// B's own ledger with the same name and key is separate.
	mustCreate(t, e, keyB, "orders")
	resp, b = appendWithKey(t, e, keyB, "orders", "shared-key", `{"data":{"owner":"a"}}`)
	var r apiRecord
	if err := json.Unmarshal(b, &r); err != nil || resp.StatusCode != http.StatusCreated || r.Sequence != 1 {
		t.Errorf("B's append: status %d: %s; want a new record 1", resp.StatusCode, b)
	}
}

func TestIdempotentAppendUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	const n = 20
	statuses := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req, _ := http.NewRequest("POST", e.srv.URL+"/v1/ledgers/orders/records", strings.NewReader(`{"data":{"order_id":"A-1001"}}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Idempotency-Key", "order-A-1001")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return // statuses[i] stays 0, which fails below
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			statuses[i], bodies[i] = resp.StatusCode, string(b)
		})
	}
	wg.Wait()
	created := 0
	for i := range n {
		switch statuses[i] {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Errorf("request %d: status %d: %s", i, statuses[i], bodies[i])
		}
		if bodies[i] != bodies[0] {
			t.Errorf("request %d got %s, request 0 got %s; want the same record", i, bodies[i], bodies[0])
		}
	}
	if created != 1 {
		t.Errorf("%d requests got 201, want 1", created)
	}
	var list recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records", key, ""), &list)
	if len(list.Records) != 1 {
		t.Errorf("the ledger has %d records, want 1", len(list.Records))
	}
}

// TestIdempotencyKeysAreNotLogged checks that keys, valid or not, never
// reach the log.
func TestIdempotencyKeysAreNotLogged(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for _, k := range []string{"secretkey-1", "secretkey 2", "secretkey%33"} {
		req := httptest.NewRequest("POST", "/v1/ledgers/orders/records", strings.NewReader(`{"data":{}}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", k)
		e.handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest("POST", "/v1/ledgers/orders/records", strings.NewReader(`{"data":{}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Add("Idempotency-Key", "secretkey-4")
	req.Header.Add("Idempotency-Key", "secretkey-5")
	e.handler.ServeHTTP(httptest.NewRecorder(), req)
	if logs := e.logs.String(); strings.Contains(logs, "secretkey") {
		t.Errorf("the log contains an idempotency key:\n%s", logs)
	}
}

// TestIdempotencyKeyCheckOrder checks which error an append gets when the
// Idempotency-Key header is invalid and something else is wrong too. The
// key, scope and body are checked first, then the header, then the ledger.
func TestIdempotencyKeyCheckOrder(t *testing.T) {
	e := newEnv(t)
	org := e.newOrg(t)
	key := e.newKey(t, org, "ledger:read", "ledger:write")
	readOnly := e.newKey(t, org, "ledger:read")
	mustCreate(t, e, key, "orders")
	bad := "not valid"
	for _, c := range []struct {
		name, apiKey, ledger, idemKey, body string
		wantType                            string
		wantStatus                          int
		wantDetail                          string
	}{
		{"no API key", "", "orders", bad, `{"data":{}}`, "unauthorized", 401, ""},
		{"read-only key", readOnly, "orders", bad, `{"data":{}}`, "insufficient-scope", 403, ""},
		{"invalid body", key, "orders", bad, `{"data":{},"extra":1}`, "invalid-request", 400, "only the documented fields"},
		{"invalid data", key, "orders", bad, `{"data":{"n":1.5}}`, "invalid-record-data", 400, "whole numbers"},
		{"missing ledger", key, "missing", bad, `{"data":{}}`, "invalid-request", 400, "Idempotency-Key"},
		{"missing ledger, valid key", key, "missing", "valid-key", `{"data":{}}`, "ledger-not-found", 404, ""},
	} {
		resp := e.doWith(t, "POST", "/v1/ledgers/"+c.ledger+"/records", c.apiKey, c.body, map[string]string{"Idempotency-Key": c.idemKey})
		p := checkProblem(t, resp, c.wantType, c.wantStatus)
		if !strings.Contains(p.Detail, c.wantDetail) {
			t.Errorf("%s: detail = %q, want it to mention %q", c.name, p.Detail, c.wantDetail)
		}
	}
	ledgerIsEmpty(t, e, key, "orders")
}
