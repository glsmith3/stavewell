package record

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestParseDataCanonicalForm(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{}`, `{}`},
		{" { } \n", `{}`},
		{`{"b": 1, "a": [true, false, null]}`, `{"a":[true,false,null],"b":1}`},
		{`{"n": -0}`, `{"n":0}`},
		{`{"max": 9007199254740991, "min": -9007199254740991}`, `{"max":9007199254740991,"min":-9007199254740991}`},
		// Strings use the shortest escapes: only quotes, backslashes and
		// control characters are escaped, and characters are otherwise
		// written as UTF-8.
		{`{"s": "A\/é😀"}`, `{"s":"A/é😀"}`},
		{`{"s": "\u0000\u001f\b\t\n\f\r\"\\"}`, `{"s":"\u0000\u001f\b\t\n\f\r\"\\"}`},
		{`{"s": "<&>` + "  " + `"}`, `{"s":"<&>` + "  " + `"}`},
		// Keys are sorted by UTF-16 code unit, not by code point: U+1F600
		// (written with the surrogate 0xD83D) sorts before U+E000.
		{`{"` + "" + `": 1, "😀": 2, "z": 3, "Z": 4}`, `{"Z":4,"z":3,"😀":2,"` + "" + `":1}`},
		{`{"outer": {"b": {"d": 1, "c": 2}, "a": []}}`, `{"outer":{"a":[],"b":{"c":2,"d":1}}}`},
	} {
		d, err := ParseData([]byte(c.in))
		if err != nil {
			t.Errorf("ParseData(%q): %v", c.in, err)
			continue
		}
		if d.Canonical() != c.want {
			t.Errorf("ParseData(%q) = %q, want %q", c.in, d.Canonical(), c.want)
		}
		sum := sha256.Sum256([]byte(c.want))
		if d.Hash() != hex.EncodeToString(sum[:]) {
			t.Errorf("ParseData(%q).Hash() = %s, want the SHA-256 hash of %q", c.in, d.Hash(), c.want)
		}
		// Canonical data is unchanged by parsing it again.
		again, err := ParseData([]byte(d.Canonical()))
		if err != nil || again != d {
			t.Errorf("parsing the canonical form of %q again = %+v, %v", c.in, again, err)
		}
	}
}

// nested returns an object nested depth levels deep, with arrays at even
// levels: {"a":[{"a":[...]}]}.
func nested(depth int) string {
	var open, close strings.Builder
	for level := 1; level <= depth; level++ {
		if level%2 == 1 {
			open.WriteString(`{"a":`)
			if level == depth {
				open.WriteString(`1`)
			}
		} else {
			open.WriteString(`[`)
			if level == depth {
				open.WriteString(`1`)
			}
		}
	}
	for level := depth; level >= 1; level-- {
		if level%2 == 1 {
			close.WriteString(`}`)
		} else {
			close.WriteString(`]`)
		}
	}
	return open.String() + close.String()
}

func TestParseDataRules(t *testing.T) {
	// padded returns an object whose canonical form is exactly n bytes.
	padded := func(n int) string {
		return `{"p": "` + strings.Repeat("x", n-len(`{"p":""}`)) + `"}`
	}
	valid := []string{
		`{"n": 9007199254740991}`,
		`{"n": -9007199254740991}`,
		`{"n": 0}`,
		`{"s": "😀"}`,
		nested(MaxDepth),
		padded(MaxDataBytes),
		// Whitespace doesn't count toward the size limit.
		`{"p": "` + strings.Repeat("x", MaxDataBytes-len(`{"p":""}`)) + `"` + strings.Repeat(" ", 1000) + `}`,
	}
	for _, in := range valid {
		if _, err := ParseData([]byte(in)); err != nil {
			t.Errorf("ParseData(%.60q): %v", in, err)
		}
	}

	for _, c := range []struct {
		in   string
		want error
	}{
		{``, ErrInvalidJSON},
		{`{"a":}`, ErrInvalidJSON},
		{`{"a":1} {}`, ErrInvalidJSON},
		{`{"a":1} x`, ErrInvalidJSON},
		{"\xef\xbb\xbf{}", ErrInvalidJSON},
		{`[]`, ErrNotObject},
		{`[{"a":1}]`, ErrNotObject},
		{`"text"`, ErrNotObject},
		{`1`, ErrNotObject},
		{`null`, ErrNotObject},
		{`true`, ErrNotObject},
		{`{"n": 9007199254740992}`, ErrNumber},
		{`{"n": -9007199254740992}`, ErrNumber},
		{`{"n": 123456789012345678901234567890}`, ErrNumber},
		{`{"n": 1.0}`, ErrNumber},
		{`{"n": 12.50}`, ErrNumber},
		{`{"n": 1e2}`, ErrNumber},
		{`{"n": 1E2}`, ErrNumber},
		{`{"n": -0.0}`, ErrNumber},
		{`{"n": 1e400}`, ErrNumber},
		{`{"a": [1, 2, 3.5]}`, ErrNumber},
		{`{"a": 1, "a": 2}`, ErrDuplicateKey},
		{`{"a": 1, "a": 2}`, ErrDuplicateKey},
		{`{"x": {"b": 1, "b": 1}}`, ErrDuplicateKey},
		{"{\"a\": \"\xff\"}", ErrInvalidString},
		{"{\"\xc3\": 1}", ErrInvalidString},
		{`{"a": "\ud800"}`, ErrInvalidString},
		{`{"a": "\udc00"}`, ErrInvalidString},
		{`{"a": "\ud800A"}`, ErrInvalidString},
		{`{"\ud800": 1}`, ErrInvalidString},
		{nested(MaxDepth + 1), ErrTooDeep},
		// Far deeper than the JSON decoder's own limit, which is 10,000.
		{nested(20000), ErrTooDeep},
		{`{"a": ` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}`, ErrTooDeep},
		{padded(MaxDataBytes + 1), ErrTooLarge},
	} {
		_, err := ParseData([]byte(c.in))
		if !errors.Is(err, c.want) {
			t.Errorf("ParseData(%.60q): err = %v, want %v", c.in, err, c.want)
		}
	}
}

// TestParseDataErrorsHideData checks that errors never quote the data.
func TestParseDataErrorsHideData(t *testing.T) {
	for _, in := range []string{
		`{"secret": 1, "secret": 2}`,
		`{"secret": "\ud800"}`,
		"{\"secret\": \"secret\xff\"}",
		`{"secret": 1.5}`,
		`{"secret": }`,
		`["secret"]`,
	} {
		_, err := ParseData([]byte(in))
		if err == nil {
			t.Fatalf("ParseData(%q) succeeded", in)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("ParseData(%q) error quotes the data: %v", in, err)
		}
	}
}

func TestZeroHash(t *testing.T) {
	if ZeroHash != strings.Repeat("0", 64) {
		t.Errorf("ZeroHash = %q", ZeroHash)
	}
}

// TestHash checks the example in the "Record hashes" section of
// api/openapi.yaml. The expected hashes were computed separately, with
// Python's hashlib over the canonical JSON written out by hand.
func TestHash(t *testing.T) {
	d, err := ParseData([]byte(`{"order_id": "A-1001", "amount": "12.50", "quantity": 2}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"amount":"12.50","order_id":"A-1001","quantity":2}`; d.Canonical() != want {
		t.Errorf("canonical data = %s, want %s", d.Canonical(), want)
	}
	if want := "c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c"; d.Hash() != want {
		t.Errorf("data hash = %s, want %s", d.Hash(), want)
	}
	h1, err := Hash(Fields{Ledger: "orders", Sequence: 1, CreatedAt: "2026-11-06T19:22:31.123Z", DataHash: d.Hash(), PreviousHash: ZeroHash})
	if err != nil {
		t.Fatal(err)
	}
	if want := "7f525a4bd1e34abb150d86e9b63ef5c403a239c796f7df6ae188de4ff6262e5a"; h1 != want {
		t.Errorf("record 1 hash = %s, want %s", h1, want)
	}

	d2, err := ParseData([]byte(`{"order_id": "A-1002", "amount": "7.25", "quantity": 1}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "86658e9cc2039fe716129d5f2f34d78d29d0efbaa929f07c8e7569e08c4f9fe5"; d2.Hash() != want {
		t.Errorf("record 2 data hash = %s, want %s", d2.Hash(), want)
	}
	h2, err := Hash(Fields{Ledger: "orders", Sequence: 2, CreatedAt: "2026-11-06T19:22:32.456Z", DataHash: d2.Hash(), PreviousHash: h1})
	if err != nil {
		t.Fatal(err)
	}
	if want := "1c868d7341149b79eab7851840f1acd2c13f91997e465944d5d10b16154075ce"; h2 != want {
		t.Errorf("record 2 hash = %s, want %s", h2, want)
	}
}

// TestHashCoversEveryField checks that changing any one field changes the
// record hash.
func TestHashCoversEveryField(t *testing.T) {
	base := Fields{Ledger: "orders", Sequence: 7, CreatedAt: "2026-11-06T19:22:31.123Z", DataHash: strings.Repeat("a", 64), PreviousHash: strings.Repeat("b", 64)}
	h, err := Hash(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Fields){
		"ledger":        func(f *Fields) { f.Ledger = "other" },
		"sequence":      func(f *Fields) { f.Sequence = 8 },
		"created_at":    func(f *Fields) { f.CreatedAt = "2026-11-06T19:22:31.124Z" },
		"data_hash":     func(f *Fields) { f.DataHash = strings.Repeat("c", 64) },
		"previous_hash": func(f *Fields) { f.PreviousHash = strings.Repeat("c", 64) },
	} {
		f := base
		change(&f)
		if got, err := Hash(f); err != nil || got == h {
			t.Errorf("changing %s: hash = %s, %v; want a different hash", name, got, err)
		}
	}
}

func TestHashRejectsSequenceOutOfRange(t *testing.T) {
	for _, seq := range []int64{0, -1, MaxNumber + 1} {
		if _, err := Hash(Fields{Ledger: "orders", Sequence: seq, CreatedAt: "2026-11-06T19:22:31.123Z", DataHash: ZeroHash, PreviousHash: ZeroHash}); err == nil {
			t.Errorf("Hash with sequence %d succeeded", seq)
		}
	}
	if _, err := Hash(Fields{Ledger: "orders", Sequence: MaxNumber, CreatedAt: "2026-11-06T19:22:31.123Z", DataHash: ZeroHash, PreviousHash: ZeroHash}); err != nil {
		t.Errorf("Hash with sequence %d: %v", int64(MaxNumber), err)
	}
}

func TestVerify(t *testing.T) {
	// Record 1 of the example in api/openapi.yaml.
	good := Stored{
		Ledger:       "orders",
		Sequence:     1,
		CreatedAt:    "2026-11-06T19:22:31.123Z",
		Data:         []byte(`{"amount":"12.50","order_id":"A-1001","quantity":2}`),
		DataHash:     "c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c",
		PreviousHash: ZeroHash,
		Hash:         "7f525a4bd1e34abb150d86e9b63ef5c403a239c796f7df6ae188de4ff6262e5a",
	}
	if !Verify(good, ZeroHash) {
		t.Fatal("Verify(the spec's record 1) = false")
	}
	// Data in another form with the same canonical JSON still passes.
	spaced := good
	spaced.Data = []byte(`{ "quantity": 2, "order_id": "A-1001", "amount": "12.50" }`)
	if !Verify(spaced, ZeroHash) {
		t.Error("Verify with non-canonical spacing and order = false")
	}
	for name, change := range map[string]func(*Stored){
		"data":          func(r *Stored) { r.Data = []byte(`{"amount":"12.51","order_id":"A-1001","quantity":2}`) },
		"data not JSON": func(r *Stored) { r.Data = []byte(`{"amount":`) },
		"data_hash":     func(r *Stored) { r.DataHash = strings.Repeat("0", 64) },
		"ledger":        func(r *Stored) { r.Ledger = "refunds" },
		"sequence":      func(r *Stored) { r.Sequence = 2 },
		"created_at":    func(r *Stored) { r.CreatedAt = "2026-11-06T19:22:31.124Z" },
		"previous_hash": func(r *Stored) { r.PreviousHash = strings.Repeat("1", 64) },
		"hash":          func(r *Stored) { r.Hash = strings.Repeat("2", 64) },
	} {
		r := good
		change(&r)
		if Verify(r, ZeroHash) {
			t.Errorf("Verify with a changed %s = true", name)
		}
	}
	// The link to the record before is checked.
	if Verify(good, strings.Repeat("3", 64)) {
		t.Error("Verify with the wrong previous record hash = true")
	}
}
