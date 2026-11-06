// Package ids creates the random identifiers that Stavewell uses, such as
// request IDs.
package ids

import (
	"crypto/rand"
	"strings"
)

// randomLen is the number of characters after the prefix and underscore.
const randomLen = 26

// New returns prefix, an underscore and 26 random lowercase letters and
// digits from the base32 alphabet (a–z and 2–7), for example
// "req_k4vz2qn7hxwm3pt6rdyc5jbe2a". The random part carries at least 128 bits
// of randomness.
func New(prefix string) string {
	return prefix + "_" + strings.ToLower(rand.Text())
}

// Valid reports whether id has the form New gives for prefix: the prefix,
// an underscore and 26 characters from a–z and 2–7.
func Valid(prefix, id string) bool {
	random, ok := strings.CutPrefix(id, prefix+"_")
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
