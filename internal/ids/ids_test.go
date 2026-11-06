package ids_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/glsmith3/stavewell/internal/ids"
)

func TestNewFormat(t *testing.T) {
	format := regexp.MustCompile(`^req_[a-z2-7]{26}$`)
	for range 100 {
		if id := ids.New("req"); !format.MatchString(id) {
			t.Fatalf("New(\"req\") = %q, want a match for %s", id, format)
		}
	}
}

func TestValid(t *testing.T) {
	for range 100 {
		if id := ids.New("org"); !ids.Valid("org", id) {
			t.Fatalf("Valid(\"org\", %q) = false for a generated ID", id)
		}
	}
	good := "org_" + strings.Repeat("a", 26)
	for _, tt := range []struct {
		prefix, id string
		want       bool
	}{
		{"org", good, true},
		{"key", "key_" + strings.Repeat("234567", 4) + "ab", true},
		{"key", good, false},                             // wrong prefix
		{"org", good[:29], false},                        // too short
		{"org", good + "a", false},                       // too long
		{"org", strings.ToUpper(good), false},            // uppercase
		{"org", good[:29] + "1", false},                  // 1 isn't base32
		{"org", " " + good, false},                       // leading space
		{"org", "org-" + strings.Repeat("a", 26), false}, // wrong separator
		{"org", "", false},
	} {
		if got := ids.Valid(tt.prefix, tt.id); got != tt.want {
			t.Errorf("Valid(%q, %q) = %v, want %v", tt.prefix, tt.id, got, tt.want)
		}
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 10000 {
		id := ids.New("req")
		if seen[id] {
			t.Fatalf("New returned %q twice", id)
		}
		seen[id] = true
	}
}
