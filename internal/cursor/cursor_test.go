package cursor_test

import (
	"encoding/base64"
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
