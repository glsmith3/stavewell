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

// revokedLeaks holds the SHA-256 hashes (apikey.Hash) of keys that reached
// the repository's history and were then revoked. It's empty, and should stay
// that way. If a key is ever committed, revoke it at once, then add its hash
// here so that TestNoKeysInHistory passes again. Never add the key itself.
var revokedLeaks = map[string]bool{}

// git runs git in dir and returns its output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %s (these tests need git): %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// repoRoot returns the repository's top directory. It also reads git's index
// and HEAD log, for go test's result cache.
//
// go test caches a passing result until the test binary, the environment or
// a file the test read changes. These tests get the tracked files and the
// history from git, which go test can't see. Adding or removing a tracked
// file changes the index, and every commit or checkout adds to the HEAD log,
// so reading both drops a cached result when either changes. CI also runs
// tests with -count=1, which never uses the cache.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("finding the repository root (this test needs git): %v", err)
	}
	dir := strings.TrimSpace(string(root))
	for _, name := range []string{"index", "logs/HEAD"} {
		path := strings.TrimSpace(git(t, dir, "rev-parse", "--path-format=absolute", "--git-path", name))
		if _, err := os.ReadFile(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("reading %s: %v", path, err)
		}
	}
	return dir
}

// TestNoKeysCommitted fails if any tracked file in the checked-out tree
// contains something shaped like an API key ("sw_" and 52 lowercase base32
// characters), other than the example key. GitHub's secret scanning doesn't
// recognize Stavewell keys, so this check does it instead.
func TestNoKeysCommitted(t *testing.T) {
	dir := repoRoot(t)
	out := []byte(git(t, dir, "ls-files", "-z"))
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

// TestNoKeysInHistory fails if any commit reachable from HEAD added a line
// containing something shaped like an API key, other than the example key
// and revoked leaks. A key that one commit adds and a later one removes still
// stays in the history, and merging a pull request brings both commits into
// the main branch. The test needs the full history: in CI, the checkout uses
// fetch-depth: 0.
func TestNoKeysInHistory(t *testing.T) {
	dir := repoRoot(t)
	if strings.TrimSpace(git(t, dir, "rev-parse", "--is-shallow-repository")) == "true" {
		t.Fatal("this is a shallow clone, so its history can't be checked. Fetch the full history with git fetch --unshallow (in CI, check out with fetch-depth: 0)")
	}
	commits := 0
	commit := ""
	for line := range strings.Lines(git(t, dir, "log", "-p", "--no-color", "--no-ext-diff", "--format=commit %H", "HEAD")) {
		if c, ok := strings.CutPrefix(line, "commit "); ok {
			commit = strings.TrimSpace(c)
			commits++
			continue
		}
		if !strings.HasPrefix(line, "+") {
			continue // only lines a commit added
		}
		for _, key := range findKeys([]byte(line)) {
			if revokedLeaks[apikey.Hash(key)] {
				continue
			}
			t.Errorf("commit %.12s added something shaped like an API key (%s…). Revoke it at once, then add its hash to revokedLeaks.", commit, key[:8])
		}
	}
	if commits < 2 {
		t.Fatalf("git log listed %d commits; want the whole history", commits)
	}
}
