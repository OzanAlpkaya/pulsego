//go:build integration

package repository_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

	var dbURL string
	if serverURL := os.Getenv(testDatabaseURLEnv); serverURL != "" {
		created, drop, err := createTestDatabase(ctx, serverURL)
		if err != nil {
			return 0, err
		}
		defer func() {
			if err := drop(); err != nil {
				fmt.Fprintf(os.Stderr, "dropping test database: %v\n", err)
			}
		}()
		dbURL = created
	} else {
		container, err := runPostgres(ctx)
		if err != nil {
			return 0, err
		}
		defer func() {
			if err := testcontainers.TerminateContainer(container); err != nil {
				fmt.Fprintf(os.Stderr, "terminating postgres: %v\n", err)
			}
		}()

		dbURL, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			return 0, fmt.Errorf("connection string: %w", err)
		}
	}

	if err := migrateURL(dbURL); err != nil {
		return 0, err
	}

	var err error
	sharedPool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		return 0, fmt.Errorf("open shared pool: %w", err)
	}
	defer sharedPool.Close()

	return m.Run(), nil
}

// testDatabaseURLEnv names an existing PostgreSQL server to use instead of
// Testcontainers. The suite never touches the database the URL points at: it
// only connects there to create and drop its own pulsego_test_* databases.
const testDatabaseURLEnv = "PULSEGO_TEST_DATABASE_URL"

// createTestDatabase creates a uniquely named database on the server behind
// serverURL and returns its connection URL plus a function that drops it.
//
// The name is the one piece of SQL in the repository that is not
// parameterised, because DDL identifiers cannot be bind parameters. It is
// generated here from crypto/rand, never taken from input, and quoted anyway.
func createTestDatabase(ctx context.Context, serverURL string) (string, func() error, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, fmt.Errorf("generate test database name: %w", err)
	}
	name := "pulsego_test_" + hex.EncodeToString(suffix)

	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s: %w", testDatabaseURLEnv, err)
	}
	parsed.Path = "/" + name
	parsed.RawPath = ""
	dbURL := parsed.String()

	quoted := pgx.Identifier{name}.Sanitize()
	if err := execOnServer(ctx, serverURL, "CREATE DATABASE "+quoted); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			return "", nil, fmt.Errorf("create test database: the role in %s lacks the CREATEDB privilege: %w",
				testDatabaseURLEnv, err)
		}
		return "", nil, fmt.Errorf("create test database %s: %w", name, err)
	}

	drop := func() error {
		// Detach from the caller's cancellation: it may already be cancelled
		// at cleanup.
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := execOnServer(dropCtx, serverURL, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("drop test database %s: %w", name, err)
		}
		return nil
	}
	return dbURL, drop, nil
}

// execOnServer runs one statement on a short-lived connection. CREATE and DROP
// DATABASE cannot run inside a transaction block, so a pool is not needed.
func execOnServer(ctx context.Context, serverURL, stmt string) error {
	conn, err := pgx.Connect(ctx, serverURL)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", testDatabaseURLEnv, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	_, err = conn.Exec(ctx, stmt)
	return err
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

// truncateMonitors gives a test empty monitors, monitor_checks and incidents tables. Tests using the shared pool run
// serially for this reason.
func truncateMonitors(t *testing.T) {
	t.Helper()

	if _, err := sharedPool.Exec(t.Context(), "TRUNCATE incidents, monitor_checks, monitors"); err != nil {
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
