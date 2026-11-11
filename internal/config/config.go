// Package config loads and validates every environment variable the API and
// the worker depend on. Loading happens once at startup and either returns a
// fully validated Config or an error listing all problems at once, so a
// misconfigured deployment fails immediately instead of at first use.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// Environment names accepted by ENVIRONMENT.
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// Connection pool sizing, derived from worker concurrency rather than
// configured directly.
const (
	poolHeadroom = 2
	minPoolConns = 4
	maxPoolConns = 300
)

// Config is the fully validated configuration shared by both binaries.
type Config struct {
	ServiceName     string
	Environment     string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration

	Server    Server
	DB        DB
	Worker    Worker
	Checks    Checks
	Telemetry Telemetry
}

// Server holds the HTTP listener settings and the API credential.
type Server struct {
	Addr         string
	InternalAddr string
	// APIKey guards /v1/*. It is never logged; see Config.LogValue.
	APIKey string
}

// DB holds the PostgreSQL connection settings.
type DB struct {
	URL      string
	MaxConns int32
}

// Worker holds the claim-loop settings of cmd/worker.
type Worker struct {
	Concurrency   int
	BatchSize     int
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

// Checks holds the defaults and policy applied to outbound checks.
type Checks struct {
	DefaultInterval     time.Duration
	DefaultTimeout      time.Duration
	AllowPrivateTargets bool
	RetentionDays       int
}

// Telemetry holds the OpenTelemetry export settings. Tracing is optional and
// must never block startup when disabled.
type Telemetry struct {
	OTelEnabled  bool
	OTLPEndpoint string
}

// IsProduction reports whether the service runs in a non-development
// environment, which tightens some defaults.
func (c Config) IsProduction() bool { return c.Environment == EnvProduction }

// LogValue implements slog.LogValuer so that logging a Config can never leak
// the API key or the database password.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("service_name", c.ServiceName),
		slog.String("environment", c.Environment),
		slog.String("log_level", c.LogLevel.String()),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.String("http_addr", c.Server.Addr),
		slog.String("internal_http_addr", c.Server.InternalAddr),
		slog.String("database", redactURL(c.DB.URL)),
		slog.Int("worker_concurrency", c.Worker.Concurrency),
		slog.Int("worker_batch_size", c.Worker.BatchSize),
		slog.Duration("worker_poll_interval", c.Worker.PollInterval),
		slog.Duration("worker_lease_duration", c.Worker.LeaseDuration),
		slog.Duration("default_check_interval", c.Checks.DefaultInterval),
		slog.Duration("default_check_timeout", c.Checks.DefaultTimeout),
		slog.Bool("allow_private_targets", c.Checks.AllowPrivateTargets),
		slog.Int("check_retention_days", c.Checks.RetentionDays),
		slog.Bool("otel_enabled", c.Telemetry.OTelEnabled),
	)
}

// LookupFunc resolves an environment variable, reporting whether it was set.
// It matches the signature of os.LookupEnv.
type LookupFunc func(key string) (string, bool)

// Load reads the configuration from the process environment.
func Load() (Config, error) { return LoadFrom(os.LookupEnv) }

// LoadFrom reads the configuration from an arbitrary source, which keeps
// tests independent of the developer's own environment. All validation
// failures are collected and returned together.
func LoadFrom(lookup LookupFunc) (Config, error) {
	l := &loader{lookup: lookup}

	cfg := Config{
		ServiceName:     l.str("SERVICE_NAME", "pulsego"),
		Environment:     l.enum("ENVIRONMENT", EnvDevelopment, EnvDevelopment, EnvStaging, EnvProduction),
		LogLevel:        l.logLevel("LOG_LEVEL", slog.LevelInfo),
		ShutdownTimeout: l.duration("SHUTDOWN_TIMEOUT", 15*time.Second, time.Second, 2*time.Minute),

		Server: Server{
			Addr:         l.str("HTTP_ADDR", ":8080"),
			InternalAddr: l.str("INTERNAL_HTTP_ADDR", ":9090"),
			APIKey:       l.apiKey("API_KEY"),
		},
		DB: DB{
			URL:      l.databaseURL("DATABASE_URL"),
			MaxConns: 10,
		},
		Worker: Worker{
			Concurrency:   l.intVal("WORKER_CONCURRENCY", 10, 1, 256),
			BatchSize:     l.intVal("WORKER_BATCH_SIZE", 20, 1, 500),
			PollInterval:  l.duration("WORKER_POLL_INTERVAL", 5*time.Second, time.Second, 5*time.Minute),
			LeaseDuration: l.duration("WORKER_LEASE_DURATION", 60*time.Second, 5*time.Second, 10*time.Minute),
		},
		Checks: Checks{
			DefaultInterval: l.duration("DEFAULT_CHECK_INTERVAL", domain.DefaultCheckInterval,
				domain.MinCheckInterval, domain.MaxCheckInterval),
			DefaultTimeout: l.duration("DEFAULT_CHECK_TIMEOUT", domain.DefaultCheckTimeout,
				domain.MinCheckTimeout, domain.MaxCheckTimeout),
			AllowPrivateTargets: l.boolVal("ALLOW_PRIVATE_TARGETS", false),
			RetentionDays:       l.intVal("CHECK_RETENTION_DAYS", 30, 1, 365),
		},
		Telemetry: Telemetry{
			OTelEnabled:  l.boolVal("OTEL_ENABLED", false),
			OTLPEndpoint: l.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		},
	}

	if required := poolSize(cfg.Worker.Concurrency); cfg.DB.MaxConns < required {
		cfg.DB.MaxConns = required
	}

	l.validateCombinations(cfg)

	if err := errors.Join(l.errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateCombinations checks the rules that involve more than one variable.
func (l *loader) validateCombinations(cfg Config) {
	// A lease shorter than the request timeout would let a second worker
	// reclaim a monitor while the first is still waiting on the network.
	if cfg.Worker.LeaseDuration <= cfg.Checks.DefaultTimeout {
		l.errf("WORKER_LEASE_DURATION (%s) must be greater than DEFAULT_CHECK_TIMEOUT (%s)",
			cfg.Worker.LeaseDuration, cfg.Checks.DefaultTimeout)
	}

	// Exporting traces without a usable destination silently drops them.
	if cfg.Telemetry.OTelEnabled {
		l.otlpEndpoint("OTEL_EXPORTER_OTLP_ENDPOINT", cfg.Telemetry.OTLPEndpoint)
	}

	// Reaching private address space is a development-only affordance; see
	// the SSRF section of the README.
	if cfg.Checks.AllowPrivateTargets && cfg.Environment != EnvDevelopment {
		l.errf("ALLOW_PRIVATE_TARGETS must be false when ENVIRONMENT is %q", cfg.Environment)
	}

	if cfg.Server.Addr == cfg.Server.InternalAddr {
		l.errf("HTTP_ADDR and INTERNAL_HTTP_ADDR must differ, both are %q", cfg.Server.Addr)
	}
}

// loader reads values one at a time and accumulates every failure so that a
// single run reports all configuration problems.
type loader struct {
	lookup LookupFunc
	errs   []error
}

func (l *loader) errf(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("config: "+format, args...))
}

// get returns the trimmed value of key and whether it was set to a non-empty
// string. An empty value is treated as unset so that a blank line in an env
// file behaves like an absent one.
func (l *loader) get(key string) (string, bool) {
	raw, ok := l.lookup(key)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(raw)
	return trimmed, trimmed != ""
}

func (l *loader) str(key, def string) string {
	if v, ok := l.get(key); ok {
		return v
	}
	return def
}

func (l *loader) required(key string) string {
	v, ok := l.get(key)
	if !ok {
		l.errf("%s is required", key)
		return ""
	}
	return v
}

func (l *loader) enum(key, def string, allowed ...string) string {
	v := l.str(key, def)
	if !slices.Contains(allowed, v) {
		l.errf("%s must be one of %s, got %q", key, strings.Join(allowed, ", "), v)
	}
	return v
}

func (l *loader) intVal(key string, def, minVal, maxVal int) int {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errf("%s must be an integer, got %q", key, v)
		return def
	}
	if n < minVal || n > maxVal {
		l.errf("%s must be between %d and %d, got %d", key, minVal, maxVal, n)
		return def
	}
	return n
}

func (l *loader) duration(key string, def, minVal, maxVal time.Duration) time.Duration {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errf("%s must be a duration such as 30s or 5m, got %q", key, v)
		return def
	}
	if d < minVal || d > maxVal {
		l.errf("%s must be between %s and %s, got %s", key, minVal, maxVal, d)
		return def
	}
	return d
}

func (l *loader) boolVal(key string, def bool) bool {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errf("%s must be a boolean such as true or false, got %q", key, v)
		return def
	}
	return b
}

func (l *loader) logLevel(key string, def slog.Level) slog.Level {
	v, ok := l.get(key)
	if !ok {
		return def
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		l.errf("%s must be one of debug, info, warn, error, got %q", key, v)
		return def
	}
	return level
}

// minAPIKeyLength keeps a placeholder value from being usable as a credential.
const minAPIKeyLength = 16

func (l *loader) apiKey(key string) string {
	v := l.required(key)
	if v != "" && len(v) < minAPIKeyLength {
		l.errf("%s must be at least %d characters", key, minAPIKeyLength)
	}
	return v
}

func (l *loader) databaseURL(key string) string {
	v := l.required(key)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil {
		l.errf("%s is not a valid URL: %v", key, err)
		return v
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		l.errf("%s must use the postgres:// or postgresql:// scheme, got %q", key, u.Scheme)
	}
	if u.Host == "" {
		l.errf("%s must include a host", key)
	}
	return v
}

// otlpEndpoint checks the OTLP/HTTP base URL used when tracing is enabled:
// it must be an http(s) URL with a host, such as http://otel-collector:4318.
func (l *loader) otlpEndpoint(key, v string) {
	if v == "" {
		l.errf("%s is required when OTEL_ENABLED is true", key)
		return
	}
	u, err := url.Parse(v)
	if err != nil {
		l.errf("%s is not a valid URL: %v", key, err)
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		l.errf("%s must use the http:// or https:// scheme, got %q", key, u.Scheme)
	}
	if u.Hostname() == "" {
		l.errf("%s must include a host", key)
	}
}

// poolSize derives the connection pool size from worker concurrency: every
// concurrent check needs a connection, plus headroom for the claim
// transaction and health probes. Nothing configures it directly, because a
// pool smaller than the concurrency limit would silently serialise the worker.
func poolSize(concurrency int) int32 {
	size := concurrency + poolHeadroom
	if size < minPoolConns {
		size = minPoolConns
	}
	if size > maxPoolConns {
		size = maxPoolConns
	}
	return int32(size) // #nosec G115 -- clamped to [minPoolConns, maxPoolConns] above
}

// redactURL renders a database URL without its password, for logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.Redacted()
}
