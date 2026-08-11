// Command deployer is the Deployer control plane: MCP server, upload endpoint,
// and deployment reconcile loop in one binary. See docs/specs/0001-stack-and-architecture.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/toyinogun/deployer/internal/config"
	"github.com/toyinogun/deployer/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("shutting down", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

	// Migrations run at startup, before anything can serve, so a boot on an empty
	// volume produces a migrated database rather than a running binary with no
	// tables (spec 0002).
	st, err := store.Open(store.Options{Path: cfg.DBPath, BusyTimeout: cfg.DBBusyTimeout})
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			slog.Error("closing the database failed", "error", err)
		}
	}()
	migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelMigrate()
	if err := st.Migrate(migrateCtx); err != nil {
		return err
	}
	slog.Info("database ready", "path", cfg.DBPath, "pod", cfg.PodName)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("ok\n")); err != nil {
			slog.WarnContext(r.Context(), "healthz write failed", "error", err)
		}
	})

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// Recreate strategy means Kubernetes waits for this pod to exit before
	// starting the next one, so a clean shutdown is what keeps the swap quiet.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// A shutdown that times out means connections were still in flight. Log it
		// rather than dropping it: it is the difference between a clean pod swap
		// and one that cut a deploy off mid request.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
	}()

	slog.Info("deployer listening", "addr", cfg.Listen, "app_domain", cfg.AppDomain)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
