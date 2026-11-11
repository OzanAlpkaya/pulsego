//go:build integration

// Integration tests run against a real PostgreSQL: a Testcontainers container
// by default, or the server named by PULSEGO_TEST_DATABASE_URL when it is set.
// They are behind the `integration` build tag so that `go test ./...` stays
// fast and works without a database; run them with `make test-integration`.
package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/OzanAlpkaya/pulsego/db"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
)

const postgresImage = "postgres:17-alpine"

// startPostgres brings up a throwaway, empty PostgreSQL database and returns
// its connection URL. With PULSEGO_TEST_DATABASE_URL set it is a fresh
// database on that server, dropped when the test finishes; otherwise it is a
// container, terminated when the test finishes.
func startPostgres(t *testing.T) string {
	t.Helper()

	ctx := context.Background()

	if serverURL := os.Getenv(testDatabaseURLEnv); serverURL != "" {
		url, drop, err := createTestDatabase(ctx, serverURL)
		if err != nil {
			t.Fatalf("%v", err)
		}
		t.Cleanup(func() {
			if err := drop(); err != nil {
				t.Errorf("%v", err)
			}
		})
		return url
	}

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
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("build connection string: %v", err)
	}
	return url
}

func migrateUp(t *testing.T, sqlDB *sql.DB) {
	t.Helper()

	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	if err := goose.UpContext(context.Background(), sqlDB, db.MigrationsDir); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

// TestMigrationsUpDown proves the schema applies and rolls back cleanly, which
// is what makes a bad migration recoverable in a real deployment.
func TestMigrationsUpDown(t *testing.T) {
	url := startPostgres(t)
	sqlDB := openSQL(t, url)

	migrateUp(t, sqlDB)

	if got, err := goose.GetDBVersionContext(context.Background(), sqlDB); err != nil {
		t.Fatalf("goose version: %v", err)
	} else if got != latestMigration {
		t.Errorf("schema version after up = %d, want %d", got, latestMigration)
	}

	for _, table := range migratedTables {
		if !tableExists(t, sqlDB, table) {
			t.Errorf("%s table does not exist after migrating up", table)
		}
	}

	// DownTo(0) unwinds every migration, not just the most recent one, which is
	// what proves the whole schema is reversible.
	if err := goose.DownToContext(context.Background(), sqlDB, db.MigrationsDir, 0); err != nil {
		t.Fatalf("goose down: %v", err)
	}

	for _, table := range migratedTables {
		if tableExists(t, sqlDB, table) {
			t.Errorf("%s table still exists after migrating down", table)
		}
	}
}

// latestMigration is the version of the newest file in db/migrations. Bump it
// with every new migration so this test notices a file that goose skipped.
const latestMigration = 5

// TestMigration00005BlockedErrorKind proves the 'blocked' error kind exists
// only from migration 00005 on, and that rolling it back keeps the affected
// check rows (as 'unknown') instead of failing on the restored constraint.
func TestMigration00005BlockedErrorKind(t *testing.T) {
	url := startPostgres(t)
	sqlDB := openSQL(t, url)
	ctx := context.Background()

	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	if err := goose.UpToContext(ctx, sqlDB, db.MigrationsDir, 4); err != nil {
		t.Fatalf("goose up to 4: %v", err)
	}

	var monitorID string
	if err := sqlDB.QueryRowContext(ctx, `
		INSERT INTO monitors (name, url, interval_seconds, timeout_ms,
		                      expected_status_min, expected_status_max, failure_threshold)
		VALUES ('example', 'https://example.com', 60, 5000, 200, 399, 3)
		RETURNING id`).Scan(&monitorID); err != nil {
		t.Fatalf("insert monitor: %v", err)
	}

	insertBlocked := func() (int64, error) {
		var id int64
		err := sqlDB.QueryRowContext(ctx, `
			INSERT INTO monitor_checks (monitor_id, success, latency_ms, error_kind, error_message)
			VALUES ($1, false, 0, 'blocked', 'blocked target: loopback')
			RETURNING id`, monitorID).Scan(&id)
		return id, err
	}

	if _, err := insertBlocked(); err == nil || !strings.Contains(err.Error(), "monitor_checks_error_kind_valid") {
		t.Fatalf("insert 'blocked' before 00005: err = %v, want a monitor_checks_error_kind_valid violation", err)
	}

	if err := goose.UpToContext(ctx, sqlDB, db.MigrationsDir, 5); err != nil {
		t.Fatalf("goose up to 5: %v", err)
	}
	checkID, err := insertBlocked()
	if err != nil {
		t.Fatalf("insert 'blocked' after 00005: %v", err)
	}

	if err := goose.DownToContext(ctx, sqlDB, db.MigrationsDir, 4); err != nil {
		t.Fatalf("goose down to 4: %v", err)
	}
	var kind string
	if err := sqlDB.QueryRowContext(ctx, `SELECT error_kind FROM monitor_checks WHERE id = $1`, checkID).Scan(&kind); err != nil {
		t.Fatalf("read check after down: %v", err)
	}
	if kind != "unknown" {
		t.Errorf("error_kind after down = %q, want unknown", kind)
	}
	if _, err := insertBlocked(); err == nil || !strings.Contains(err.Error(), "monitor_checks_error_kind_valid") {
		t.Fatalf("insert 'blocked' after down: err = %v, want a monitor_checks_error_kind_valid violation", err)
	}
}

// migratedTables lists every table the migrations create.
var migratedTables = []string{"monitors", "monitor_checks", "incidents"}

func tableExists(t *testing.T, sqlDB *sql.DB, table string) bool {
	t.Helper()

	var exists bool
	if err := sqlDB.QueryRowContext(t.Context(), `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
		t.Fatalf("check %s table: %v", table, err)
	}
	return exists
}

// TestMonitorsPartialIndex verifies the scheduler's hot-path index exists with
// its soft-delete predicate. Without the predicate the index would grow with
// every deleted monitor.
func TestMonitorsPartialIndex(t *testing.T) {
	url := startPostgres(t)
	sqlDB := openSQL(t, url)
	migrateUp(t, sqlDB)

	var definition string
	err := sqlDB.QueryRowContext(t.Context(),
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'monitors' AND indexname = 'monitors_due_idx'`,
	).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("monitors_due_idx does not exist")
	}
	if err != nil {
		t.Fatalf("read index definition: %v", err)
	}

	for _, want := range []string{"enabled", "next_check_at", "deleted_at IS NULL"} {
		if !strings.Contains(definition, want) {
			t.Errorf("index definition %q is missing %q", definition, want)
		}
	}
}

// TestMonitorsConstraints checks that the database is the last line of defence
// for every bound the domain package declares, so a bad write cannot land even
// if application validation is bypassed.
func TestMonitorsConstraints(t *testing.T) {
	url := startPostgres(t)
	sqlDB := openSQL(t, url)
	migrateUp(t, sqlDB)

	const insert = `
		INSERT INTO monitors (name, url, interval_seconds, timeout_ms,
		                      expected_status_min, expected_status_max,
		                      failure_threshold, state, lease_owner, lease_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	type row struct {
		name             string
		url              string
		interval         int
		timeoutMS        int
		statusMin        int
		statusMax        int
		failureThreshold int
		state            string
		leaseOwner       *string
		leaseUntil       *time.Time
	}

	valid := row{
		name: "example", url: "https://example.com", interval: 60, timeoutMS: 5000,
		statusMin: 200, statusMax: 399, failureThreshold: 3, state: "unknown",
	}

	owner := "worker-1"
	future := time.Now().Add(time.Minute)

	tests := []struct {
		name       string
		mutate     func(*row)
		constraint string
	}{
		{name: "blank name", mutate: func(r *row) { r.name = "   " }, constraint: "monitors_name_not_blank"},
		{name: "blank url", mutate: func(r *row) { r.url = "" }, constraint: "monitors_url_not_blank"},
		{name: "interval too small", mutate: func(r *row) { r.interval = 1 }, constraint: "monitors_interval_range"},
		{name: "interval too large", mutate: func(r *row) { r.interval = 90000 }, constraint: "monitors_interval_range"},
		{name: "timeout too small", mutate: func(r *row) { r.timeoutMS = 10 }, constraint: "monitors_timeout_range"},
		{name: "timeout too large", mutate: func(r *row) { r.timeoutMS = 120000 }, constraint: "monitors_timeout_range"},
		{name: "status min out of range", mutate: func(r *row) { r.statusMin = 99 }, constraint: "monitors_expected_status_min_range"},
		{name: "status max out of range", mutate: func(r *row) { r.statusMax = 600 }, constraint: "monitors_expected_status_max_range"},
		{name: "status range inverted", mutate: func(r *row) { r.statusMin, r.statusMax = 400, 200 }, constraint: "monitors_expected_status_order"},
		{name: "failure threshold too small", mutate: func(r *row) { r.failureThreshold = 0 }, constraint: "monitors_failure_threshold_range"},
		{name: "failure threshold too large", mutate: func(r *row) { r.failureThreshold = 99 }, constraint: "monitors_failure_threshold_range"},
		{name: "unknown state", mutate: func(r *row) { r.state = "flapping" }, constraint: "monitors_state_valid"},
		{name: "lease owner without expiry", mutate: func(r *row) { r.leaseOwner = &owner }, constraint: "monitors_lease_paired"},
		{name: "lease expiry without owner", mutate: func(r *row) { r.leaseUntil = &future }, constraint: "monitors_lease_paired"},
	}

	// The valid row must be accepted, otherwise every rejection below is
	// meaningless.
	if _, err := sqlDB.ExecContext(t.Context(), insert, valid.name, valid.url, valid.interval, valid.timeoutMS,
		valid.statusMin, valid.statusMax, valid.failureThreshold, valid.state,
		valid.leaseOwner, valid.leaseUntil); err != nil {
		t.Fatalf("valid monitor was rejected: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.mutate(&r)

			_, err := sqlDB.ExecContext(t.Context(), insert, r.name, r.url, r.interval, r.timeoutMS,
				r.statusMin, r.statusMax, r.failureThreshold, r.state, r.leaseOwner, r.leaseUntil)
			if err == nil {
				t.Fatalf("insert succeeded, want a violation of %s", tt.constraint)
			}
			if !strings.Contains(err.Error(), tt.constraint) {
				t.Errorf("error = %v, want a violation of %s", err, tt.constraint)
			}
		})
	}
}

// TestNewPoolConnects covers the pool helper both binaries start with.
func TestNewPoolConnects(t *testing.T) {
	url := startPostgres(t)

	ctx := context.Background()
	pool, err := repository.NewPool(ctx, url, 5, nil)
	if err != nil {
		t.Fatalf("NewPool() returned an unexpected error: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		t.Errorf("Ping() returned an unexpected error: %v", err)
	}
}

// TestNewPoolRejectsUnreachableDatabase makes sure startup fails loudly rather
// than serving traffic against a database that was never reachable.
func TestNewPoolRejectsUnreachableDatabase(t *testing.T) {
	ctx := context.Background()

	// Port 1 is reserved and never listening.
	_, err := repository.NewPool(ctx, "postgres://pulsego:pulsego@127.0.0.1:1/pulsego?sslmode=disable", 5, nil)
	if err == nil {
		t.Fatal("NewPool() succeeded against an unreachable database, want an error")
	}
}
