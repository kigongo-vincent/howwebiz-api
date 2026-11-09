package main

import (
	"context"
	"log"
	"log/slog"
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
	db := config.ConnectDB()

	// 2. Services, providers and the catalog; scheduler + indexer run in
	// the background for the server's lifetime
	deps := config.Wire(ctx, db, app, true)
	config.RegisterAppRoutes(deps)

	// 3. Serve until SIGINT/SIGTERM, then drain
	port := base.Env("PORT", "3000")
	go func() {
		if err := app.Listen(":" + port); err != nil {
			log.Fatalf("failed to start server: %v", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
		slog.Error("http shutdown", "err", err)
	}
	// ctx is cancelled, so in-flight crawls stop and record their result
	deps.Wait()
}
