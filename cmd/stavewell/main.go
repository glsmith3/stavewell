// Command stavewell runs the Stavewell API server, or, with "admin"
// arguments, an admin command that manages organizations and API keys. Run
// "stavewell help" for the list of commands.
//
// Configuration comes from environment variables:
//
//	STAVEWELL_ADDR  Address to listen on. Default: 127.0.0.1:8080
//	STAVEWELL_DB    Path of the SQLite database file. Default: stavewell.db
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/glsmith3/stavewell/internal/apikey"
	"github.com/glsmith3/stavewell/internal/buildinfo"
	"github.com/glsmith3/stavewell/internal/server"
	"github.com/glsmith3/stavewell/internal/store"
)

// defaultAddr listens on the local machine only. Public traffic reaches the
// server through a tunnel, never through an open port.
const defaultAddr = "127.0.0.1:8080"

// defaultDBPath is the database file, in the current directory.
const defaultDBPath = "stavewell.db"

// shutdownTimeout is how long in-flight requests get to finish after a
// shutdown signal.
const shutdownTimeout = 10 * time.Second

// startupTimeout is how long opening the database may take, including
// waiting for another process that holds its write lock.
const startupTimeout = 30 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, restore default signal handling, so a second
	// signal stops the process immediately instead of waiting for shutdown.
	go func() {
		<-ctx.Done()
		stop()
	}()

	cfg := config{addr: os.Getenv("STAVEWELL_ADDR"), dbPath: os.Getenv("STAVEWELL_DB")}
	if cfg.addr == "" {
		cfg.addr = defaultAddr
	}
	if cfg.dbPath == "" {
		cfg.dbPath = defaultDBPath
	}

	// With arguments, run an admin command instead of the server.
	if args := os.Args[1:]; len(args) > 0 {
		c := command{ctx: ctx, stdout: os.Stdout, stderr: os.Stderr, dbPath: cfg.dbPath, now: time.Now}
		code := exitUsage
		switch {
		case slices.ContainsFunc(args, apikey.LooksLikeKey):
			fmt.Fprintf(c.stderr, "stavewell: %v\n", errKeyInArgs)
			code = exitError
		case args[0] == "admin":
			code = runAdmin(c, args[1:])
		case isHelp(args[0]):
			fmt.Fprint(c.stdout, usage)
			code = exitOK
		default:
			_ = c.usageError(fmt.Sprintf("unknown command %q", args[0]))
		}
		stop()
		os.Exit(code)
	}

	if err := run(ctx, logger, cfg); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// config is the server's configuration.
type config struct {
	addr   string // address to listen on
	dbPath string // path of the database file
}

// run opens the database, then serves the API until ctx is canceled, then
// shuts down gracefully. It returns an error if the database can't be
// opened, the address can't be bound or the server fails.
func run(ctx context.Context, logger *slog.Logger, cfg config) (err error) {
	// Startup has its own deadline. A shutdown signal that arrives during
	// startup takes effect once startup finishes.
	startCtx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	db, err := store.Open(startCtx, cfg.dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	version, err := db.SchemaVersion(startCtx)
	if err != nil {
		return err
	}
	logger.Info("database ready", "schema_version", version)

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler: server.Handler(logger),
		// Route net/http's own messages through the structured logger.
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10, // 16 KiB
	}

	logger.Info("listening",
		"addr", ln.Addr().String(),
		"version", buildinfo.Version,
		"commit", buildinfo.Commit(),
	)

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()

	select {
	case err := <-errs:
		// Serve returns only on failure here, because Shutdown hasn't been
		// called yet.
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
