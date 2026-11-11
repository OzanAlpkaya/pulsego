// Command worker runs the claim-and-check loop: it claims due monitors with
// time-bounded leases, checks them with bounded concurrency, records the
// results, and drains in-flight checks on SIGINT/SIGTERM. Next to it, a
// retention runner deletes check history older than CHECK_RETENTION_DAYS, and
// an internal HTTP server on INTERNAL_HTTP_ADDR serves /healthz, /readyz and
// /metrics.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/OzanAlpkaya/pulsego/internal/checker"
	"github.com/OzanAlpkaya/pulsego/internal/config"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
	"github.com/OzanAlpkaya/pulsego/internal/security"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// Timeouts of the internal listener. It serves probes and scrapes only, so
// they are not configurable.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
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

	metrics := telemetry.NewMetrics()

	// Tracing never dials during setup, so an unreachable collector cannot
	// delay startup; with OTEL_ENABLED=false it installs a no-op provider.
	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled:     cfg.Telemetry.OTelEnabled,
		Endpoint:    cfg.Telemetry.OTLPEndpoint,
		ServiceName: cfg.ServiceName + "-worker",
		Environment: cfg.Environment,
	}, log)
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	// Deferred before the pool, so it runs last: after the scheduler has
	// drained, the internal server has stopped and the pool is closed.
	defer flushTracing(log, shutdownTracing, cfg.ShutdownTimeout)

	pool, err := repository.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns, telemetry.NewQueryTracer(metrics))
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	log.Info("database connected", slog.Int("max_conns", int(cfg.DB.MaxConns)))

	// Listen before any check starts, so a taken or invalid address stops
	// the process at once with a clear error instead of running unobserved.
	internal := newServer(httpapi.NewInternalRouter(httpapi.InternalConfig{
		Pinger:  pool,
		Metrics: metrics.Handler(),
	}, log))
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.Server.InternalAddr)
	if err != nil {
		return fmt.Errorf("internal server: listen on %s: %w", cfg.Server.InternalAddr, err)
	}

	store := repository.NewSchedulerStore(pool)
	chk := checker.New(security.Policy{AllowPrivate: cfg.Checks.AllowPrivateTargets})
	defer chk.CloseIdleConnections()

	sched := scheduler.New(store, chk, scheduler.Config{
		WorkerID:        workerID,
		Concurrency:     cfg.Worker.Concurrency,
		BatchSize:       cfg.Worker.BatchSize,
		PollInterval:    cfg.Worker.PollInterval,
		LeaseDuration:   cfg.Worker.LeaseDuration,
		ShutdownTimeout: cfg.ShutdownTimeout,
		Metrics:         metrics,
	}, baseLog)

	// Retention adds worker_id to its logs, so concurrent cleaners can be
	// told apart.
	retention := scheduler.NewRetention(repository.NewRetentionStore(pool), cfg.Checks.RetentionDays, log)

	// gctx is cancelled by the signal or by a failing internal server; either
	// way the scheduler drains first and the internal server stops after it,
	// so /readyz and /metrics stay up while in-flight checks finish.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		log.Info("listening", slog.String("server", "internal"), slog.String("addr", ln.Addr().String()))
		// ErrServerClosed is the expected result of a graceful shutdown.
		if err := internal.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("internal server failed", slog.Any("error", err))
			return fmt.Errorf("internal server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		// Returns as soon as gctx is done; a DELETE in flight is cancelled
		// and rolls back.
		retention.Run(gctx)
		return nil
	})
	g.Go(func() error {
		// Returns once gctx is cancelled and every in-flight check has
		// finished or, after cfg.ShutdownTimeout, been cancelled and released.
		sched.Run(gctx)

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), cfg.ShutdownTimeout)
		defer cancel()
		if err := internal.Shutdown(shutdownCtx); err != nil {
			log.Error("graceful shutdown failed", slog.String("server", "internal"), slog.Any("error", err))
			return fmt.Errorf("shutdown internal server: %w", err)
		}
		log.Info("server stopped", slog.String("server", "internal"))
		return nil
	})

	// Every goroutine has returned before the deferred pool.Close runs.
	if err := g.Wait(); err != nil {
		return err
	}

	log.Info("stopped")
	return nil
}

// flushTracing exports pending spans within SHUTDOWN_TIMEOUT. A failure is
// logged, never returned: losing the last spans must not turn a clean stop
// into a failed one.
func flushTracing(log *slog.Logger, shutdown func(context.Context) error, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := shutdown(ctx); err != nil {
		log.Warn("tracing shutdown failed", slog.Any("error", err))
	}
}

func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
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
