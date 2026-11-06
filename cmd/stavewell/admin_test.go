package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/glsmith3/stavewell/internal/apikey"
	"github.com/glsmith3/stavewell/internal/store"
)

// adminNow is the fixed time admin commands see in these tests.
var adminNow = time.Date(2026, 11, 6, 19, 22, 31, 123_000_000, time.UTC)

// admin runs an admin command against the database at dbPath at time now.
func admin(t *testing.T, dbPath string, now time.Time, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := command{ctx: context.Background(), stdout: &out, stderr: &errOut, dbPath: dbPath, now: func() time.Time { return now }}
	code = runAdmin(c, args)
	return out.String(), errOut.String(), code
}

// field returns the value on the line that starts with label, in output
// such as "ID:  org_…".
func field(t *testing.T, output, label string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(label) + `:\s+(.+)$`).FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("output has no %q line:\n%s", label, output)
	}
	return m[1]
}

func newDBPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "stavewell.db")
}

func TestAdminOrganizations(t *testing.T) {
	db := newDBPath(t)
	out, _, code := admin(t, db, adminNow, "org", "list")
	if code != exitOK || out != "No organizations.\n" {
		t.Errorf("org list on a new database = %d, %q; want 0, \"No organizations.\"", code, out)
	}

	out, errOut, code := admin(t, db, adminNow, "org", "create", "--name", "Northwind test")
	if code != exitOK {
		t.Fatalf("org create = %d; stderr: %s", code, errOut)
	}
	id := field(t, out, "ID")
	if !regexp.MustCompile(`^org_[a-z2-7]{26}$`).MatchString(id) {
		t.Errorf("ID = %q", id)
	}
	if got := field(t, out, "Name"); got != "Northwind test" {
		t.Errorf("Name = %q", got)
	}
	if got := field(t, out, "Created"); got != "2026-11-06T19:22:31.123Z" {
		t.Errorf("Created = %q", got)
	}

	out, _, code = admin(t, db, adminNow, "org", "list")
	if code != exitOK || !strings.HasPrefix(out, "ID ") || !strings.Contains(out, id) || !strings.Contains(out, "Northwind test") {
		t.Errorf("org list = %d:\n%s", code, out)
	}
}

func TestAdminKeyLifecycle(t *testing.T) {
	db := newDBPath(t)
	out, _, _ := admin(t, db, adminNow, "org", "create", "--name", "Test")
	org := field(t, out, "ID")

	out, errOut, code := admin(t, db, adminNow, "key", "create", "--org", org, "--name", "Doc verification", "--scopes", "ledger:write, ledger:read")
	if code != exitOK {
		t.Fatalf("key create = %d; stderr: %s", code, errOut)
	}
	secret := field(t, out, "Key")
	keyID := field(t, out, "Key ID")
	if !apikey.Valid(secret) || !strings.HasPrefix(keyID, "key_") {
		t.Errorf("Key = %q, Key ID = %q", secret, keyID)
	}
	if got := field(t, out, "Scopes"); got != "ledger:read, ledger:write" {
		t.Errorf("Scopes = %q", got)
	}
	if got := field(t, out, "Expires"); got != "2027-02-04T19:22:31.123Z" {
		t.Errorf("Expires = %q, want 90 days after creation", got)
	}
	if !strings.Contains(out, "only time the key is shown") {
		t.Errorf("output doesn't warn that the key is shown once:\n%s", out)
	}

	// The key works.
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if p, err := s.Authenticate(context.Background(), secret, adminNow); err != nil || p.KeyID != keyID || p.OrganizationID != org {
		t.Fatalf("Authenticate = %+v, %v", p, err)
	}

	// Listing shows the key ID and status, never the key.
	out, _, code = admin(t, db, adminNow, "key", "list", "--org", org)
	if code != exitOK || !strings.Contains(out, keyID) || !strings.Contains(out, "active") || !strings.Contains(out, "ledger:read,ledger:write") {
		t.Errorf("key list = %d:\n%s", code, out)
	}
	if strings.Contains(out, secret) {
		t.Error("key list shows the key itself")
	}
	out, _, _ = admin(t, db, adminNow.AddDate(0, 0, 90), "key", "list", "--org", org)
	if !strings.Contains(out, "expired") {
		t.Errorf("key list after expiry doesn't say expired:\n%s", out)
	}

	// Revoking.
	revokeTime := adminNow.Add(time.Hour)
	out, errOut, code = admin(t, db, revokeTime, "key", "revoke", "--key", keyID)
	if code != exitOK || out != "API key "+keyID+" revoked at 2026-11-06T20:22:31.123Z.\n" {
		t.Errorf("key revoke = %d, %q; stderr: %s", code, out, errOut)
	}
	out, _, code = admin(t, db, revokeTime.Add(time.Hour), "key", "revoke", "--key", keyID)
	if code != exitOK || !strings.Contains(out, "already revoked at 2026-11-06T20:22:31.123Z") {
		t.Errorf("second key revoke = %d, %q", code, out)
	}
	out, _, _ = admin(t, db, revokeTime, "key", "list", "--org", org)
	if !strings.Contains(out, "revoked 2026-11-06T20:22:31.123Z") {
		t.Errorf("key list after revoking:\n%s", out)
	}
	var ie *store.InactiveKeyError
	if _, err := s.Authenticate(context.Background(), secret, revokeTime); !errors.As(err, &ie) || !ie.Revoked {
		t.Errorf("Authenticate after revoking: err = %v, want a revoked key error", err)
	}
}

func TestAdminKeyExpiry(t *testing.T) {
	db := newDBPath(t)
	out, _, _ := admin(t, db, adminNow, "org", "create", "--name", "Test")
	org := field(t, out, "ID")
	for _, tt := range []struct {
		days string
		want string
	}{
		{"1", "2026-11-07T19:22:31.123Z"},
		{"365", "2027-11-06T19:22:31.123Z"},
	} {
		out, errOut, code := admin(t, db, adminNow, "key", "create", "--org", org, "--name", "k", "--scopes", "ledger:read", "--expires-in-days", tt.days)
		if code != exitOK {
			t.Fatalf("--expires-in-days %s: code %d; stderr: %s", tt.days, code, errOut)
		}
		if got := field(t, out, "Expires"); got != tt.want {
			t.Errorf("--expires-in-days %s: Expires = %q, want %q", tt.days, got, tt.want)
		}
	}
}

func TestAdminUsageErrors(t *testing.T) {
	db := newDBPath(t)
	for _, args := range [][]string{
		{},
		{"org"},
		{"org", "delete"},
		{"ledger", "list"},
		{"org", "create"},
		{"org", "create", "--name"},
		{"org", "create", "--name", "x", "extra"},
		{"org", "create", "--colour", "x"},
		{"org", "list", "extra"},
		{"key", "create", "--org", "org_x", "--name", "k"},
		{"key", "create", "--org", "org_x", "--name", "k", "--scopes", "ledger:read", "--expires-in-days", "ninety"},
		{"key", "list"},
		{"key", "revoke"},
	} {
		_, errOut, code := admin(t, db, adminNow, args...)
		if code != exitUsage || !strings.Contains(errOut, "Usage:") {
			t.Errorf("admin %q = %d; want %d and the usage text; stderr: %s", args, code, exitUsage, errOut)
		}
	}
}

func TestAdminNeverRepeatsKeys(t *testing.T) {
	db := newDBPath(t)
	out, _, _ := admin(t, db, adminNow, "org", "create", "--name", "Test")
	org := field(t, out, "ID")
	key := apikey.Generate()
	// Any of these pieces in the output would expose the key.
	pieces := []string{key, key[3:19], strings.ToUpper(key[3:19])}
	tests := []struct {
		name string
		args []string
	}{
		{"revoke with the key", []string{"key", "revoke", "--key", key}},
		{"revoke with a trailing space", []string{"key", "revoke", "--key", key + " "}},
		{"revoke with Bearer in front", []string{"key", "revoke", "--key", "Bearer " + key}},
		{"revoke without --key", []string{"key", "revoke", key}},
		{"revoke with an uppercase key", []string{"key", "revoke", "--key", strings.ToUpper(key)}},
		{"revoke with a key one character short", []string{"key", "revoke", "--key", key[:len(key)-1]}},
		{"key list with the key as --org", []string{"key", "list", "--org", key}},
		{"key create with the key as --org", []string{"key", "create", "--org", key, "--name", "k", "--scopes", "ledger:read"}},
		{"key create with the key in --name", []string{"key", "create", "--org", org, "--name", "copy of " + key, "--scopes", "ledger:read"}},
		{"org create with the key as --name", []string{"org", "create", "--name", key}},
		{"the key as an unknown command", []string{key, "list"}},
		{"the key as an unknown flag", []string{"org", "list", "--" + key}},
	}
	for _, tt := range tests {
		out, errOut, code := admin(t, db, adminNow, tt.args...)
		if code != exitError || !strings.Contains(errOut, "looks like it contains an API key") {
			t.Errorf("%s: code %d, stderr %q; want %d and the API key message", tt.name, code, errOut, exitError)
		}
		for _, piece := range pieces {
			if strings.Contains(out+errOut, piece) {
				t.Errorf("%s: output contains part of the key", tt.name)
			}
		}
	}
	// Nothing was created.
	if out, _, _ := admin(t, db, adminNow, "org", "list"); strings.Count(out, "org_") != 1 {
		t.Errorf("org list after refused commands:\n%s", out)
	}
	if out, _, _ := admin(t, db, adminNow, "key", "list", "--org", org); !strings.Contains(out, "has no API keys") {
		t.Errorf("key list after refused commands:\n%s", out)
	}
}

func TestRunCommand(t *testing.T) {
	db := newDBPath(t)
	key := apikey.Generate()
	tests := []struct {
		args     []string
		wantCode int
		wantOut  string // in stdout
		wantErr  string // in stderr
	}{
		{[]string{"help"}, exitOK, "Usage:", ""},
		{[]string{"--help"}, exitOK, "Usage:", ""},
		{[]string{"admin", "org", "list"}, exitOK, "No organizations.", ""},
		{[]string{"serve"}, exitUsage, "", `unknown command "serve"`},
		{[]string{key}, exitError, "", "looks like it contains an API key"},
		{[]string{"admin", "key", "revoke", "--key", key}, exitError, "", "looks like it contains an API key"},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		c := command{ctx: context.Background(), stdout: &out, stderr: &errOut, dbPath: db, now: func() time.Time { return adminNow }}
		code := runCommand(c, tt.args)
		if code != tt.wantCode || !strings.Contains(out.String(), tt.wantOut) || !strings.Contains(errOut.String(), tt.wantErr) {
			t.Errorf("runCommand(%q) = %d, stdout %q, stderr %q", tt.args, code, out.String(), errOut.String())
		}
		if strings.Contains(out.String()+errOut.String(), key[3:19]) {
			t.Errorf("runCommand(%q) repeated part of the key", tt.args[:1])
		}
	}
}

func TestAdminHelp(t *testing.T) {
	db := newDBPath(t)
	for _, args := range [][]string{
		{"help"}, {"--help"}, {"-h"},
		{"org", "create", "--help"},
		{"key", "create", "-h"},
		{"key", "list", "--org", "org_x", "--help"},
	} {
		out, errOut, code := admin(t, db, adminNow, args...)
		if code != exitOK || !strings.HasPrefix(out, "Usage:") || errOut != "" {
			t.Errorf("admin %q = %d, stdout %q, stderr %q; want 0 and the usage on stdout", args, code, out, errOut)
		}
	}
}

func TestAdminErrors(t *testing.T) {
	db := newDBPath(t)
	out, _, _ := admin(t, db, adminNow, "org", "create", "--name", "Test")
	org := field(t, out, "ID")
	unknownOrg := "org_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	secret := apikey.Generate()
	tests := []struct {
		name string
		args []string
		want string // in stderr
	}{
		{"empty name", []string{"org", "create", "--name", ""}, "--name is required"},
		{"name too long", []string{"org", "create", "--name", strings.Repeat("a", 65)}, "longer than 64"},
		{"name with a control character", []string{"org", "create", "--name", "a\tb"}, "control, formatting"},
		{"key for an unknown organization", []string{"key", "create", "--org", unknownOrg, "--name", "k", "--scopes", "ledger:read"}, "not found"},
		{"unknown scope", []string{"key", "create", "--org", org, "--name", "k", "--scopes", "ledger:admin"}, "unknown scope"},
		{"only commas for scopes", []string{"key", "create", "--org", org, "--name", "k", "--scopes", ","}, "at least one scope"},
		{"0 days", []string{"key", "create", "--org", org, "--name", "k", "--scopes", "ledger:read", "--expires-in-days", "0"}, "1 to 365 days"},
		{"366 days", []string{"key", "create", "--org", org, "--name", "k", "--scopes", "ledger:read", "--expires-in-days", "366"}, "1 to 365 days"},
		{"list keys of an unknown organization", []string{"key", "list", "--org", unknownOrg}, "not found"},
		{"revoke an unknown key", []string{"key", "revoke", "--key", "key_aaaaaaaaaaaaaaaaaaaaaaaaaa"}, "not found"},
		{"revoke with the key itself", []string{"key", "revoke", "--key", secret}, "looks like it contains an API key"},
		{"name with a right-to-left override", []string{"org", "create", "--name", "abc\u202edcba"}, "formatting"},
		{"name with a trailing space", []string{"org", "create", "--name", "Test "}, "start or end with a space"},
	}
	for _, tt := range tests {
		out, errOut, code := admin(t, db, adminNow, tt.args...)
		if code == exitOK || !strings.Contains(errOut, tt.want) {
			t.Errorf("%s: code %d, stderr %q; want a failure mentioning %q", tt.name, code, errOut, tt.want)
		}
		if strings.Contains(out+errOut, secret) {
			t.Errorf("%s: output repeats the key", tt.name)
		}
	}
	// None of the failed commands created a key.
	if out, _, _ := admin(t, db, adminNow, "key", "list", "--org", org); !strings.Contains(out, "has no API keys") {
		t.Errorf("after failed commands, key list:\n%s", out)
	}
}
