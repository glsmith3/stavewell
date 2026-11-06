package ids_test

import (
	"regexp"
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
