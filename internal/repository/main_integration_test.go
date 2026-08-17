//go:build integration

package repository_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/OzanAlpkaya/pulsego/db"
)

// sharedPool is a migrated database reused by every test that does not need a
// pristine schema. Starting one container for the package rather than one per
// test is the difference between a suite that runs in seconds and one nobody
// waits for; tests keep their isolation by truncating between runs.
var sharedPool *pgxpool.Pool

func TestMain(m *testing.M) {
	code, err := runTests(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runTests(m *testing.M) (int, error) {
	ctx := context.Background()

	container, err := runPostgres(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintf(os.Stderr, "terminating postgres: %v\n", err)
		}
	}()

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return 0, fmt.Errorf("connection string: %w", err)
	}

	if err := migrateURL(url); err != nil {
		return 0, err
	}

	sharedPool, err = pgxpool.New(ctx, url)
	if err != nil {
		return 0, fmt.Errorf("open shared pool: %w", err)
	}
	defer sharedPool.Close()

	return m.Run(), nil
}

func runPostgres(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("pulsego"),
		tcpostgres.WithUsername("pulsego"),
		tcpostgres.WithPassword("pulsego"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	return container, nil
}

// migrateURL applies every migration to a database, using its own short-lived
// database/sql handle because goose does not speak pgx directly.
func migrateURL(url string) error {
	connCfg, err := pgx.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("parse connection string: %w", err)
	}

	sqlDB := stdlib.OpenDB(*connCfg)
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(db.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.Up(sqlDB, db.MigrationsDir); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// truncateMonitors gives a test an empty table. Tests using the shared pool run
// serially for this reason.
func truncateMonitors(t *testing.T) {
	t.Helper()

	if _, err := sharedPool.Exec(t.Context(), "TRUNCATE monitors"); err != nil {
		t.Fatalf("truncating monitors failed: %v", err)
	}
}

// openSQL returns a database/sql handle for tests that drive goose directly.
func openSQL(t *testing.T, url string) *sql.DB {
	t.Helper()

	connCfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}

	sqlDB := stdlib.OpenDB(*connCfg)
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Logf("close sql handle: %v", err)
		}
	})
	return sqlDB
}
