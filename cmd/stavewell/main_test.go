package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testConfig returns a configuration with a database in a temporary
// directory.
func testConfig(t *testing.T, addr string) config {
	t.Helper()
	return config{addr: addr, dbPath: filepath.Join(t.TempDir(), "stavewell.db")}
}

func TestRunReturnsErrorWhenAddressInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if err := run(context.Background(), logger, testConfig(t, ln.Addr().String())); err == nil {
		t.Fatal("run() returned nil, want an error for an address in use")
	}
	if strings.Contains(logs.String(), "listening") {
		t.Error("logged \"listening\" even though the address couldn't be bound")
	}
}

func TestRunShutsDownWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := testConfig(t, "127.0.0.1:0")
	go func() { done <- run(ctx, discardLogger(), cfg) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want nil after a graceful shutdown", err)
		}
	case <-time.After(shutdownTimeout + 5*time.Second):
		t.Fatal("run() didn't return after the context was canceled")
	}
}

func TestRunCreatesDatabase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t, "127.0.0.1:0")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	done := make(chan error, 1)
	go func() { done <- run(ctx, logger, cfg) }()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run() = %v", err)
	}
	if _, err := os.Stat(cfg.dbPath); err != nil {
		t.Errorf("database file wasn't created: %v", err)
	}
	if !strings.Contains(logs.String(), "database ready") {
		t.Errorf("log doesn't report the database as ready: %s", logs.String())
	}
}

func TestRunReturnsErrorForUnusableDatabase(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	// A directory can't be opened as a database file.
	cfg := config{addr: "127.0.0.1:0", dbPath: t.TempDir()}
	if err := run(context.Background(), logger, cfg); err == nil {
		t.Fatal("run() returned nil, want an error for a database path that's a directory")
	}
	if strings.Contains(logs.String(), "listening") {
		t.Error("logged \"listening\" even though the database couldn't be opened")
	}
}
