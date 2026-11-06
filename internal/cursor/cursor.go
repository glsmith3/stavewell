// Package cursor encodes and decodes the opaque positions that list
// endpoints return as next_cursor.
//
// A cursor holds only values the caller already knows: which list it belongs
// to, the organization's public ID and the last item returned. It never holds
// internal row numbers, which would reveal how many organizations or ledgers
// exist. Cursors aren't signed: a caller who edits one can only point it at a
// position in their own organization's list, which they could reach anyway,
// because the server takes the organization from the API key and checks the
// cursor against it.
package cursor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// version is the cursor format version. A later format uses a new version,
// so that cursors saved by callers keep working or fail clearly.
const version = 1

// maxLen is the longest cursor accepted, which is well over any real one.
const maxLen = 512

// The lists a cursor can belong to.
const (
	ListLedgers = "ledgers"
)

// ErrInvalid means a cursor is malformed, or belongs to a different list or
// organization.
var ErrInvalid = errors.New("invalid cursor")

// Position is where a page of a list starts.
type Position struct {
	List           string // which list, for example ListLedgers
	OrganizationID string // the organization's public ID
	After          string // the last item already returned; empty for the start
}

// payload is the encoded form of a Position.
type payload struct {
	V int    `json:"v"`
	L string `json:"l"`
	O string `json:"o"`
	A string `json:"a"`
}

var encoding = base64.RawURLEncoding

// Encode returns the cursor for p.
func Encode(p Position) string {
	// Marshalling can't fail: every field is a string or an int.
	b, _ := json.Marshal(payload{V: version, L: p.List, O: p.OrganizationID, A: p.After})
	return encoding.EncodeToString(b)
}

// Decode returns the position in cursor c. It returns ErrInvalid unless c is a
// cursor Encode made for the given list and organization.
func Decode(c, list, orgID string) (Position, error) {
	if c == "" || len(c) > maxLen {
		return Position{}, ErrInvalid
	}
	b, err := encoding.DecodeString(c)
	if err != nil {
		return Position{}, ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p payload
	if err := dec.Decode(&p); err != nil || dec.More() {
		return Position{}, ErrInvalid
	}
	if p.V != version || p.L != list || p.O != orgID {
		return Position{}, ErrInvalid
	}
	// Re-encoding must give the same cursor, so that only cursors Encode
	// made are accepted, not other spellings of the same values.
	pos := Position{List: p.L, OrganizationID: p.O, After: p.A}
	if Encode(pos) != c {
		return Position{}, ErrInvalid
	}
	return pos, nil
}
