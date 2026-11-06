package apikey_test

import (
	"strings"
	"testing"

	"github.com/glsmith3/stavewell/internal/apikey"
)

func TestGenerate(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		key := apikey.Generate()
		if !apikey.Valid(key) {
			t.Fatalf("Generate() = %q, which isn't Valid", key)
		}
		if seen[key] {
			t.Fatalf("Generate() returned %q twice", key)
		}
		seen[key] = true
	}
}

func TestValid(t *testing.T) {
	random := strings.Repeat("a", 52)
	tests := []struct {
		key  string
		want bool
	}{
		{"sw_" + random, true},
		{"sw_" + strings.Repeat("234567", 8) + "abcd", true},
		{apikey.ExampleKey, true},
		{"", false},
		{random, false},                                     // no prefix
		{"SW_" + random, false},                             // uppercase prefix
		{"sk_" + random, false},                             // wrong prefix
		{"sw_" + random[:51], false},                        // too short
		{"sw_" + random + "a", false},                       // too long
		{"sw_" + strings.ToUpper(random), false},            // uppercase
		{"sw_" + random[:51] + "1", false},                  // 1 isn't base32
		{"sw_" + random[:51] + "8", false},                  // 8 isn't base32
		{"sw_" + random[:51] + "=", false},                  // padding
		{"sw_" + random[:50] + "é", false},                  // non-ASCII
		{"Bearer sw_" + random, false},                      // whole header value
		{"sw_" + random[:26] + " " + random[:25], false},    // space
		{" sw_" + random, false},                            // leading space
		{"sw_" + random[:51] + "\n", false},                 // trailing newline
		{"sw_" + random[:25] + "\x00" + random[:26], false}, // NUL
	}
	for _, tt := range tests {
		if got := apikey.Valid(tt.key); got != tt.want {
			t.Errorf("Valid(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestHash(t *testing.T) {
	// The expected value was computed independently, with Python's hashlib.
	const want = "3d78406771b21f466d58da4b89e5baac9b7548967152be0ef57afa51df0c5c1d"
	if got := apikey.Hash(apikey.ExampleKey); got != want {
		t.Errorf("Hash(ExampleKey) = %s, want %s", got, want)
	}
}

func TestLooksLikeKey(t *testing.T) {
	key := apikey.Generate()
	for _, s := range []string{
		key,
		key + " ",
		"Bearer " + key,
		"STAVEWELL_KEY=" + key,
		strings.ToUpper(key),
		key[:len(key)-1], // one character short
		key[:3+16],       // the prefix and 16 characters
		apikey.ExampleKey,
		key[3:],                           // no prefix
		strings.ToUpper(key[3:]),          // no prefix, uppercase
		"sw-" + key[3:],                   // wrong separator
		`sw\_` + key[3:],                  // copied from escaped Markdown
		"name " + key[3:3+32] + " suffix", // 32 characters, no prefix
	} {
		if !apikey.LooksLikeKey(s) {
			t.Errorf("LooksLikeKey(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"",
		"key_abprqbbshjploerfcxoy6stzbw",
		"org_ngztvgyskawogjffukqizkghrh",
		"Local testing",
		key[:3+15], // the prefix and only 15 characters
		"sw_" + strings.Repeat("1", 52),
		key[3 : 3+31],               // 31 characters, no prefix
		"Internationalization test", // a long word is under 32 letters
	} {
		if apikey.LooksLikeKey(s) {
			t.Errorf("LooksLikeKey(%q) = true, want false", s)
		}
	}
}

func TestValidScope(t *testing.T) {
	for _, s := range []string{"ledger:read", "ledger:write"} {
		if !apikey.ValidScope(s) {
			t.Errorf("ValidScope(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "ledger", "ledger:admin", "Ledger:read", "ledger:read "} {
		if apikey.ValidScope(s) {
			t.Errorf("ValidScope(%q) = true, want false", s)
		}
	}
}
