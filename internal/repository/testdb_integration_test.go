//go:build integration

package repository_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// QA tests for D1: with PULSEGO_TEST_DATABASE_URL set, every database the
// suite uses is pulsego_test_<16 hex>, created and dropped by the suite, and a
// role without CREATEDB fails with a message naming that privilege.

var testDatabaseName = regexp.MustCompile(`^pulsego_test_[0-9a-f]{16}$`)

func requireServerURL(t *testing.T) string {
	t.Helper()
	serverURL := os.Getenv(testDatabaseURLEnv)
	if serverURL == "" {
		t.Skipf("%s is not set; these tests cover the external-server mode only", testDatabaseURLEnv)
	}
	return serverURL
}

func databaseExists(t *testing.T, serverURL, name string) bool {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), serverURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()

	var exists bool
	if err := conn.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	return exists
}

func TestSharedPoolNeverUsesTheURLDatabase(t *testing.T) {
	serverURL := requireServerURL(t)

	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}
	urlDB := strings.TrimPrefix(parsed.Path, "/")

	var current string
	if err := sharedPool.QueryRow(t.Context(), "SELECT current_database()").Scan(&current); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if current == urlDB {
		t.Fatalf("shared pool runs on %q, the database the URL points at", current)
	}
	if !testDatabaseName.MatchString(current) {
		t.Fatalf("shared pool database %q does not match %s", current, testDatabaseName)
	}
}

func TestCreateTestDatabaseIsNamedAndDroppedWithForce(t *testing.T) {
	serverURL := requireServerURL(t)

	dbURL, drop, err := createTestDatabase(t.Context(), serverURL)
	if err != nil {
		t.Fatalf("createTestDatabase: %v", err)
	}
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse created URL: %v", err)
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if !testDatabaseName.MatchString(name) {
		_ = drop()
		t.Fatalf("created database %q does not match %s", name, testDatabaseName)
	}
	if !databaseExists(t, serverURL, name) {
		t.Fatalf("database %q was not created", name)
	}

	// An open connection must not block the drop: it uses WITH (FORCE).
	conn, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		_ = drop()
		t.Fatalf("connect to created database: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()

	if err := drop(); err != nil {
		t.Fatalf("drop with an open connection: %v", err)
	}
	if databaseExists(t, serverURL, name) {
		t.Fatalf("database %q still exists after drop", name)
	}
}

func TestCreateTestDatabaseWithoutCreateDBNamesThePrivilege(t *testing.T) {
	serverURL := requireServerURL(t)

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	role := "pulsego_qa_nocreatedb_" + hex.EncodeToString(suffix)
	quoted := pgx.Identifier{role}.Sanitize()

	if err := execOnServer(t.Context(), serverURL,
		"CREATE ROLE "+quoted+" LOGIN NOCREATEDB PASSWORD 'qa-nocreatedb'"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		if err := execOnServer(context.Background(), serverURL, "DROP ROLE IF EXISTS "+quoted); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})

	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(role, "qa-nocreatedb")

	_, drop, err := createTestDatabase(t.Context(), parsed.String())
	if err == nil {
		_ = drop()
		t.Fatal("createTestDatabase succeeded for a role without CREATEDB")
	}
	if !strings.Contains(err.Error(), "CREATEDB") {
		t.Fatalf("error %q does not name the CREATEDB privilege", err)
	}
}
