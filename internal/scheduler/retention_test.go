package scheduler_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
)

const testRetentionDays = 30

// retentionCall is one DeleteExpiredChecks call the fake store saw.
type retentionCall struct {
	retentionDays, batchSize int32
}

// fakeRetentionStore replays a script of results, one per call. Once the
// script is used up it returns 0 rows. If block is set, a call made after the
// script is used up waits for ctx to be cancelled and returns its error.
type fakeRetentionStore struct {
	mu      sync.Mutex
	results []retentionResult
	calls   []retentionCall
	block   bool
}

type retentionResult struct {
	n   int64
	err error
}

func (f *fakeRetentionStore) DeleteExpiredChecks(ctx context.Context, retentionDays, batchSize int32) (int64, error) {
	f.mu.Lock()
	f.calls = append(f.calls, retentionCall{retentionDays, batchSize})
	if len(f.results) == 0 {
		block := f.block
		f.mu.Unlock()
		if block {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return 0, nil
	}
	r := f.results[0]
	f.results = f.results[1:]
	f.mu.Unlock()
	return r.n, r.err
}

func (f *fakeRetentionStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type retentionHarness struct {
	logs   *syncBuffer
	ticks  chan time.Time
	cancel context.CancelFunc
	done   chan struct{}
}

// startRetention runs a retention runner whose ticks the test sends by hand.
func startRetention(t *testing.T, store scheduler.RetentionStore) *retentionHarness {
	t.Helper()

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := scheduler.NewRetention(store, testRetentionDays, log)
	ticks := make(chan time.Time)
	r.SetRetentionTicks(ticks)

	ctx, cancel := context.WithCancel(context.Background())
	h := &retentionHarness{logs: logs, ticks: ticks, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(waitLimit):
			t.Error("Retention.Run did not return at cleanup")
		}
	})
	return h
}

// tick triggers one run; it blocks until the runner is idle and receives it.
func (h *retentionHarness) tick(t *testing.T) {
	t.Helper()
	select {
	case h.ticks <- time.Now():
	case <-time.After(waitLimit):
		t.Fatal("retention runner did not take the tick")
	}
}

// messages returns the log entries with the given msg.
func (h *retentionHarness) messages(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range h.logs.entries(t) {
		if e["msg"] == msg {
			out = append(out, e)
		}
	}
	return out
}

func TestRetentionBatchesUntilShortBatch(t *testing.T) {
	full := int64(scheduler.RetentionBatchSize)
	store := &fakeRetentionStore{results: []retentionResult{{n: full}, {n: full}, {n: 12}}}
	h := startRetention(t, store)

	// The startup run needs no tick.
	waitFor(t, "startup run to finish", func() bool {
		return len(h.messages(t, "retention cleanup finished")) == 1
	})

	if got := store.callCount(); got != 3 {
		t.Fatalf("DeleteExpiredChecks calls = %d, want 3 (two full batches, one short)", got)
	}
	for i, c := range store.calls {
		if c.retentionDays != testRetentionDays || c.batchSize != scheduler.RetentionBatchSize {
			t.Errorf("call %d = %+v, want retention %d, batch %d",
				i, c, testRetentionDays, scheduler.RetentionBatchSize)
		}
	}
	entry := h.messages(t, "retention cleanup finished")[0]
	if entry["level"] != "INFO" {
		t.Errorf("finished log level = %v, want INFO", entry["level"])
	}
	if got, want := entry["deleted"], float64(2*full+12); got != want {
		t.Errorf("deleted = %v, want %v", got, want)
	}

	// A tick starts a new run; an empty table is a single short batch.
	h.tick(t)
	waitFor(t, "ticked run to finish", func() bool {
		return len(h.messages(t, "retention cleanup finished")) == 2
	})
	if got := store.callCount(); got != 4 {
		t.Errorf("DeleteExpiredChecks calls = %d after the tick, want 4", got)
	}
	if got := h.messages(t, "retention cleanup finished")[1]["deleted"]; got != float64(0) {
		t.Errorf("second run deleted = %v, want 0", got)
	}
}

func TestRetentionErrorIsRetriedNextTick(t *testing.T) {
	full := int64(scheduler.RetentionBatchSize)
	store := &fakeRetentionStore{results: []retentionResult{
		{n: full},
		{err: errors.New("connection reset")},
		{n: 7},
	}}
	h := startRetention(t, store)

	waitFor(t, "startup run to fail", func() bool {
		return len(h.messages(t, "retention cleanup failed, retrying next tick")) == 1
	})
	failed := h.messages(t, "retention cleanup failed, retrying next tick")[0]
	if failed["level"] != "ERROR" {
		t.Errorf("failure log level = %v, want ERROR", failed["level"])
	}
	if failed["deleted"] != float64(full) {
		t.Errorf("failure log deleted = %v, want %d deleted before the error", failed["deleted"], full)
	}
	if got := store.callCount(); got != 2 {
		t.Fatalf("calls after the failed run = %d, want 2: an error ends the run", got)
	}
	if n := len(h.messages(t, "retention cleanup finished")); n != 0 {
		t.Errorf("finished logged %d times for a failed run, want 0", n)
	}

	h.tick(t)
	waitFor(t, "retry to finish", func() bool {
		return len(h.messages(t, "retention cleanup finished")) == 1
	})
	if got := h.messages(t, "retention cleanup finished")[0]["deleted"]; got != float64(7) {
		t.Errorf("retry deleted = %v, want 7", got)
	}
	if got := store.callCount(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestRetentionStopsOnCancel(t *testing.T) {
	t.Run("while idle", func(t *testing.T) {
		store := &fakeRetentionStore{}
		h := startRetention(t, store)
		waitFor(t, "startup run to finish", func() bool {
			return len(h.messages(t, "retention cleanup finished")) == 1
		})

		h.cancel()
		select {
		case <-h.done:
		case <-time.After(waitLimit):
			t.Fatal("Run did not return after cancel")
		}
		if got := store.callCount(); got != 1 {
			t.Errorf("calls = %d, want 1", got)
		}
		if n := len(h.messages(t, "retention stopped")); n != 1 {
			t.Errorf("stopped logged %d times, want 1", n)
		}
	})

	t.Run("mid run", func(t *testing.T) {
		full := int64(scheduler.RetentionBatchSize)
		store := &fakeRetentionStore{results: []retentionResult{{n: full}}, block: true}
		h := startRetention(t, store)
		waitFor(t, "second batch to start", func() bool { return store.callCount() == 2 })

		h.cancel()
		select {
		case <-h.done:
		case <-time.After(waitLimit):
			t.Fatal("Run did not return after cancel")
		}
		if n := len(h.messages(t, "retention cleanup failed, retrying next tick")); n != 0 {
			t.Errorf("a cancelled batch was logged as a failure %d times, want 0", n)
		}
		if n := len(h.messages(t, "retention cleanup interrupted by shutdown")); n != 1 {
			t.Errorf("interrupted logged %d times, want 1", n)
		}
		if got := store.callCount(); got != 2 {
			t.Errorf("calls = %d, want 2: no batch after cancel", got)
		}
	})
}

func TestNewRetentionPanicsOnInvalidDays(t *testing.T) {
	for _, days := range []int{0, -1, 366} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewRetention(days=%d) did not panic", days)
				}
			}()
			scheduler.NewRetention(&fakeRetentionStore{}, days, slog.New(slog.DiscardHandler))
		}()
	}
}
