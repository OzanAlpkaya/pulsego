package scheduler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/checker"
	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
)

const (
	testWorkerID = "test-worker"
	testPoll     = 10 * time.Millisecond
	// testShutdown is the shutdown timeout for tests whose checks are still
	// blocked when they stop the scheduler.
	testShutdown = 50 * time.Millisecond
	// waitFor gives up after this long; generous so -race on a busy machine
	// does not flake.
	waitLimit = 10 * time.Second
)

// fakeStore hands out an unlimited (or limited) supply of due monitors and
// records every call.
type fakeStore struct {
	mu       sync.Mutex
	url      string
	supply   int // monitors left to hand out; < 0 means unlimited
	claims   []int32
	records  []uuid.UUID
	releases []uuid.UUID
	// writeCtxErrs collects ctx.Err() seen by Record and Release.
	writeCtxErrs []error
	recordErr    error
}

func newFakeStore(supply int) *fakeStore {
	return &fakeStore{supply: supply, url: "https://example.invalid/"}
}

func (f *fakeStore) Claim(_ context.Context, workerID string, batchSize int32, _ time.Duration) ([]domain.Monitor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if workerID != testWorkerID {
		return nil, errors.New("wrong worker id")
	}
	f.claims = append(f.claims, batchSize)
	n := int(batchSize)
	if f.supply >= 0 {
		n = min(n, f.supply)
		f.supply -= n
	}
	out := make([]domain.Monitor, n)
	for i := range out {
		out[i] = domain.Monitor{
			ID:                uuid.New(),
			URL:               f.url,
			TimeoutMS:         5000,
			ExpectedStatusMin: 200,
			ExpectedStatusMax: 399,
			NextCheckAt:       time.Now(),
		}
	}
	return out, nil
}

func (f *fakeStore) Record(ctx context.Context, _ string, id uuid.UUID, _ time.Time, _ domain.CheckOutcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCtxErrs = append(f.writeCtxErrs, ctx.Err())
	f.records = append(f.records, id)
	return f.recordErr
}

func (f *fakeStore) Release(ctx context.Context, _ string, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeCtxErrs = append(f.writeCtxErrs, ctx.Err())
	f.releases = append(f.releases, id)
	return nil
}

type snapshot struct {
	claims            []int32
	records, releases int
	writeCtxErrs      []error
}

func (f *fakeStore) snapshot() snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return snapshot{
		claims:       append([]int32(nil), f.claims...),
		records:      len(f.records),
		releases:     len(f.releases),
		writeCtxErrs: append([]error(nil), f.writeCtxErrs...),
	}
}

// gateChecker blocks every check until it receives from gate (one value lets
// one check finish) or its context is cancelled.
type gateChecker struct {
	gate    chan struct{}
	running atomic.Int32
}

func newGateChecker() *gateChecker { return &gateChecker{gate: make(chan struct{})} }

func (g *gateChecker) Check(ctx context.Context, _ domain.Monitor) (domain.CheckOutcome, error) {
	g.running.Add(1)
	defer g.running.Add(-1)

	select {
	case <-g.gate:
		status := int32(200)
		return domain.CheckOutcome{Success: true, StatusCode: &status, LatencyMS: 1}, nil
	case <-ctx.Done():
		return domain.CheckOutcome{}, ctx.Err()
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(b.buf.Bytes()))
	for dec.More() {
		var e map[string]any
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("decode log line: %v", err)
		}
		out = append(out, e)
	}
	return out
}

type harness struct {
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan struct{}
}

// start runs a scheduler in the background. The test must call stop (or
// cancel and wait on done) before it ends.
func start(t *testing.T, store scheduler.Store, chk scheduler.Checker, concurrency, batch int, shutdown time.Duration) *harness {
	t.Helper()

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := scheduler.New(store, chk, scheduler.Config{
		WorkerID:        testWorkerID,
		Concurrency:     concurrency,
		BatchSize:       batch,
		PollInterval:    testPoll,
		LeaseDuration:   time.Minute,
		ShutdownTimeout: shutdown,
	}, log)

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{logs: logs, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(waitLimit):
			t.Error("Run did not return at cleanup")
		}
	})
	return h
}

// stop signals shutdown and waits for Run to return.
func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(waitLimit):
		t.Fatal("Run did not return after shutdown")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settle lets several poll ticks pass.
func settle() { time.Sleep(10 * testPoll) }

func TestNeverClaimsMoreThanFreeSlots(t *testing.T) {
	store := newFakeStore(-1)
	chk := newGateChecker()
	h := start(t, store, chk, 3, 10, testShutdown)

	waitFor(t, "3 running checks", func() bool { return chk.running.Load() == 3 })
	settle()
	if got := store.snapshot().claims; len(got) != 1 || got[0] != 3 {
		t.Fatalf("claims = %v, want [3]: the first claim asks for min(batch, free)", got)
	}

	// Let exactly one check finish: one slot frees up, so the next claim
	// asks for exactly one monitor.
	chk.gate <- struct{}{}
	waitFor(t, "a second claim", func() bool { return len(store.snapshot().claims) == 2 })
	waitFor(t, "3 running checks again", func() bool { return chk.running.Load() == 3 })
	settle()

	if got := store.snapshot().claims; len(got) != 2 || got[1] != 1 {
		t.Fatalf("claims = %v, want [3 1]", got)
	}

	h.stop(t)
}

func TestSkipsClaimWhenSlotsAreFull(t *testing.T) {
	store := newFakeStore(-1)
	chk := newGateChecker()
	h := start(t, store, chk, 2, 2, testShutdown)

	waitFor(t, "2 running checks", func() bool { return chk.running.Load() == 2 })
	before := len(store.snapshot().claims)
	settle()
	if after := len(store.snapshot().claims); after != before {
		t.Fatalf("claims went from %d to %d while every slot was busy", before, after)
	}

	h.stop(t)
}

func TestShutdownWaitsForInFlightChecks(t *testing.T) {
	store := newFakeStore(2)
	chk := newGateChecker()
	h := start(t, store, chk, 5, 5, waitLimit)

	waitFor(t, "2 running checks", func() bool { return chk.running.Load() == 2 })
	h.cancel()
	claimsAtSignal := len(store.snapshot().claims)

	settle()
	select {
	case <-h.done:
		t.Fatal("Run returned while checks were still in flight")
	default:
	}

	chk.gate <- struct{}{}
	chk.gate <- struct{}{}
	h.stop(t)

	got := store.snapshot()
	if got.records != 2 || got.releases != 0 {
		t.Fatalf("records = %d, releases = %d; want 2 and 0", got.records, got.releases)
	}
	if len(got.claims) != claimsAtSignal {
		t.Fatalf("claimed after the signal: %d claims, %d at signal", len(got.claims), claimsAtSignal)
	}
	for _, err := range got.writeCtxErrs {
		if err != nil {
			t.Fatalf("a write after the signal ran on a cancelled context: %v", err)
		}
	}
}

func TestChecksRunningAtShutdownTimeoutAreReleased(t *testing.T) {
	store := newFakeStore(3)
	chk := newGateChecker() // never opened: checks end only by cancellation
	h := start(t, store, chk, 3, 3, testShutdown)

	waitFor(t, "3 running checks", func() bool { return chk.running.Load() == 3 })
	h.stop(t)

	if n := chk.running.Load(); n != 0 {
		t.Fatalf("%d checks still running after Run returned", n)
	}
	got := store.snapshot()
	if got.records != 0 || got.releases != 3 {
		t.Fatalf("records = %d, releases = %d; want 0 and 3", got.records, got.releases)
	}
	for _, err := range got.writeCtxErrs {
		if err != nil {
			t.Fatalf("release ran on a cancelled context: %v", err)
		}
	}
}

func TestLeaseLostIsLoggedAndSwallowed(t *testing.T) {
	store := newFakeStore(-1)
	store.recordErr = scheduler.ErrLeaseLost
	chk := newGateChecker()
	h := start(t, store, chk, 1, 1, testShutdown)

	waitFor(t, "a running check", func() bool { return chk.running.Load() == 1 })
	chk.gate <- struct{}{}
	// The scheduler keeps going: the freed slot is claimed again.
	waitFor(t, "a second claim", func() bool { return len(store.snapshot().claims) >= 2 })
	h.stop(t)

	var warned bool
	for _, e := range h.logs.entries(t) {
		if e["level"] == "ERROR" {
			t.Errorf("unexpected error log: %v", e)
		}
		if e["level"] == "WARN" && e["msg"] == "lease lost, check result discarded" {
			warned = true
			if e["worker_id"] != testWorkerID {
				t.Errorf("warn log worker_id = %v, want %q", e["worker_id"], testWorkerID)
			}
			if _, ok := e["monitor_id"]; !ok {
				t.Errorf("warn log has no monitor_id: %v", e)
			}
		}
		if _, ok := e["url"]; ok {
			t.Errorf("log entry carries a URL: %v", e)
		}
	}
	if !warned {
		t.Fatal("no warn log for the lost lease")
	}
}

// TestConcurrencyBound runs the real checker against a blocking handler:
// with N=3 and batch size 10, at most 3 requests are ever in flight, and 3
// are reached.
func TestConcurrencyBound(t *testing.T) {
	const (
		concurrency = 3
		batch       = 10
		total       = 15
	)

	var inFlight, maxInFlight atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	store := newFakeStore(total)
	store.url = srv.URL
	chk := checker.New()
	t.Cleanup(chk.CloseIdleConnections)
	h := start(t, store, chk, concurrency, batch, waitLimit)

	waitFor(t, "3 requests in flight", func() bool { return inFlight.Load() == concurrency })
	settle() // give an over-eager scheduler time to exceed the bound
	close(release)
	waitFor(t, "every monitor recorded", func() bool { return store.snapshot().records == total })
	h.stop(t)

	if got := maxInFlight.Load(); got != concurrency {
		t.Fatalf("max in-flight = %d, want exactly %d", got, concurrency)
	}
	for _, c := range store.snapshot().claims {
		if c > concurrency {
			t.Fatalf("a claim asked for %d monitors with only %d slots", c, concurrency)
		}
	}
}

func TestFailedClaimIsRetriedNextTick(t *testing.T) {
	store := &flakyStore{fakeStore: newFakeStore(1)}
	chk := newGateChecker()
	h := start(t, store, chk, 1, 1, waitLimit)

	waitFor(t, "a check after the failed claim", func() bool { return chk.running.Load() == 1 })
	chk.gate <- struct{}{}
	waitFor(t, "the result recorded", func() bool { return store.snapshot().records == 1 })
	h.stop(t)

	var logged bool
	for _, e := range h.logs.entries(t) {
		if e["level"] == "ERROR" && e["msg"] == "claim failed, retrying next tick" {
			logged = true
		}
	}
	if !logged {
		t.Fatal("the failed claim was not logged")
	}
}

// flakyStore fails its first claim.
type flakyStore struct {
	*fakeStore
	failed atomic.Bool
}

func (f *flakyStore) Claim(ctx context.Context, workerID string, batchSize int32, lease time.Duration) ([]domain.Monitor, error) {
	if f.failed.CompareAndSwap(false, true) {
		return nil, errors.New("database unreachable")
	}
	return f.fakeStore.Claim(ctx, workerID, batchSize, lease)
}
