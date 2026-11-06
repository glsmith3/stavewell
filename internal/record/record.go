// Package record checks record data and computes record hashes, following
// the rules in the "Record data" and "Record hashes" sections of
// api/openapi.yaml.
//
// Canonical JSON here means RFC 8785, as produced by
// encoding/json/jsontext's Value.Canonicalize. The rule on numbers keeps
// every number a whole number that a double-precision float holds exactly,
// so canonicalizing never changes a number's value.
package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on data.
const (
	// MaxDataBytes is the largest canonical form of data: 16 KiB.
	MaxDataBytes = 16 << 10
	// MaxDepth is how many levels deep data can be nested, counting data
	// itself as level 1.
	MaxDepth = 32
	// MaxNumber is the largest number allowed in data, 2⁵³−1. Its negative
	// is the smallest. It's also the largest sequence number that canonical
	// JSON writes exactly.
	MaxNumber = 1<<53 - 1
)

// ZeroHash is the previous_hash of record 1: 64 zeros.
const ZeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

// The rules data must follow. ParseData returns an error that wraps one of
// these, with the byte offset near the problem. The errors never include any
// of the data, which may be sensitive.
var (
	// ErrInvalidJSON means data isn't exactly one valid JSON value.
	ErrInvalidJSON = errors.New("data isn't valid JSON")
	// ErrNotObject means data isn't a JSON object.
	ErrNotObject = errors.New("data isn't a JSON object")
	// ErrNumber means data has a number that isn't a whole number from
	// -MaxNumber to MaxNumber written without a decimal point or exponent.
	ErrNumber = errors.New("data has a number that isn't an allowed whole number")
	// ErrDuplicateKey means an object in data has the same key twice.
	ErrDuplicateKey = errors.New("data has an object with a repeated key")
	// ErrInvalidString means data has invalid UTF-8 or an unpaired
	// surrogate escape.
	ErrInvalidString = errors.New("data has invalid UTF-8 or an unpaired surrogate escape")
	// ErrTooDeep means data is nested more than MaxDepth levels deep.
	ErrTooDeep = errors.New("data is nested too deeply")
	// ErrTooLarge means the canonical form of data is larger than
	// MaxDataBytes.
	ErrTooLarge = errors.New("data is too large")
)

// Data is record data that follows the rules, in canonical form.
type Data struct {
	canonical string
	hash      string
}

// Canonical returns the data as canonical JSON.
func (d Data) Canonical() string { return d.canonical }

// Hash returns the data's data_hash: the SHA-256 hash of its canonical JSON.
func (d Data) Hash() string { return d.hash }

// IsZero reports whether d is the zero Data, which isn't valid data.
func (d Data) IsZero() bool { return d.canonical == "" }

// ParseData checks that raw is one JSON value that follows the rules for
// record data, and returns it in canonical form.
func ParseData(raw []byte) (Data, error) {
	if err := checkStructure(raw); err != nil {
		return Data{}, err
	}
	if !utf8.Valid(raw) {
		return Data{}, fmt.Errorf("%w (near byte %d)", ErrInvalidString, invalidUTF8Offset(raw))
	}
	v := jsontext.Value(bytes.Clone(raw))
	if err := v.Canonicalize(); err != nil {
		if errors.Is(err, jsontext.ErrDuplicateName) {
			return Data{}, fmt.Errorf("%w (near byte %d)", ErrDuplicateKey, offset(err))
		}
		// The structure is valid and the bytes are valid UTF-8, so the only
		// problem left is an unpaired surrogate escape.
		return Data{}, fmt.Errorf("%w (near byte %d)", ErrInvalidString, offset(err))
	}
	if len(v) > MaxDataBytes {
		return Data{}, fmt.Errorf("%w (%d bytes in canonical form)", ErrTooLarge, len(v))
	}
	return Data{canonical: string(v), hash: hashHex(v)}, nil
}

// checkStructure checks that raw is one JSON object, nested at most MaxDepth
// levels deep, whose numbers are all allowed. Repeated keys and invalid
// strings pass here, so that ParseData can report them as such rather than
// as invalid JSON.
//
// It stops at the first level too deep, so it never reaches the JSON
// decoder's own nesting limit.
func checkStructure(raw []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(raw),
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	tok, err := dec.ReadToken()
	if err != nil {
		return fmt.Errorf("%w (near byte %d)", ErrInvalidJSON, offset(err))
	}
	if tok.Kind() != '{' {
		return fmt.Errorf("%w (near byte %d)", ErrNotObject, dec.InputOffset())
	}
	for dec.StackDepth() > 0 {
		tok, err := dec.ReadToken()
		if err != nil {
			return fmt.Errorf("%w (near byte %d)", ErrInvalidJSON, offset(err))
		}
		if dec.StackDepth() > MaxDepth {
			return fmt.Errorf("%w (near byte %d)", ErrTooDeep, dec.InputOffset())
		}
		if tok.Kind() == '0' && !allowedNumber(tok.String()) {
			return fmt.Errorf("%w (near byte %d)", ErrNumber, dec.InputOffset())
		}
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		// Something follows the object.
		return fmt.Errorf("%w (near byte %d)", ErrInvalidJSON, dec.InputOffset())
	}
	return nil
}

// allowedNumber reports whether the JSON number s is a whole number from
// -MaxNumber to MaxNumber, written without a decimal point or exponent.
func allowedNumber(s string) bool {
	if strings.ContainsAny(s, ".eE") {
		return false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && -MaxNumber <= n && n <= MaxNumber
}

// offset returns the byte offset of a JSON syntax error, or -1 if err isn't
// one. The error's own message isn't used, because it can quote the data.
func offset(err error) int64 {
	var se *jsontext.SyntacticError
	if errors.As(err, &se) {
		return se.ByteOffset
	}
	return -1
}

// invalidUTF8Offset returns the offset of the first invalid UTF-8 in b.
func invalidUTF8Offset(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}

// hashHex returns the SHA-256 hash of b as 64 lowercase hexadecimal
// characters.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Fields are the parts of a record that its record hash covers.
type Fields struct {
	Ledger       string
	Sequence     int64
	CreatedAt    string // in the API's time format, for example 2026-11-06T19:22:31.123Z
	DataHash     string
	PreviousHash string
}

// hashedFields is the object whose canonical JSON a record hash covers.
type hashedFields struct {
	CreatedAt    string `json:"created_at"`
	DataHash     string `json:"data_hash"`
	Ledger       string `json:"ledger"`
	PreviousHash string `json:"previous_hash"`
	Sequence     int64  `json:"sequence"`
}

// Hash returns the record hash for f: the SHA-256 hash of the canonical JSON
// of an object with exactly the fields created_at, data_hash, ledger,
// previous_hash and sequence.
func Hash(f Fields) (string, error) {
	if f.Sequence < 1 || f.Sequence > MaxNumber {
		return "", fmt.Errorf("record: sequence number %d is out of range", f.Sequence)
	}
	b, err := json.Marshal(hashedFields{
		CreatedAt:    f.CreatedAt,
		DataHash:     f.DataHash,
		Ledger:       f.Ledger,
		PreviousHash: f.PreviousHash,
		Sequence:     f.Sequence,
	})
	if err != nil {
		return "", fmt.Errorf("record: hashing: %w", err)
	}
	v := jsontext.Value(b)
	if err := v.Canonicalize(); err != nil {
		return "", fmt.Errorf("record: hashing: %w", err)
	}
	return hashHex(v), nil
}
