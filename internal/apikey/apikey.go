// Package apikey creates and checks Stavewell API keys.
//
// A key is "sw_" followed by 52 lowercase letters and digits from the base32
// alphabet (a–z and 2–7), encoding 256 random bits. Stavewell stores only the
// key's SHA-256 hash. A slow password hash isn't needed: a 256-bit random key
// can't be guessed, so a fast hash is just as safe.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// Prefix starts every API key.
const Prefix = "sw_"

// randomLen is the number of base32 characters after the prefix.
const randomLen = 52

// ExampleKey is the key that documentation and tests use as a placeholder.
// It's correctly formed, but no real key is ever this value.
const ExampleKey = "sw_exampleexampleexampleexampleexampleexampleexamplekey"

// The scopes an API key can have.
const (
	ScopeLedgerRead  = "ledger:read"
	ScopeLedgerWrite = "ledger:write"
)

// Scopes lists every scope, in the order they're displayed.
var Scopes = []string{ScopeLedgerRead, ScopeLedgerWrite}

// ValidScope reports whether s is a known scope.
func ValidScope(s string) bool {
	for _, scope := range Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Generate returns a new random API key.
func Generate() string {
	b := make([]byte, 32)
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b)
	return Prefix + strings.ToLower(encoding.EncodeToString(b))
}

// Valid reports whether key has the form of an API key. It doesn't say
// whether the key exists.
func Valid(key string) bool {
	random, ok := strings.CutPrefix(key, Prefix)
	if !ok || len(random) != randomLen {
		return false
	}
	for _, c := range random {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// Hash returns the SHA-256 hash of key, as 64 lowercase hexadecimal
// characters. This is the only form in which a key is stored.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
