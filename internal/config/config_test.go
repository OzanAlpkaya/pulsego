package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/config"
)

// validEnv is the smallest environment that loads successfully. Tests copy it
// and change only the variable under test.
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://pulsego:secret@localhost:5432/pulsego?sslmode=disable",
		"API_KEY":      "0123456789abcdef",
	}
}

// lookupFrom turns a map into a config.LookupFunc, so no test touches the
// process environment and every test can run in parallel.
func lookupFrom(env map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoadFromDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadFrom(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("LoadFrom() returned an unexpected error: %v", err)
	}

	if got, want := cfg.ServiceName, "pulsego"; got != want {
		t.Errorf("ServiceName = %q, want %q", got, want)
	}
	if got, want := cfg.Environment, config.EnvDevelopment; got != want {
		t.Errorf("Environment = %q, want %q", got, want)
	}
	if got, want := cfg.LogLevel, slog.LevelInfo; got != want {
		t.Errorf("LogLevel = %v, want %v", got, want)
	}
	if got, want := cfg.ShutdownTimeout, 15*time.Second; got != want {
		t.Errorf("ShutdownTimeout = %v, want %v", got, want)
	}
	if got, want := cfg.Server.Addr, ":8080"; got != want {
		t.Errorf("Server.Addr = %q, want %q", got, want)
	}
	if got, want := cfg.Server.InternalAddr, ":9090"; got != want {
		t.Errorf("Server.InternalAddr = %q, want %q", got, want)
	}
	if got, want := cfg.Worker.Concurrency, 10; got != want {
		t.Errorf("Worker.Concurrency = %d, want %d", got, want)
	}
	if got, want := cfg.Worker.BatchSize, 20; got != want {
		t.Errorf("Worker.BatchSize = %d, want %d", got, want)
	}
	if got, want := cfg.Worker.PollInterval, 5*time.Second; got != want {
		t.Errorf("Worker.PollInterval = %v, want %v", got, want)
	}
	if got, want := cfg.Worker.LeaseDuration, 60*time.Second; got != want {
		t.Errorf("Worker.LeaseDuration = %v, want %v", got, want)
	}
	if got, want := cfg.Checks.DefaultInterval, 60*time.Second; got != want {
		t.Errorf("Checks.DefaultInterval = %v, want %v", got, want)
	}
	if got, want := cfg.Checks.DefaultTimeout, 5*time.Second; got != want {
		t.Errorf("Checks.DefaultTimeout = %v, want %v", got, want)
	}
	if cfg.Checks.AllowPrivateTargets {
		t.Error("Checks.AllowPrivateTargets = true, want false by default")
	}
	if got, want := cfg.Checks.RetentionDays, 30; got != want {
		t.Errorf("Checks.RetentionDays = %d, want %d", got, want)
	}
	if cfg.Telemetry.OTelEnabled {
		t.Error("Telemetry.OTelEnabled = true, want false by default")
	}
}

func TestLoadFromOverrides(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env["SERVICE_NAME"] = "pulsego-staging"
	env["ENVIRONMENT"] = config.EnvStaging
	env["LOG_LEVEL"] = "debug"
	env["HTTP_ADDR"] = "127.0.0.1:8000"
	env["INTERNAL_HTTP_ADDR"] = "127.0.0.1:9000"
	env["WORKER_CONCURRENCY"] = "32"
	env["WORKER_BATCH_SIZE"] = "50"
	env["WORKER_POLL_INTERVAL"] = "2s"
	env["WORKER_LEASE_DURATION"] = "90s"
	env["DEFAULT_CHECK_INTERVAL"] = "30s"
	env["DEFAULT_CHECK_TIMEOUT"] = "10s"
	env["CHECK_RETENTION_DAYS"] = "7"
	env["SHUTDOWN_TIMEOUT"] = "30s"
	env["OTEL_ENABLED"] = "true"
	env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://otel-collector:4317"

	cfg, err := config.LoadFrom(lookupFrom(env))
	if err != nil {
		t.Fatalf("LoadFrom() returned an unexpected error: %v", err)
	}

	if got, want := cfg.LogLevel, slog.LevelDebug; got != want {
		t.Errorf("LogLevel = %v, want %v", got, want)
	}
	if got, want := cfg.Worker.Concurrency, 32; got != want {
		t.Errorf("Worker.Concurrency = %d, want %d", got, want)
	}
	if got, want := cfg.Worker.LeaseDuration, 90*time.Second; got != want {
		t.Errorf("Worker.LeaseDuration = %v, want %v", got, want)
	}
	if got, want := cfg.Checks.DefaultTimeout, 10*time.Second; got != want {
		t.Errorf("Checks.DefaultTimeout = %v, want %v", got, want)
	}
	if !cfg.Telemetry.OTelEnabled {
		t.Error("Telemetry.OTelEnabled = false, want true")
	}
}

// TestLoadFromInvalid covers one rejection per validation rule. Each case
// asserts on a distinctive fragment of the message so that a regression in the
// wording is caught too — an unclear error is the failure mode this package
// exists to prevent.
func TestLoadFromInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(map[string]string)
		wantText string
	}{
		{
			name:     "database url missing",
			mutate:   func(env map[string]string) { delete(env, "DATABASE_URL") },
			wantText: "DATABASE_URL is required",
		},
		{
			name:     "database url empty",
			mutate:   func(env map[string]string) { env["DATABASE_URL"] = "   " },
			wantText: "DATABASE_URL is required",
		},
		{
			name:     "database url wrong scheme",
			mutate:   func(env map[string]string) { env["DATABASE_URL"] = "mysql://localhost:3306/pulsego" },
			wantText: "must use the postgres:// or postgresql:// scheme",
		},
		{
			name:     "database url without host",
			mutate:   func(env map[string]string) { env["DATABASE_URL"] = "postgres:///pulsego" },
			wantText: "DATABASE_URL must include a host",
		},
		{
			name:     "api key missing",
			mutate:   func(env map[string]string) { delete(env, "API_KEY") },
			wantText: "API_KEY is required",
		},
		{
			name:     "api key too short",
			mutate:   func(env map[string]string) { env["API_KEY"] = "short" },
			wantText: "API_KEY must be at least 16 characters",
		},
		{
			name:     "environment unknown",
			mutate:   func(env map[string]string) { env["ENVIRONMENT"] = "qa" },
			wantText: `ENVIRONMENT must be one of development, staging, production, got "qa"`,
		},
		{
			name:     "log level unknown",
			mutate:   func(env map[string]string) { env["LOG_LEVEL"] = "verbose" },
			wantText: "LOG_LEVEL must be one of debug, info, warn, error",
		},
		{
			name:     "worker concurrency not an integer",
			mutate:   func(env map[string]string) { env["WORKER_CONCURRENCY"] = "many" },
			wantText: `WORKER_CONCURRENCY must be an integer, got "many"`,
		},
		{
			name:     "worker concurrency below minimum",
			mutate:   func(env map[string]string) { env["WORKER_CONCURRENCY"] = "0" },
			wantText: "WORKER_CONCURRENCY must be between 1 and 256, got 0",
		},
		{
			name:     "worker concurrency above maximum",
			mutate:   func(env map[string]string) { env["WORKER_CONCURRENCY"] = "1000" },
			wantText: "WORKER_CONCURRENCY must be between 1 and 256, got 1000",
		},
		{
			name:     "poll interval unparseable",
			mutate:   func(env map[string]string) { env["WORKER_POLL_INTERVAL"] = "5 seconds" },
			wantText: "WORKER_POLL_INTERVAL must be a duration",
		},
		{
			name:     "check interval below domain minimum",
			mutate:   func(env map[string]string) { env["DEFAULT_CHECK_INTERVAL"] = "1s" },
			wantText: "DEFAULT_CHECK_INTERVAL must be between 10s and 24h0m0s",
		},
		{
			name:     "check timeout above domain maximum",
			mutate:   func(env map[string]string) { env["DEFAULT_CHECK_TIMEOUT"] = "5m" },
			wantText: "DEFAULT_CHECK_TIMEOUT must be between 100ms and 1m0s",
		},
		{
			name:     "retention days out of range",
			mutate:   func(env map[string]string) { env["CHECK_RETENTION_DAYS"] = "400" },
			wantText: "CHECK_RETENTION_DAYS must be between 1 and 365, got 400",
		},
		{
			name:     "allow private targets not a boolean",
			mutate:   func(env map[string]string) { env["ALLOW_PRIVATE_TARGETS"] = "yes please" },
			wantText: "ALLOW_PRIVATE_TARGETS must be a boolean",
		},
		{
			name: "lease shorter than check timeout",
			mutate: func(env map[string]string) {
				env["WORKER_LEASE_DURATION"] = "5s"
				env["DEFAULT_CHECK_TIMEOUT"] = "10s"
			},
			wantText: "WORKER_LEASE_DURATION (5s) must be greater than DEFAULT_CHECK_TIMEOUT (10s)",
		},
		{
			name: "lease equal to check timeout",
			mutate: func(env map[string]string) {
				env["WORKER_LEASE_DURATION"] = "10s"
				env["DEFAULT_CHECK_TIMEOUT"] = "10s"
			},
			wantText: "must be greater than DEFAULT_CHECK_TIMEOUT",
		},
		{
			name:     "otel enabled without endpoint",
			mutate:   func(env map[string]string) { env["OTEL_ENABLED"] = "true" },
			wantText: "OTEL_EXPORTER_OTLP_ENDPOINT is required when OTEL_ENABLED is true",
		},
		{
			name: "private targets allowed outside development",
			mutate: func(env map[string]string) {
				env["ENVIRONMENT"] = config.EnvProduction
				env["ALLOW_PRIVATE_TARGETS"] = "true"
			},
			wantText: `ALLOW_PRIVATE_TARGETS must be false when ENVIRONMENT is "production"`,
		},
		{
			name: "public and internal listeners collide",
			mutate: func(env map[string]string) {
				env["HTTP_ADDR"] = ":8080"
				env["INTERNAL_HTTP_ADDR"] = ":8080"
			},
			wantText: "HTTP_ADDR and INTERNAL_HTTP_ADDR must differ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := validEnv()
			tt.mutate(env)

			_, err := config.LoadFrom(lookupFrom(env))
			if err == nil {
				t.Fatal("LoadFrom() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantText)
			}
		})
	}
}

// TestLoadFromReportsEveryProblem is the reason validation accumulates errors:
// a misconfigured deployment should need one restart to find every mistake,
// not one per variable.
func TestLoadFromReportsEveryProblem(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"WORKER_CONCURRENCY": "0",
		"LOG_LEVEL":          "verbose",
	}

	_, err := config.LoadFrom(lookupFrom(env))
	if err == nil {
		t.Fatal("LoadFrom() succeeded, want an error")
	}

	for _, want := range []string{"DATABASE_URL", "API_KEY", "WORKER_CONCURRENCY", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing a report for %s:\n%v", want, err)
		}
	}
}

// TestConfigLogValueRedactsSecrets guards CLAUDE.md rule 10: the API key and
// the database password must never reach the logs.
func TestConfigLogValueRedactsSecrets(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadFrom(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("LoadFrom() returned an unexpected error: %v", err)
	}

	rendered := cfg.LogValue().String()

	if strings.Contains(rendered, "0123456789abcdef") {
		t.Errorf("log value leaked the API key: %s", rendered)
	}
	if strings.Contains(rendered, "secret") {
		t.Errorf("log value leaked the database password: %s", rendered)
	}
	if !strings.Contains(rendered, "localhost:5432") {
		t.Errorf("log value should still identify the database host: %s", rendered)
	}
}

// TestDBMaxConnsCoversWorkerConcurrency keeps the pool from becoming the
// bottleneck that silently serialises a highly concurrent worker.
func TestDBMaxConnsCoversWorkerConcurrency(t *testing.T) {
	t.Parallel()

	env := validEnv()
	env["WORKER_CONCURRENCY"] = "64"

	cfg, err := config.LoadFrom(lookupFrom(env))
	if err != nil {
		t.Fatalf("LoadFrom() returned an unexpected error: %v", err)
	}

	if got, want := cfg.DB.MaxConns, int32(66); got != want {
		t.Errorf("DB.MaxConns = %d, want %d", got, want)
	}
}
