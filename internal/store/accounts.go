package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/glsmith3/stavewell/internal/apikey"
	"github.com/glsmith3/stavewell/internal/ids"
)

// Errors returned by the account functions.
var (
	// ErrNotFound means no organization or API key has the given ID.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyRevoked means the API key was revoked earlier.
	ErrAlreadyRevoked = errors.New("API key already revoked")
	// ErrMalformedKey means the presented key doesn't have the form of an
	// API key.
	ErrMalformedKey = errors.New("malformed API key")
	// ErrUnknownKey means no API key has the presented value.
	ErrUnknownKey = errors.New("unknown API key")
)

// InactiveKeyError means the presented API key exists but is expired or
// revoked.
type InactiveKeyError struct {
	KeyID   string // the key's public ID, which isn't secret
	Revoked bool   // true if revoked, false if expired
}

func (e *InactiveKeyError) Error() string {
	if e.Revoked {
		return "API key " + e.KeyID + " is revoked"
	}
	return "API key " + e.KeyID + " is expired"
}

// Limits on names and key lifetimes.
const (
	MaxNameLength  = 64 // characters, for organization and API key names
	DefaultKeyDays = 90
	MaxKeyDays     = 365
)

// Organization is an account boundary.
type Organization struct {
	ID        string // public ID, for example org_…
	Name      string
	CreatedAt time.Time
}

// APIKey describes an API key. It never holds the key itself.
type APIKey struct {
	ID             string // public key ID, for example key_…
	OrganizationID string // the organization's public ID
	Name           string
	Scopes         []string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RevokedAt      time.Time // zero if the key isn't revoked
}

// Revoked reports whether the key is revoked.
func (k APIKey) Revoked() bool { return !k.RevokedAt.IsZero() }

// Expired reports whether the key has expired at now.
func (k APIKey) Expired(now time.Time) bool { return !now.Before(k.ExpiresAt) }

// Principal is the caller an API key identifies.
type Principal struct {
	OrgRowID       int64 // internal row number; never sent to callers
	OrganizationID string
	KeyID          string
	Scopes         []string
}

// HasScope reports whether the caller's key has scope.
func (p Principal) HasScope(scope string) bool {
	return slices.Contains(p.Scopes, scope)
}

// CheckName reports whether name is acceptable for an organization or API
// key: 1 to 64 characters, with no control characters.
func CheckName(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case !utf8.ValidString(name):
		return errors.New("name isn't valid UTF-8")
	case n == 0:
		return errors.New("name is empty")
	case n > MaxNameLength:
		return fmt.Errorf("name is longer than %d characters", MaxNameLength)
	case strings.ContainsFunc(name, unicode.IsControl):
		return errors.New("name contains a control character")
	}
	return nil
}

// CreateOrganization adds an organization.
func (s *Store) CreateOrganization(ctx context.Context, name string, now time.Time) (Organization, error) {
	if err := CheckName(name); err != nil {
		return Organization{}, err
	}
	org := Organization{ID: ids.New("org"), Name: name, CreatedAt: truncate(now)}
	_, err := s.write.ExecContext(ctx,
		"INSERT INTO organizations (public_id, name, created_at) VALUES (?, ?, ?)",
		org.ID, org.Name, FormatTime(org.CreatedAt))
	if err != nil {
		return Organization{}, fmt.Errorf("store: creating organization: %w", err)
	}
	return org, nil
}

// ListOrganizations returns every organization, oldest first.
func (s *Store) ListOrganizations(ctx context.Context) ([]Organization, error) {
	rows, err := s.read.QueryContext(ctx, "SELECT public_id, name, created_at FROM organizations ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("store: listing organizations: %w", err)
	}
	defer rows.Close()
	var orgs []Organization
	for rows.Next() {
		var org Organization
		var created string
		if err := rows.Scan(&org.ID, &org.Name, &created); err != nil {
			return nil, fmt.Errorf("store: listing organizations: %w", err)
		}
		if org.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing organizations: %w", err)
	}
	return orgs, nil
}

// NewAPIKey describes an API key to create.
type NewAPIKey struct {
	OrganizationID string   // the organization's public ID
	Name           string   // 1 to 64 characters
	Scopes         []string // at least one
	Days           int      // how many days until the key expires, 1 to 365
}

// CreateAPIKey adds an API key and returns its description and the key
// itself. The key is returned only here: Stavewell stores only its hash.
func (s *Store) CreateAPIKey(ctx context.Context, nk NewAPIKey, now time.Time) (APIKey, string, error) {
	if err := CheckName(nk.Name); err != nil {
		return APIKey{}, "", err
	}
	if nk.Days < 1 || nk.Days > MaxKeyDays {
		return APIKey{}, "", fmt.Errorf("a key must expire in 1 to %d days", MaxKeyDays)
	}
	scopes, err := normalizeScopes(nk.Scopes)
	if err != nil {
		return APIKey{}, "", err
	}

	secret := apikey.Generate()
	created := truncate(now)
	key := APIKey{
		ID:             ids.New("key"),
		OrganizationID: nk.OrganizationID,
		Name:           nk.Name,
		Scopes:         scopes,
		CreatedAt:      created,
		ExpiresAt:      created.AddDate(0, 0, nk.Days),
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("store: creating API key: %w", err)
	}
	defer tx.Rollback()

	var orgRowID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM organizations WHERE public_id = ?", nk.OrganizationID).Scan(&orgRowID)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, "", fmt.Errorf("organization %s: %w", nk.OrganizationID, ErrNotFound)
	}
	if err != nil {
		return APIKey{}, "", fmt.Errorf("store: creating API key: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO api_keys (public_id, organization_id, name, key_hash, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key.ID, orgRowID, key.Name, apikey.Hash(secret), FormatTime(key.CreatedAt), FormatTime(key.ExpiresAt))
	if err != nil {
		return APIKey{}, "", fmt.Errorf("store: creating API key: %w", err)
	}
	keyRowID, err := res.LastInsertId()
	if err != nil {
		return APIKey{}, "", fmt.Errorf("store: creating API key: %w", err)
	}
	for _, scope := range scopes {
		if _, err := tx.ExecContext(ctx, "INSERT INTO api_key_scopes (api_key_id, scope) VALUES (?, ?)", keyRowID, scope); err != nil {
			return APIKey{}, "", fmt.Errorf("store: creating API key: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return APIKey{}, "", fmt.Errorf("store: creating API key: %w", err)
	}
	return key, secret, nil
}

// normalizeScopes checks scopes and returns them without duplicates, in the
// order apikey.Scopes lists them.
func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, errors.New("a key needs at least one scope")
	}
	for _, s := range scopes {
		if !apikey.ValidScope(s) {
			return nil, fmt.Errorf("unknown scope %q (scopes are %s)", s, strings.Join(apikey.Scopes, ", "))
		}
	}
	var out []string
	for _, s := range apikey.Scopes {
		if slices.Contains(scopes, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// ListAPIKeys returns the organization's API keys, oldest first.
func (s *Store) ListAPIKeys(ctx context.Context, orgID string) ([]APIKey, error) {
	var orgRowID int64
	err := s.read.QueryRowContext(ctx, "SELECT id FROM organizations WHERE public_id = ?", orgID).Scan(&orgRowID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("organization %s: %w", orgID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: listing API keys: %w", err)
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT k.public_id, k.name, k.created_at, k.expires_at, COALESCE(k.revoked_at, ''),
		        COALESCE((SELECT group_concat(scope, ' ') FROM api_key_scopes WHERE api_key_id = k.id), '')
		 FROM api_keys k WHERE k.organization_id = ? ORDER BY k.id`, orgRowID)
	if err != nil {
		return nil, fmt.Errorf("store: listing API keys: %w", err)
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		key := APIKey{OrganizationID: orgID}
		var created, expires, revoked, scopes string
		if err := rows.Scan(&key.ID, &key.Name, &created, &expires, &revoked, &scopes); err != nil {
			return nil, fmt.Errorf("store: listing API keys: %w", err)
		}
		if key.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if key.ExpiresAt, err = parseTime(expires); err != nil {
			return nil, err
		}
		if revoked != "" {
			if key.RevokedAt, err = parseTime(revoked); err != nil {
				return nil, err
			}
		}
		key.Scopes = sortScopes(strings.Fields(scopes))
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing API keys: %w", err)
	}
	return keys, nil
}

// sortScopes puts scopes in the order apikey.Scopes lists them.
func sortScopes(scopes []string) []string {
	var out []string
	for _, s := range apikey.Scopes {
		if slices.Contains(scopes, s) {
			out = append(out, s)
		}
	}
	return out
}

// RevokeAPIKey revokes an API key. It returns ErrAlreadyRevoked, and the
// time of the earlier revocation, if the key was already revoked.
func (s *Store) RevokeAPIKey(ctx context.Context, keyID string, now time.Time) (time.Time, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: revoking API key: %w", err)
	}
	defer tx.Rollback()

	var revoked sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT revoked_at FROM api_keys WHERE public_id = ?", keyID).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("API key %s: %w", keyID, ErrNotFound)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: revoking API key: %w", err)
	}
	if revoked.Valid {
		at, err := parseTime(revoked.String)
		if err != nil {
			return time.Time{}, err
		}
		return at, ErrAlreadyRevoked
	}
	at := truncate(now)
	if _, err := tx.ExecContext(ctx, "UPDATE api_keys SET revoked_at = ? WHERE public_id = ?", FormatTime(at), keyID); err != nil {
		return time.Time{}, fmt.Errorf("store: revoking API key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, fmt.Errorf("store: revoking API key: %w", err)
	}
	return at, nil
}

// Authenticate returns the caller that key identifies at now. It returns
// ErrMalformedKey, ErrUnknownKey or an *InactiveKeyError if the key can't be
// used. It reads only the key's hash from the database.
func (s *Store) Authenticate(ctx context.Context, key string, now time.Time) (Principal, error) {
	if !apikey.Valid(key) {
		return Principal{}, ErrMalformedKey
	}
	var p Principal
	var expires string
	var revoked sql.NullString
	var scopes string
	err := s.read.QueryRowContext(ctx,
		`SELECT k.public_id, o.id, o.public_id, k.expires_at, k.revoked_at,
		        COALESCE((SELECT group_concat(scope, ' ') FROM api_key_scopes WHERE api_key_id = k.id), '')
		 FROM api_keys k JOIN organizations o ON o.id = k.organization_id
		 WHERE k.key_hash = ?`, apikey.Hash(key)).
		Scan(&p.KeyID, &p.OrgRowID, &p.OrganizationID, &expires, &revoked, &scopes)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnknownKey
	}
	if err != nil {
		return Principal{}, fmt.Errorf("store: authenticating: %w", err)
	}
	if revoked.Valid {
		return Principal{}, &InactiveKeyError{KeyID: p.KeyID, Revoked: true}
	}
	expiresAt, err := parseTime(expires)
	if err != nil {
		return Principal{}, err
	}
	if !now.Before(expiresAt) {
		return Principal{}, &InactiveKeyError{KeyID: p.KeyID}
	}
	p.Scopes = sortScopes(strings.Fields(scopes))
	return p, nil
}

// truncate drops anything finer than a millisecond, the precision Stavewell
// stores, so a returned time equals the stored one.
func truncate(t time.Time) time.Time {
	return t.UTC().Truncate(time.Millisecond)
}

// parseTime parses a stored time.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: reading stored time %q: %w", s, err)
	}
	return t, nil
}
