// Command api serves the PulseGo public HTTP API and an internal operator
// endpoint. It never runs database migrations; see cmd/migrate.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/OzanAlpkaya/pulsego/internal/config"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/security"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// Timeouts applied to every connection the public listener accepts. They are
// deliberately not configurable: they protect the process rather than express
// a policy an operator would tune per environment.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := telemetry.NewLogger(os.Stdout, cfg.LogLevel, cfg.ServiceName+"-api", cfg.Environment)
	log.Info("starting", slog.Any("config", cfg))

	// Cancelled on the first SIGINT/SIGTERM; a second signal is left to the
	// runtime so an operator can always force an exit.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metrics := telemetry.NewMetrics()

	// Tracing never dials during setup, so an unreachable collector cannot
	// delay startup; with OTEL_ENABLED=false it installs a no-op provider.
	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled:     cfg.Telemetry.OTelEnabled,
		Endpoint:    cfg.Telemetry.OTLPEndpoint,
		ServiceName: cfg.ServiceName + "-api",
		Environment: cfg.Environment,
	}, log)
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	// Deferred before the pool, so it runs after the HTTP servers have
	// drained and the pool is closed: the last spans are flushed too.
	defer flushTracing(log, shutdownTracing, cfg.ShutdownTimeout)

	pool, err := repository.NewPool(ctx, cfg.DB.URL, cfg.DB.MaxConns, telemetry.NewQueryTracer(metrics))
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	log.Info("database connected", slog.Int("max_conns", int(cfg.DB.MaxConns)))

	defaults, err := monitor.DefaultsFromDurations(cfg.Checks.DefaultInterval, cfg.Checks.DefaultTimeout)
	if err != nil {
		return fmt.Errorf("monitor defaults: %w", err)
	}

	monitors := monitor.NewService(repository.NewMonitorStore(pool), defaults,
		security.Policy{AllowPrivate: cfg.Checks.AllowPrivateTargets})

	public := newServer(cfg.Server.Addr, httpapi.NewRouter(httpapi.RouterConfig{
		APIKey:   cfg.Server.APIKey,
		Monitors: monitors,
		Pinger:   pool,
		Metrics:  metrics,
	}, log))
	internal := newServer(cfg.Server.InternalAddr, httpapi.NewInternalRouter(httpapi.InternalConfig{
		Pinger:  pool,
		Metrics: metrics.Handler(),
	}, log))

	// The group returns as soon as a listener fails or a signal arrives, at
	// which point both servers are drained with the same deadline.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error { return serve(log, "public", public) })
	g.Go(func() error { return serve(log, "internal", internal) })
	g.Go(func() error {
		<-gctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), cfg.ShutdownTimeout)
		defer cancel()

		log.Info("shutting down", slog.Duration("timeout", cfg.ShutdownTimeout))

		return errors.Join(
			shutdown(shutdownCtx, log, "public", public),
			shutdown(shutdownCtx, log, "internal", internal),
		)
	})

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

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func serve(log *slog.Logger, name string, srv *http.Server) error {
	log.Info("listening", slog.String("server", name), slog.String("addr", srv.Addr))

	// ErrServerClosed is the expected result of a graceful shutdown.
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s server: %w", name, err)
	}
	return nil
}

func shutdown(ctx context.Context, log *slog.Logger, name string, srv *http.Server) error {
	if err := srv.Shutdown(ctx); err != nil {
		// Report but do not mask other shutdown errors: in-flight requests
		// that outlive the deadline are closed by Shutdown regardless.
		log.Error("graceful shutdown failed", slog.String("server", name), slog.Any("error", err))
		return fmt.Errorf("shutdown %s server: %w", name, err)
	}

	log.Info("server stopped", slog.String("server", name))
	return nil
}
