//go:build integration

package repository_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/checker"
	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// TestQALegacyUnsafeMonitorBlockedEndToEnd covers a monitor stored before
// Sprint 5 that targets loopback (the store does not validate URLs). With
// ALLOW_PRIVATE_TARGETS=false the real scheduler, checker and store must
// record a 'blocked' check without ever reaching the target, keep the URL
// and its query out of error_message, and the monitor service must still let
// the operator disable it (a PATCH without url) while refusing to set another
// private URL.
func TestQALegacyUnsafeMonitorBlockedEndToEnd(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	store := newStore(t) // truncates the tables
	params := validCreate()
	params.URL = target.URL + "/health?token=qa-secret-value"
	params.TimeoutMS = 2000
	created, err := store.Create(t.Context(), params)
	if err != nil {
		t.Fatalf("create legacy monitor: %v", err)
	}

	policy := security.Policy{AllowPrivate: false}
	chk := checker.New(policy)
	t.Cleanup(chk.CloseIdleConnections)
	const poll = 300 * time.Millisecond
	s := scheduler.New(repository.NewSchedulerStore(sharedPool), chk, scheduler.Config{
		WorkerID:        "qa-s5-worker",
		Concurrency:     1,
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
	deadline := time.Now().Add(poll + created.Timeout() + 2*time.Second)
	for countChecks(t, created.ID) == 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("no check row for the legacy monitor")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	var (
		success   bool
		errorKind *string
		message   *string
	)
	if err := sharedPool.QueryRow(t.Context(),
		`SELECT success, error_kind, error_message FROM monitor_checks WHERE monitor_id = $1`,
		created.ID).Scan(&success, &errorKind, &message); err != nil {
		t.Fatalf("read check row: %v", err)
	}
	if success || errorKind == nil || *errorKind != string(domain.ErrorKindBlocked) {
		t.Fatalf("check = success %v, error_kind %v; want a blocked failure", success, deref(errorKind))
	}
	if hits.Load() != 0 {
		t.Fatalf("target was reached %d times, want 0", hits.Load())
	}
	if message != nil {
		for _, leak := range []string{"qa-secret-value", "token=", "/health", target.URL} {
			if strings.Contains(*message, leak) {
				t.Errorf("error_message %q contains %q", *message, leak)
			}
		}
	}

	svc := monitor.NewService(store, monitor.Defaults{IntervalSeconds: 60, TimeoutMS: 5000}, policy)
	off := false
	updated, err := svc.Update(t.Context(), created.ID, monitor.UpdateInput{Enabled: &off})
	if err != nil {
		t.Fatalf("disable legacy monitor: %v", err)
	}
	if updated.Enabled {
		t.Fatal("monitor still enabled after PATCH enabled=false")
	}
	private := "http://10.0.0.1/"
	if _, err := svc.Update(t.Context(), created.ID, monitor.UpdateInput{URL: &private}); err == nil {
		t.Fatal("PATCH to a private URL succeeded, want a validation error")
	}
}
