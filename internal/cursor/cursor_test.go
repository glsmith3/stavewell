package cursor_test

import (
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/glsmith3/stavewell/internal/cursor"
)

const org = "org_aaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRoundTrip(t *testing.T) {
	for _, after := range []string{"", "orders", "a-1", strings.Repeat("x", 64)} {
		p := cursor.Position{List: cursor.ListLedgers, OrganizationID: org, After: after}
		c := cursor.Encode(p)
		got, err := cursor.Decode(c, cursor.ListLedgers, org)
		if err != nil || got != p {
			t.Errorf("Decode(Encode(%+v)) = %+v, %v", p, got, err)
		}
		if strings.ContainsAny(c, "+/=") {
			t.Errorf("cursor %q isn't URL-safe", c)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	good := cursor.Encode(cursor.Position{List: cursor.ListLedgers, OrganizationID: org, After: "orders"})
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		name string
		c    string
		list string
		org  string
	}{
		{"empty", "", cursor.ListLedgers, org},
		{"another organization", good, cursor.ListLedgers, "org_bbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{"another list", good, "records", org},
		{"not base64", "not a cursor!", cursor.ListLedgers, org},
		{"padded base64", good + "==", cursor.ListLedgers, org},
		{"not JSON", raw("hello"), cursor.ListLedgers, org},
		{"another version", raw(`{"v":2,"l":"ledgers","o":"` + org + `","a":"orders"}`), cursor.ListLedgers, org},
		{"extra field", raw(`{"v":1,"l":"ledgers","o":"` + org + `","a":"orders","x":1}`), cursor.ListLedgers, org},
		{"trailing data", raw(`{"v":1,"l":"ledgers","o":"` + org + `","a":"orders"}{}`), cursor.ListLedgers, org},
		{"another spelling", raw(`{"a":"orders","l":"ledgers","o":"` + org + `","v":1}`), cursor.ListLedgers, org},
		{"too long", strings.Repeat("a", 513), cursor.ListLedgers, org},
	}
	for _, tt := range tests {
		if _, err := cursor.Decode(tt.c, tt.list, tt.org); err != cursor.ErrInvalid {
			t.Errorf("%s: err = %v, want ErrInvalid", tt.name, err)
		}
	}
}

func TestRecordListsAreSeparate(t *testing.T) {
	c := cursor.Encode(cursor.Position{List: cursor.ListRecords("orders"), OrganizationID: org, After: "2"})
	if p, err := cursor.Decode(c, cursor.ListRecords("orders"), org); err != nil || p.After != "2" {
		t.Errorf("Decode for the same ledger = %+v, %v", p, err)
	}
	for _, list := range []string{cursor.ListRecords("other"), cursor.ListRecords("order"), cursor.ListRecords("orders-2"), cursor.ListLedgers} {
		if _, err := cursor.Decode(c, list, org); err != cursor.ErrInvalid {
			t.Errorf("Decode for list %q: err = %v, want ErrInvalid", list, err)
		}
	}
	// The longest ledger name still fits.
	long := cursor.Encode(cursor.Position{List: cursor.ListRecords(strings.Repeat("x", 64)), OrganizationID: org, After: "9007199254740991"})
	if _, err := cursor.Decode(long, cursor.ListRecords(strings.Repeat("x", 64)), org); err != nil {
		t.Errorf("Decode of a cursor for a 64-character ledger name: %v", err)
	}
}

// TestSpecExamples checks that the example cursors in api/openapi.yaml are
// real cursors for the example organization.
func TestSpecExamples(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const exampleOrg = "org_gc2dmlmfey346kwie5ge5rb6zi"
	examples := regexp.MustCompile(`next_cursor: (\S+)`).FindAllStringSubmatch(string(spec), -1)
	if len(examples) != 2 {
		t.Fatalf("found %d example cursors in the spec, want 2", len(examples))
	}
	for i, list := range []string{cursor.ListLedgers, cursor.ListRecords("orders")} {
		if _, err := cursor.Decode(examples[i][1], list, exampleOrg); err != nil {
			t.Errorf("example cursor %d isn't a cursor for %q: %v", i+1, list, err)
		}
	}
}
