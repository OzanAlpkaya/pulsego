// Command worker runs the claim-and-check loop: it claims due monitors with
// time-bounded leases, checks them with bounded concurrency, records the
// results, and drains in-flight checks on SIGINT/SIGTERM. Next to it, a
// retention runner deletes check history older than CHECK_RETENTION_DAYS.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/checker"
	"github.com/OzanAlpkaya/pulsego/internal/config"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
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

	// The scheduler adds worker_id itself, so it gets the base logger.
	baseLog := telemetry.NewLogger(os.Stdout, cfg.LogLevel, cfg.ServiceName+"-worker", cfg.Environment)
	log := baseLog.With(slog.String("worker_id", workerID))
	log.Info("starting", slog.Any("config", cfg))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := repository.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	log.Info("database connected", slog.Int("max_conns", int(cfg.DB.MaxConns)))

	store := repository.NewSchedulerStore(pool)
	chk := checker.New()
	defer chk.CloseIdleConnections()

	// Retention runs next to the scheduler on the same signal context. It
	// adds worker_id to its logs, so concurrent cleaners can be told apart.
	var retentionDone sync.WaitGroup
	retention := scheduler.NewRetention(repository.NewRetentionStore(pool), cfg.Checks.RetentionDays, log)
	retentionDone.Go(func() { retention.Run(ctx) })

	// Returns once the signal context is cancelled and every in-flight check
	// has finished or, after cfg.ShutdownTimeout, been cancelled and released.
	scheduler.New(store, chk, scheduler.Config{
		WorkerID:        workerID,
		Concurrency:     cfg.Worker.Concurrency,
		BatchSize:       cfg.Worker.BatchSize,
		PollInterval:    cfg.Worker.PollInterval,
		LeaseDuration:   cfg.Worker.LeaseDuration,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, baseLog).Run(ctx)

	// The retention runner returns as soon as the signal context is done; a
	// DELETE in flight is cancelled and rolls back. Wait for it before the
	// deferred pool.Close.
	retentionDone.Wait()

	log.Info("stopped")
	return nil
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
