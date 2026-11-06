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
//
// go test caches a passing result until the test binary, the environment or
// a file the test read changes. The list of tracked files comes from running
// git, which go test can't see, so the test also reads git's index file:
// adding or removing a tracked file changes the index, and with it the
// cached result. CI also runs tests with -count=1, which never uses the
// cache.
func TestNoKeysCommitted(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("finding the repository root (this test needs git): %v", err)
	}
	dir := strings.TrimSpace(string(root))
	index, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-path", "index").Output()
	if err != nil {
		t.Fatalf("finding the git index: %v", err)
	}
	if _, err := os.ReadFile(strings.TrimSpace(string(index))); err != nil {
		t.Fatalf("reading the git index: %v", err)
	}
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
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue // deleted in the working tree, or a broken symlink
		}
		if err != nil {
			t.Fatalf("checking %s: %v", name, err)
		}
		if info.IsDir() {
			continue // a submodule, or a symlink to a directory
		}
		b, err := os.ReadFile(path)
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
