// Command worker runs the claim-and-check loop. Sprint 1 establishes the
// process skeleton — configuration, logging, database pool, ticker, and
// graceful shutdown — while the claim transaction and the HTTP checker are
// added in Sprint 3.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OzanAlpkaya/pulsego/internal/config"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Each worker instance needs an identity that is unique across replicas
	// and across restarts, because it becomes the monitors.lease_owner value.
	workerID := newWorkerID()

	log := telemetry.NewLogger(os.Stdout, cfg.LogLevel, cfg.ServiceName+"-worker", cfg.Environment).
		With(slog.String("worker_id", workerID))
	log.Info("starting", slog.Any("config", cfg))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := repository.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	log.Info("database connected", slog.Int("max_conns", int(cfg.DB.MaxConns)))

	// Returns once the signal context is cancelled. Sprint 3 adds the drain
	// of in-flight checks here, bounded by cfg.ShutdownTimeout; with no work
	// in flight yet there is nothing to wait for, and waiting anyway would
	// only delay every restart.
	loop(ctx, log, cfg, pool)

	log.Info("stopped")
	return nil
}

// loop polls for due monitors until the context is cancelled.
func loop(ctx context.Context, log *slog.Logger, cfg config.Config, pool *pgxpool.Pool) {
	ticker := time.NewTicker(cfg.Worker.PollInterval)
	defer ticker.Stop()

	log.Info("poll loop started",
		slog.Duration("interval", cfg.Worker.PollInterval),
		slog.Int("concurrency", cfg.Worker.Concurrency),
		slog.Int("batch_size", cfg.Worker.BatchSize),
		slog.Duration("lease_duration", cfg.Worker.LeaseDuration),
	)

	for {
		select {
		case <-ctx.Done():
			log.Info("poll loop stopping")
			return
		case <-ticker.C:
			// Sprint 3 replaces this with the FOR UPDATE SKIP LOCKED claim
			// transaction followed by a bounded-concurrency check batch.
			if err := pool.Ping(ctx); err != nil {
				log.Error("database unreachable", slog.Any("error", err))
				continue
			}
			log.Debug("poll tick")
		}
	}
}

// newWorkerID builds an identifier that ties a lease back to a specific
// process: the hostname makes it readable in the database, the UUID suffix
// keeps two restarts of the same container distinct.
func newWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + uuid.NewString()
}
