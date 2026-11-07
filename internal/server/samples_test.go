package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// sampleDir holds the published chain verification samples.
const sampleDir = "../../examples/chain-verification/"

// sample is one published chain verification sample.
type sample struct {
	name    string
	command string
	args    []string
}

var samples = []sample{
	{"Python", "python3", []string{"-I", sampleDir + "verify_chain.py"}},
	{"JavaScript", "node", []string{sampleDir + "verify_chain.js"}},
}

// edgeCases are record data that a reader's canonical JSON could easily get
// wrong.
var edgeCases = []string{
	`{}`,
	`{"order_id":"A-1001","amount":"12.50","quantity":2}`,
	// Key order: by UTF-16 code unit, so U+1F600 (stored as the surrogates
	// D83D DE00) sorts before U+E000 and U+FF5A, though its code point is
	// larger.
	`{"b":1,"a":2,"B":3,"A":4,"aa":5,"":6}`,
	`{"":1,"😀":2,"ｚ":3,"é":4,"é":5}`,
	`{"\u2028":1,"\ufeff":2,"\uffff":3,"𐀀":4}`,
	// Keys that JavaScript objects would otherwise put first.
	`{"10":1,"9":2,"1":3,"01":4,"-1":5}`,
	`{"__proto__":{"polluted":true},"constructor":1,"toString":2}`,
	// Escapes: control characters, quotes and backslashes are escaped;
	// "/", DEL, U+2028, U+2029 and <&> aren't.
	`{"s":"\u0000\u0001\u001f\b\t\n\f\r\"\\\/\u007f\u2028\u2029<&>"}`,
	`{"a\"b":1,"a\\b":2,"a\nb":3,"\u0000":4}`,
	`{"s":"日本語 🎉 😀 ñ é"}`,
	`{"max":9007199254740991,"min":-9007199254740991,"zero":0,"negative_zero":-0,"n":[1,-1,10,100,1000000]}`,
	`{"t":true,"f":false,"n":null,"a":[],"o":{},"nested":[[[]],[{}]]}`,
	nestedData(32),
	`{"p":"` + strings.Repeat("x", 16<<10-len(`{"p":""}`)) + `"}`,
}

// sampleRun is what a sample printed and its exit status.
type sampleRun struct {
	result *chainResult // nil if it printed no result
	exit   int
	stderr string
}

// runSample runs a sample against the server at baseURL.
func runSample(t *testing.T, s sample, path, baseURL, key, ledger string) sampleRun {
	t.Helper()
	cmd := exec.Command(path, append(s.args, ledger, "--url", baseURL)...)
	cmd.Env = []string{"STAVEWELL_API_KEY=" + key, "PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	run := sampleRun{stderr: stderr.String()}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%s sample: %v", s.name, err)
		}
		run.exit = exit.ExitCode()
	}
	run.stderr = stderr.String()
	if run.exit > 2 {
		t.Fatalf("%s sample: exit status %d\n%s", s.name, run.exit, run.stderr)
	}
	if out := strings.TrimSpace(stdout.String()); out != "" {
		var c chainResult
		if err := json.Unmarshal([]byte(out), &c); err != nil {
			t.Fatalf("%s sample printed %q: %v\n%s", s.name, out, err, run.stderr)
		}
		run.result = &c
	}
	return run
}

// full returns every field of c, for comparing results exactly.
func (c chainResult) full() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// sampleCase is one ledger the samples check, and what they must report.
type sampleCase struct {
	name     string
	baseURL  string
	key      string
	want     chainResult // the result a sample must print
	wantExit int
}

// edgeCaseLedger returns an env whose "orders" ledger holds the edge cases,
// and a key that can read and write it.
func edgeCaseLedger(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for _, d := range edgeCases {
		mustAppend(t, e, key, "orders", `{"data":`+d+`}`)
	}
	return e, key
}

// rewritingProxy returns a server in front of e that passes each
// successful list of records through rewrite, as a broken or dishonest
// server could.
func rewritingProxy(t *testing.T, e *env, rewrite func([]byte) ([]byte, error)) *httptest.Server {
	t.Helper()
	target, err := url.Parse(e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if !strings.HasSuffix(resp.Request.URL.Path, "/records") || resp.StatusCode != http.StatusOK {
			return nil
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if b, err = rewrite(b); err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.ContentLength = int64(len(b))
		resp.Header.Del("Content-Length")
		return nil
	}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv
}

// truncate returns a page with only its first 2 records, and has_more
// false.
func truncate(b []byte) ([]byte, error) {
	var page map[string]any
	if err := json.Unmarshal(b, &page); err != nil {
		return nil, err
	}
	if records, ok := page["records"].([]any); ok && len(records) > 2 {
		page["records"] = records[:2]
	}
	page["has_more"] = false
	return json.Marshal(page)
}

// TestSamplesAgreeWithServer checks that the published Python and JavaScript
// samples compute the same hashes as the server, and reach the same result
// as the server's chain verification: on edge-case records, across several
// pages, and after changes made by editing the database. It also checks
// that they don't report a chain as intact when the server returns only
// part of it. In CI, both Python 3 and Node.js must be installed.
// Elsewhere, a missing one skips that sample.
func TestSamplesAgreeWithServer(t *testing.T) {
	var cases []sampleCase

	// An intact ledger, over three pages.
	e, key := edgeCaseLedger(t)
	for i := range 230 {
		appendRecord(t, e, key, "orders", fmt.Sprintf(`{"i":%d}`, i))
	}
	intact := verifyChain(t, e, key, "")
	if !intact.Intact || *intact.LastSequence != int64(len(edgeCases)+230) {
		t.Fatalf("server: %s, want intact through every record", intact)
	}
	cases = append(cases, sampleCase{"intact", e.srv.URL, key, intact, 0})

	// Changes made by editing the database, each caught by a different
	// check. Record 2 holds {"amount":"12.50","order_id":"A-1001","quantity":2}.
	for _, c := range []struct {
		name, sql  string
		wantBroken int64
	}{
		{"data changed", `UPDATE records SET data = '{"i":99}' WHERE sequence = 5`, 5},
		{"number rewritten as 2.0", `UPDATE records SET data = '{"amount":"12.50","order_id":"A-1001","quantity":2.0}' WHERE sequence = 2`, 2},
		{"record removed", `DELETE FROM records WHERE sequence = 4`, 5},
		{"created_at changed", `UPDATE records SET created_at = '2030-01-01T00:00:00.000Z' WHERE sequence = 3`, 3},
		{"previous_hash changed", `UPDATE records SET previous_hash = '` + strings.Repeat("0", 64) + `' WHERE sequence = 6`, 6},
		{"latest record's hash changed", fmt.Sprintf(`UPDATE records SET hash = '%s' WHERE sequence = %d`, strings.Repeat("0", 64), len(edgeCases)), int64(len(edgeCases))},
	} {
		te, tkey := edgeCaseLedger(t)
		tamperDB(t, te, c.sql)
		res := verifyChain(t, te, tkey, "")
		if res.Intact || res.BrokenSequence == nil || *res.BrokenSequence != c.wantBroken {
			t.Fatalf("server after %s: %s, want broken at %d", c.name, res, c.wantBroken)
		}
		cases = append(cases, sampleCase{c.name, te.srv.URL, tkey, res, 1})
	}

	// Spacing added to stored data, with its value unchanged. The server
	// sees the stored text and reports the change. The API returns data
	// without the spacing, so the samples can't see it, and find the chain
	// intact. The docs say so.
	se, skey := edgeCaseLedger(t)
	before := verifyChain(t, se, skey, "")
	tamperDB(t, se, `UPDATE records SET data = '{ }' WHERE sequence = 1`)
	if res := verifyChain(t, se, skey, ""); res.Intact || *res.BrokenSequence != 1 {
		t.Fatalf("server after adding spacing: %s, want broken at 1", res)
	}
	cases = append(cases, sampleCase{"spacing added", se.srv.URL, skey, before, 0})

	// A server that returns only the first 2 records.
	proxy := rewritingProxy(t, e, truncate)
	two := chainResult{Ledger: "orders", Intact: true, FirstSequence: ptr(1), LastSequence: ptr(2), LastHash: nil}
	var first2 recordList
	decode(t, e.do(t, "GET", "/v1/ledgers/orders/records?limit=2", key, ""), &first2)
	two.LastHash = &first2.Records[1].Hash
	cases = append(cases, sampleCase{"server returns part of the ledger", proxy.URL, key, two, 2})
	malformed := rewritingProxy(t, e, func(b []byte) ([]byte, error) {
		return bytes.Replace(b, []byte(`"sequence":1,`), []byte(`"sequence":1.0,`), 1), nil
	})

	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			path, err := exec.LookPath(s.command)
			if err != nil {
				if os.Getenv("CI") != "" {
					t.Fatalf("%s isn't installed, and CI must run every sample: %v", s.command, err)
				}
				t.Skipf("%s isn't installed: %v", s.command, err)
			}
			for _, c := range cases {
				run := runSample(t, s, path, c.baseURL, c.key, "orders")
				if run.exit != c.wantExit {
					t.Errorf("%s: exit status %d, want %d\n%s", c.name, run.exit, c.wantExit, run.stderr)
				}
				if run.result == nil || run.result.full() != c.want.full() {
					got := "nothing"
					if run.result != nil {
						got = run.result.full()
					}
					t.Errorf("%s: sample printed %s, want %s", c.name, got, c.want.full())
				}
			}
			// Errors, and responses that aren't valid, print no result and
			// exit with status 2.
			for name, c := range map[string]struct{ baseURL, key string }{
				"unknown API key":         {e.srv.URL, "sw_" + strings.Repeat("a", 52)},
				"sequence written as 1.0": {malformed.URL, key},
			} {
				run := runSample(t, s, path, c.baseURL, c.key, "orders")
				if run.exit != 2 || run.result != nil {
					t.Errorf("%s: exit status %d, result %v; want 2 and no result", name, run.exit, run.result)
				}
			}
		})
	}
}

func ptr(n int64) *int64 { return &n }
