// Command stavewell runs the Stavewell API server.
//
// Configuration comes from environment variables:
//
//	STAVEWELL_ADDR  Address to listen on. Default: 127.0.0.1:8080
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/glsmith3/stavewell/internal/buildinfo"
	"github.com/glsmith3/stavewell/internal/server"
)

// defaultAddr listens on the local machine only. Public traffic reaches the
// server through a tunnel, never through an open port.
const defaultAddr = "127.0.0.1:8080"

// shutdownTimeout is how long in-flight requests get to finish after a
// shutdown signal.
const shutdownTimeout = 10 * time.Second

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

	addr := os.Getenv("STAVEWELL_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	if err := run(ctx, logger, addr); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// run serves the API on addr until ctx is canceled, then shuts down
// gracefully. It returns an error if the address can't be bound or the
// server fails.
func run(ctx context.Context, logger *slog.Logger, addr string) error {
	ln, err := net.Listen("tcp", addr)
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
