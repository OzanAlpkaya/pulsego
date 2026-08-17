// Command migrate applies the embedded goose migrations and exits. It is a
// separate binary on purpose: the API and worker never migrate on startup, so
// scaling them to several replicas cannot race on the schema. Compose runs it
// once before the services come up.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/OzanAlpkaya/pulsego/db"
	"github.com/OzanAlpkaya/pulsego/internal/config"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// migrateTimeout bounds a migration run so a stuck lock cannot hang a deploy.
const migrateTimeout = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet, so report on stderr as well.
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	command := "up"
	args := os.Args[1:]
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := telemetry.NewLogger(os.Stdout, cfg.LogLevel, cfg.ServiceName+"-migrate", cfg.Environment)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, migrateTimeout)
	defer cancel()

	sqlDB, err := openDB(ctx, cfg.DB.URL)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := sqlDB.Close(); cerr != nil {
			log.Error("closing database failed", slog.Any("error", cerr))
		}
	}()

	goose.SetBaseFS(db.Migrations)
	goose.SetLogger(gooseLogger{log: log})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	log.Info("running migrations", slog.String("command", command))

	if err := goose.RunContext(ctx, command, sqlDB, db.MigrationsDir, args...); err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}

	log.Info("migrations complete", slog.String("command", command))
	return nil
}

// openDB opens a database/sql handle over pgx and waits for the server to
// accept connections, which Compose cannot fully guarantee on first boot.
func openDB(ctx context.Context, databaseURL string) (*sql.DB, error) {
	connCfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	sqlDB := stdlib.OpenDB(*connCfg)

	const (
		attempts = 30
		delay    = time.Second
	)
	var lastErr error
	for range attempts {
		if err := sqlDB.PingContext(ctx); err == nil {
			return sqlDB, nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lastErr)
		case <-time.After(delay):
		}
	}

	_ = sqlDB.Close()
	return nil, fmt.Errorf("database unreachable after %s: %w", attempts*delay, lastErr)
}

// gooseLogger adapts goose's logger interface onto slog.
type gooseLogger struct{ log *slog.Logger }

func (g gooseLogger) Printf(format string, v ...any) {
	g.log.Info(strings.TrimSpace(fmt.Sprintf(format, v...)))
}

func (g gooseLogger) Fatalf(format string, v ...any) {
	g.log.Error(strings.TrimSpace(fmt.Sprintf(format, v...)))
	os.Exit(1)
}
