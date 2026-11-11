//go:build integration

package repository_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/checker"
	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// TestSchedulerEndToEnd runs the real scheduler, checker and SchedulerStore
// against an httptest target: a monitor created through the store gets a
// check row within the poll interval plus the monitor's timeout.
func TestSchedulerEndToEnd(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") != checker.UserAgent {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	monitors := newStore(t) // truncates the tables
	params := validCreate()
	params.URL = target.URL + "/health?token=secret"
	params.TimeoutMS = 2000
	created, err := monitors.Create(t.Context(), params)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	const poll = 500 * time.Millisecond
	chk := checker.New(security.Policy{AllowPrivate: true}) // httptest target on loopback
	t.Cleanup(chk.CloseIdleConnections)
	s := scheduler.New(repository.NewSchedulerStore(sharedPool), chk, scheduler.Config{
		WorkerID:        "e2e-worker",
		Concurrency:     2,
		BatchSize:       5,
		PollInterval:    poll,
		LeaseDuration:   30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// A little slack on top of poll + timeout for scheduling under -race.
	deadline := time.Now().Add(poll + created.Timeout() + time.Second)
	for countChecks(t, created.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no check row within poll interval + timeout (target hits: %d)", hits.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done

	var (
		success    bool
		statusCode *int32
		errorKind  *string
	)
	if err := sharedPool.QueryRow(t.Context(),
		`SELECT success, status_code, error_kind FROM monitor_checks WHERE monitor_id = $1`,
		created.ID).Scan(&success, &statusCode, &errorKind); err != nil {
		t.Fatalf("read check row: %v", err)
	}
	if !success || statusCode == nil || *statusCode != http.StatusNoContent || errorKind != nil {
		t.Fatalf("check row = success %v, status %v, error_kind %v; want a 204 success",
			success, deref(statusCode), deref(errorKind))
	}

	lease := readLease(t, created.ID)
	if lease.owner != nil || lease.until != nil {
		t.Fatalf("lease not cleared after the check: %+v", lease)
	}
	if n := countChecks(t, created.ID); n != 1 {
		t.Fatalf("%d check rows, want exactly 1 (the next check is an interval away)", n)
	}

	var (
		state    string
		failures int32
	)
	if err := sharedPool.QueryRow(t.Context(),
		`SELECT state, consecutive_failures FROM monitors WHERE id = $1`, created.ID).
		Scan(&state, &failures); err != nil {
		t.Fatalf("read monitor state: %v", err)
	}
	if state != string(domain.StateUp) || failures != 0 {
		t.Fatalf("state = %q with %d failures after a successful check, want %q with 0",
			state, failures, domain.StateUp)
	}
}
