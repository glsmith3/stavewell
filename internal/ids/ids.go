// Package ids creates the random identifiers that Stavewell uses, such as
// request IDs.
package ids

import (
	"crypto/rand"
	"strings"
)

// New returns prefix, an underscore and 26 random lowercase letters and
// digits from the base32 alphabet (a–z and 2–7), for example
// "req_k4vz2qn7hxwm3pt6rdyc5jbe2a". The random part carries at least 128 bits
// of randomness.
func New(prefix string) string {
	return prefix + "_" + strings.ToLower(rand.Text())
}
