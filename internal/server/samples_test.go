package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	// Keys that JavaScript objects would otherwise put first.
	`{"10":1,"9":2,"1":3,"01":4,"-1":5}`,
	`{"__proto__":{"polluted":true},"constructor":1,"toString":2}`,
	// Escapes: control characters, quotes and backslashes are escaped;
	// "/", DEL, U+2028, U+2029 and <&> aren't.
	`{"s":"\u0000\u0001\u001f\b\t\n\f\r\"\\\/\u007f  <&>"}`,
	`{"a\"b":1,"a\\b":2,"a\nb":3,"\u0000":4}`,
	`{"s":"日本語 🎉 😀 ñ é"}`,
	`{"max":9007199254740991,"min":-9007199254740991,"zero":0,"negative_zero":-0,"n":[1,-1,10,100,1000000]}`,
	`{"t":true,"f":false,"n":null,"a":[],"o":{},"nested":[[[]],[{}]]}`,
	nestedData(32),
	`{"p":"` + strings.Repeat("x", 16<<10-len(`{"p":""}`)) + `"}`,
}

// runSample runs a sample against the env's server and returns its result.
func runSample(t *testing.T, e *env, s sample, path, key, ledger string) chainResult {
	t.Helper()
	cmd := exec.Command(path, append(s.args, ledger, "--url", e.srv.URL)...)
	cmd.Env = []string{"STAVEWELL_API_KEY=" + key, "PATH=" + os.Getenv("PATH")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		t.Fatalf("%s sample: %v\n%s", s.name, err, stderr.String())
	}
	var c chainResult
	if err := json.Unmarshal(stdout.Bytes(), &c); err != nil {
		t.Fatalf("%s sample printed %q: %v\n%s", s.name, stdout.String(), err, stderr.String())
	}
	if want := c.Intact; (err == nil) != want {
		t.Errorf("%s sample: exit error %v with intact=%v", s.name, err, c.Intact)
	}
	return c
}

// full returns every field of c, for comparing results exactly.
func (c chainResult) full() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// TestSamplesAgreeWithServer checks that the published Python and JavaScript
// samples compute the same hashes as the server, so that they agree with
// the server's chain verification: on edge-case records, across several
// pages, and after tampering. In CI, both Python 3 and Node.js must be
// installed. Elsewhere, a missing one skips that sample.
func TestSamplesAgreeWithServer(t *testing.T) {
	e := newEnv(t)
	key := e.fullKey(t)
	mustCreate(t, e, key, "orders")
	for _, d := range edgeCases {
		mustAppend(t, e, key, "orders", `{"data":`+d+`}`)
	}
	// Enough records for three pages.
	for i := range 230 {
		appendRecord(t, e, key, "orders", fmt.Sprintf(`{"i":%d}`, i))
	}

	server := verifyChain(t, e, key, "")
	if !server.Intact || *server.LastSequence != int64(len(edgeCases)+230) {
		t.Fatalf("server: %s, want intact through every record", server)
	}
	// Ledgers changed as someone editing the database could change them.
	// Changing data breaks that record's data_hash. Removing a record
	// breaks only the link from the next record.
	type tampered struct {
		name string
		e    *env
		key  string
		want chainResult
	}
	var changes []tampered
	for _, c := range []struct {
		name, sql  string
		wantBroken int64
	}{
		{"data changed", `UPDATE records SET data = '{"i":99}' WHERE sequence = 5`, 5},
		{"record removed", `DELETE FROM records WHERE sequence = 4`, 5},
	} {
		te := newEnv(t)
		tkey := te.fullKey(t)
		mustCreate(t, te, tkey, "orders")
		for _, d := range edgeCases {
			mustAppend(t, te, tkey, "orders", `{"data":`+d+`}`)
		}
		tamperDB(t, te, c.sql)
		res := verifyChain(t, te, tkey, "")
		if res.Intact || res.BrokenSequence == nil || *res.BrokenSequence != c.wantBroken {
			t.Fatalf("server after %s: %s, want broken at %d", c.name, res, c.wantBroken)
		}
		changes = append(changes, tampered{c.name, te, tkey, res})
	}

	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			path, err := exec.LookPath(s.command)
			if err != nil {
				if os.Getenv("CI") != "" {
					t.Fatalf("%s isn't installed, and CI must run every sample: %v", s.command, err)
				}
				t.Skipf("%s isn't installed: %v", s.command, err)
			}
			if got := runSample(t, e, s, path, key, "orders"); got.full() != server.full() {
				t.Errorf("intact ledger: sample %s, server %s", got.full(), server.full())
			}
			for _, c := range changes {
				if got := runSample(t, c.e, s, path, c.key, "orders"); got.full() != c.want.full() {
					t.Errorf("%s: sample %s, server %s", c.name, got.full(), c.want.full())
				}
			}
		})
	}
}
