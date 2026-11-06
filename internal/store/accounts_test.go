package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/glsmith3/stavewell/internal/apikey"
)

// testNow is a fixed time for account tests.
var testNow = time.Date(2026, 11, 6, 19, 22, 31, 123_456_789, time.UTC)

func TestCreateAndListOrganizations(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)

	a, err := s.CreateOrganization(ctx, "Northwind test", testNow)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateOrganization(ctx, "Contoso test ✓", testNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || !strings.HasPrefix(a.ID, "org_") {
		t.Errorf("IDs = %q, %q; want two different org_ IDs", a.ID, b.ID)
	}
	if want := testNow.Truncate(time.Millisecond); !a.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, want)
	}

	orgs, err := s.ListOrganizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 2 || orgs[0] != a || orgs[1] != b {
		t.Errorf("ListOrganizations() = %+v, want %+v then %+v", orgs, a, b)
	}
}

func TestCheckName(t *testing.T) {
	good := []string{"a", "Northwind test", strings.Repeat("é", 64), "Test 01 – synthetic"}
	bad := []string{"", strings.Repeat("a", 65), "tab\there", "new\nline", "nul\x00", "\x1b[31mred", "bad \xff utf-8"}
	for _, name := range good {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range bad {
		if err := CheckName(name); err == nil {
			t.Errorf("CheckName(%q) = nil, want an error", name)
		}
	}
	s, _ := openTestStore(t)
	if _, err := s.CreateOrganization(context.Background(), "", testNow); err == nil {
		t.Error("CreateOrganization with an empty name succeeded")
	}
}

func TestCreateAPIKey(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org, err := s.CreateOrganization(ctx, "Test", testNow)
	if err != nil {
		t.Fatal(err)
	}

	key, secret, err := s.CreateAPIKey(ctx, NewAPIKey{
		OrganizationID: org.ID,
		Name:           "Doc verification",
		Scopes:         []string{"ledger:write", "ledger:read", "ledger:write"},
		Days:           90,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !apikey.Valid(secret) {
		t.Errorf("secret %q isn't a valid key", secret)
	}
	if !strings.HasPrefix(key.ID, "key_") || key.OrganizationID != org.ID || key.Name != "Doc verification" {
		t.Errorf("key = %+v", key)
	}
	if want := []string{"ledger:read", "ledger:write"}; !slices.Equal(key.Scopes, want) {
		t.Errorf("Scopes = %v, want %v (duplicates removed, in standard order)", key.Scopes, want)
	}
	if want := testNow.Truncate(time.Millisecond).AddDate(0, 0, 90); !key.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", key.ExpiresAt, want)
	}

	// The key itself must not be stored anywhere; only its hash.
	snap := dbSnapshot(t, s.read)
	if strings.Contains(snap, secret) || strings.Contains(snap, secret[len(apikey.Prefix):]) {
		t.Error("the database contains the key itself")
	}
	if !strings.Contains(snap, apikey.Hash(secret)) {
		t.Error("the database doesn't contain the key's hash")
	}

	keys, err := s.ListAPIKeys(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ID != key.ID || !slices.Equal(keys[0].Scopes, key.Scopes) ||
		!keys[0].ExpiresAt.Equal(key.ExpiresAt) || keys[0].Revoked() {
		t.Errorf("ListAPIKeys() = %+v, want [%+v]", keys, key)
	}
}

func TestCreateAPIKeyRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org, err := s.CreateOrganization(ctx, "Test", testNow)
	if err != nil {
		t.Fatal(err)
	}
	valid := NewAPIKey{OrganizationID: org.ID, Name: "k", Scopes: []string{"ledger:read"}, Days: 90}
	tests := []struct {
		name   string
		change func(*NewAPIKey)
	}{
		{"no scopes", func(k *NewAPIKey) { k.Scopes = nil }},
		{"unknown scope", func(k *NewAPIKey) { k.Scopes = []string{"ledger:admin"} }},
		{"0 days", func(k *NewAPIKey) { k.Days = 0 }},
		{"366 days", func(k *NewAPIKey) { k.Days = 366 }},
		{"empty name", func(k *NewAPIKey) { k.Name = "" }},
		{"unknown organization", func(k *NewAPIKey) { k.OrganizationID = "org_aaaaaaaaaaaaaaaaaaaaaaaaaa" }},
	}
	for _, tt := range tests {
		nk := valid
		tt.change(&nk)
		if _, _, err := s.CreateAPIKey(ctx, nk, testNow); err == nil {
			t.Errorf("%s: CreateAPIKey succeeded, want an error", tt.name)
		}
	}
	nk := valid
	nk.OrganizationID = "org_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, _, err := s.CreateAPIKey(ctx, nk, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown organization: err = %v, want ErrNotFound", err)
	}
	if keys, err := s.ListAPIKeys(ctx, org.ID); err != nil || len(keys) != 0 {
		t.Errorf("after failed creates, ListAPIKeys() = %v, %v; want no keys", keys, err)
	}
	if _, err := s.ListAPIKeys(ctx, "org_aaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListAPIKeys for an unknown organization: err = %v, want ErrNotFound", err)
	}
	// 1 and 365 days are both allowed.
	for _, days := range []int{1, 365} {
		nk := valid
		nk.Days = days
		if _, _, err := s.CreateAPIKey(ctx, nk, testNow); err != nil {
			t.Errorf("%d days: %v", days, err)
		}
	}
}

func TestRevokeAPIKey(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	org, _ := s.CreateOrganization(ctx, "Test", testNow)
	key, secret, err := s.CreateAPIKey(ctx, NewAPIKey{OrganizationID: org.ID, Name: "k", Scopes: []string{"ledger:read"}, Days: 90}, testNow)
	if err != nil {
		t.Fatal(err)
	}

	revokedAt := testNow.Add(time.Hour)
	at, err := s.RevokeAPIKey(ctx, key.ID, revokedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !at.Equal(revokedAt.Truncate(time.Millisecond)) {
		t.Errorf("revoked at %v, want %v", at, revokedAt)
	}
	again, err := s.RevokeAPIKey(ctx, key.ID, revokedAt.Add(time.Hour))
	if !errors.Is(err, ErrAlreadyRevoked) || !again.Equal(at) {
		t.Errorf("second revoke = %v, %v; want ErrAlreadyRevoked and the first time %v", again, err, at)
	}
	if _, err := s.RevokeAPIKey(ctx, "key_aaaaaaaaaaaaaaaaaaaaaaaaaa", revokedAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking an unknown key: err = %v, want ErrNotFound", err)
	}
	keys, _ := s.ListAPIKeys(ctx, org.ID)
	if len(keys) != 1 || !keys[0].Revoked() || !keys[0].RevokedAt.Equal(at) {
		t.Errorf("ListAPIKeys() = %+v, want the key revoked at %v", keys, at)
	}
	var ie *InactiveKeyError
	if _, err := s.Authenticate(ctx, secret, revokedAt); !errors.As(err, &ie) || !ie.Revoked || ie.KeyID != key.ID {
		t.Errorf("Authenticate with a revoked key: err = %v, want a revoked InactiveKeyError for %s", err, key.ID)
	}
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	orgA, _ := s.CreateOrganization(ctx, "A", testNow)
	orgB, _ := s.CreateOrganization(ctx, "B", testNow)
	keyA, secretA, err := s.CreateAPIKey(ctx, NewAPIKey{OrganizationID: orgA.ID, Name: "a", Scopes: []string{"ledger:read", "ledger:write"}, Days: 1}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	keyB, secretB, err := s.CreateAPIKey(ctx, NewAPIKey{OrganizationID: orgB.ID, Name: "b", Scopes: []string{"ledger:read"}, Days: 1}, testNow)
	if err != nil {
		t.Fatal(err)
	}

	// Each key identifies its own organization, and only that one.
	pA, err := s.Authenticate(ctx, secretA, testNow)
	if err != nil {
		t.Fatal(err)
	}
	pB, err := s.Authenticate(ctx, secretB, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if pA.OrganizationID != orgA.ID || pA.KeyID != keyA.ID || !pA.HasScope("ledger:write") {
		t.Errorf("principal A = %+v", pA)
	}
	if pB.OrganizationID != orgB.ID || pB.KeyID != keyB.ID || pB.HasScope("ledger:write") || !pB.HasScope("ledger:read") {
		t.Errorf("principal B = %+v", pB)
	}
	if pA.OrgRowID == pB.OrgRowID {
		t.Error("two organizations' keys have the same organization row number")
	}

	// The key works until just before it expires, and not from then on.
	expiry := keyA.ExpiresAt
	if _, err := s.Authenticate(ctx, secretA, expiry.Add(-time.Millisecond)); err != nil {
		t.Errorf("a millisecond before expiry: %v", err)
	}
	var ie *InactiveKeyError
	if _, err := s.Authenticate(ctx, secretA, expiry); !errors.As(err, &ie) || ie.Revoked {
		t.Errorf("at expiry: err = %v, want an expired InactiveKeyError", err)
	}

	for _, tt := range []struct {
		name string
		key  string
		want error
	}{
		{"unknown", apikey.Generate(), ErrUnknownKey},
		{"example key", apikey.ExampleKey, ErrUnknownKey},
		{"malformed", "sw_short", ErrMalformedKey},
		{"empty", "", ErrMalformedKey},
		{"the stored hash", apikey.Hash(secretA), ErrMalformedKey},
		{"uppercase", strings.ToUpper(secretA), ErrMalformedKey},
	} {
		if _, err := s.Authenticate(ctx, tt.key, testNow); !errors.Is(err, tt.want) {
			t.Errorf("%s key: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}
