package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/glsmith3/stavewell/internal/api"
	"github.com/glsmith3/stavewell/internal/store"
)

// TestAppendRecordHandlerChecksData calls the append handler directly, past
// checkAppendBody, to check its own data check and the errors it maps to.
// No database is needed: each request fails before reaching it.
func TestAppendRecordHandlerChecksData(t *testing.T) {
	s := &Server{}
	ctx := context.WithValue(context.Background(), principalKey{}, store.Principal{OrgRowID: 1, OrganizationID: "org_aaaaaaaaaaaaaaaaaaaaaaaaaa"})
	request := func(data string) api.AppendRecordRequestObject {
		return api.AppendRecordRequestObject{Name: "orders", Body: &api.AppendRecordRequest{Data: json.RawMessage(data)}}
	}
	for _, c := range []struct {
		req      api.AppendRecordRequestObject
		wantType string
		want413  bool
	}{
		{api.AppendRecordRequestObject{Name: "orders"}, "invalid-request", false},
		{request(`[]`), "invalid-record-data", false},
		{request(`{"n":1.5}`), "invalid-record-data", false},
		{request(`{"a":1,"a":2}`), "invalid-record-data", false},
		{request(`{"p":"` + strings.Repeat("x", 16<<10) + `"}`), "record-too-large", true},
	} {
		resp, err := s.AppendRecord(ctx, c.req)
		if err != nil {
			t.Fatalf("AppendRecord: %v", err)
		}
		var p api.Problem
		switch r := resp.(type) {
		case api.AppendRecord400ApplicationProblemPlusJSONResponse:
			p = api.Problem(r)
			if c.want413 {
				t.Errorf("got a 400 response, want 413")
			}
		case api.AppendRecord413ApplicationProblemPlusJSONResponse:
			p = api.Problem(r)
			if !c.want413 {
				t.Errorf("got a 413 response, want 400")
			}
		default:
			t.Fatalf("response %T, want an error", resp)
		}
		if p.Type != typeURLBase+c.wantType {
			t.Errorf("type = %s, want %s", p.Type, c.wantType)
		}
	}
}
