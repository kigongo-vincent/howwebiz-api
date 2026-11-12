package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // CRON_TZ works in minimal images

	"github.com/kigongo-vincent/howwebiz-api/config"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 1. Build the application setup
	app := config.SetupApplication()
	db := config.ConnectDB()         // crawlers, ingest, upkeep
	apiDB := config.ConnectAPIDB(db) // HTTP requests only: never starved by the above

	// 2. Services, providers and the catalog; scheduler + indexer run in
	// the background for the server's lifetime
	deps := config.WireAPI(ctx, db, apiDB, app, true)
	config.RegisterAppRoutes(deps)

	// 3. Serve until SIGINT/SIGTERM, then drain
	port := base.Env("PORT", "3000")
	go func() {
		if err := app.Listen(":" + port); err != nil {
			log.Fatalf("failed to start server: %v", err)
		}
	}()

	<-ctx.Done()
	stop() // a second SIGINT/SIGTERM now ends the process at once
	go func() {
		force := make(chan os.Signal, 1)
		signal.Notify(force, syscall.SIGINT, syscall.SIGTERM)
		<-force
		slog.Warn("second signal: exiting now (running crawls resume on next start)")
		os.Exit(1)
	}()

	// Safe restart. ctx is cancelled, so every crawl stops at its next safe
	// point (between pages; each page's songs and cursor are already
	// stored) and is recorded as interrupted with its options, to be resumed
	// on the next start. Meanwhile HTTP stops accepting connections and
	// finishes the requests in flight.
	grace := base.EnvDuration("SHUTDOWN_GRACE", 30*time.Second)
	slog.Info("shutting down", "grace", grace)
	httpDone := make(chan struct{})
	go func() {
		if err := app.ShutdownWithTimeout(min(grace, 15*time.Second)); err != nil {
			slog.Error("http shutdown", "err", err)
		}
		close(httpDone)
	}()
	if deps.WaitFor(grace) {
		slog.Info("crawls stopped cleanly")
	} else {
		slog.Warn("crawls still running after grace; recorded as interrupted, resume on next start")
	}
	<-httpDone
	if deps.Cache != nil {
		_ = deps.Cache.Close()
	}
	slog.Info("shutdown complete")
}
