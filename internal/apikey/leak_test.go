package apikey_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/glsmith3/stavewell/internal/apikey"
)

// keyPattern matches anything shaped like an API key.
var keyPattern = regexp.MustCompile(`sw_[a-z2-7]{52}`)

// findKeys returns the key-shaped strings in b, other than the example key.
func findKeys(b []byte) []string {
	var found []string
	for _, m := range keyPattern.FindAll(b, -1) {
		if string(m) != apikey.ExampleKey {
			found = append(found, string(m))
		}
	}
	return found
}

func TestFindKeys(t *testing.T) {
	real := apikey.Generate()
	text := []byte("docs use " + apikey.ExampleKey + ", but someone pasted " + real + " here")
	got := findKeys(text)
	if len(got) != 1 || got[0] != real {
		t.Errorf("findKeys() = %q, want only %q", got, real)
	}
}

// TestNoKeysCommitted fails if any file in the repository contains something
// shaped like an API key, other than the example key. GitHub's secret
// scanning doesn't recognize Stavewell keys, so this check does it instead.
func TestNoKeysCommitted(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("finding the repository root (this test needs git): %v", err)
	}
	dir := strings.TrimSpace(string(root))
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("listing files: %v", err)
	}
	files := bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0})
	if len(files) < 10 {
		t.Fatalf("git ls-files found only %d files; want the whole repository", len(files))
	}
	for _, name := range files {
		path := filepath.Join(dir, string(name))
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue // deleted in the working tree but not yet committed
		}
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for _, key := range findKeys(b) {
			// Show only the start of the key, so the test output doesn't
			// spread it further.
			t.Errorf("%s contains something shaped like an API key (%s…). Revoke it, then remove it.", name, key[:8])
		}
	}
}
