package scheduler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/OzanAlpkaya/pulsego/internal/checker"
	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
	"github.com/OzanAlpkaya/pulsego/internal/security"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// fakeMetrics records every call of the scheduler.Metrics interface.
type fakeMetrics struct {
	mu          sync.Mutex
	checks      []checkObs
	transitions [][2]domain.State
	claimed     int
	claimErrors int
	active      int
	maxActive   int
	activeIncs  int
	activeDecs  int
	lags        []time.Duration
}

type checkObs struct {
	success bool
	kind    string
	d       time.Duration
}

var _ scheduler.Metrics = (*fakeMetrics)(nil)

func (f *fakeMetrics) ObserveCheck(success bool, kind *domain.ErrorKind, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := ""
	if kind != nil {
		k = string(*kind)
	}
	f.checks = append(f.checks, checkObs{success: success, kind: k, d: d})
}

func (f *fakeMetrics) StateChanged(from, to domain.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, [2]domain.State{from, to})
}

func (f *fakeMetrics) AddClaimed(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed += n
}

func (f *fakeMetrics) ClaimError() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimErrors++
}

func (f *fakeMetrics) ActiveChecksInc() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active++
	f.activeIncs++
	f.maxActive = max(f.maxActive, f.active)
}

func (f *fakeMetrics) ActiveChecksDec() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active--
	f.activeDecs++
}

func (f *fakeMetrics) ObserveSchedulerLag(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lags = append(f.lags, d)
}

func (f *fakeMetrics) get(read func(*fakeMetrics)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	read(f)
}

func metricsConfig(m scheduler.Metrics, concurrency, batch int, shutdown time.Duration) scheduler.Config {
	return scheduler.Config{
		WorkerID:        testWorkerID,
		Concurrency:     concurrency,
		BatchSize:       batch,
		PollInterval:    testPoll,
		LeaseDuration:   time.Minute,
		ShutdownTimeout: shutdown,
		Metrics:         m,
	}
}

func TestMetricsForOneRecordedCheck(t *testing.T) {
	t.Parallel()
	store := newFakeStore(1)
	store.lag = 2 * time.Second
	store.change = domain.StateChange{From: domain.StateDegraded, To: domain.StateDown, Incident: domain.IncidentOpen}
	chk := newGateChecker()
	m := &fakeMetrics{}
	h := startWith(t, store, chk, metricsConfig(m, 2, 2, testShutdown))

	waitFor(t, "a running check", func() bool { return chk.running.Load() == 1 })
	m.get(func(m *fakeMetrics) {
		if m.active != 1 {
			t.Errorf("active checks during the check = %d, want 1", m.active)
		}
	})
	chk.gate <- struct{}{}
	waitFor(t, "the result recorded", func() bool { return store.snapshot().records == 1 })
	h.stop(t)

	m.get(func(m *fakeMetrics) {
		if m.claimed != 1 {
			t.Errorf("claimed = %d, want 1", m.claimed)
		}
		if len(m.lags) != 1 || m.lags[0] < 2*time.Second || m.lags[0] > 2*time.Second+waitLimit {
			t.Errorf("lags = %v, want one of about 2s", m.lags)
		}
		if m.claimErrors != 0 {
			t.Errorf("claim errors = %d, want 0", m.claimErrors)
		}
		if m.activeIncs != 1 || m.activeDecs != 1 || m.active != 0 {
			t.Errorf("active checks inc/dec/now = %d/%d/%d, want 1/1/0", m.activeIncs, m.activeDecs, m.active)
		}
		// gateChecker reports success with LatencyMS 1.
		if len(m.checks) != 1 || m.checks[0] != (checkObs{success: true, d: time.Millisecond}) {
			t.Errorf("checks = %+v, want one success of 1ms", m.checks)
		}
		if len(m.transitions) != 1 || m.transitions[0] != [2]domain.State{domain.StateDegraded, domain.StateDown} {
			t.Errorf("transitions = %v, want [degraded down]", m.transitions)
		}
	})
}

func TestMetricsNegativeLagIsClamped(t *testing.T) {
	t.Parallel()
	store := newFakeStore(1)
	store.lag = -time.Hour // next_check_at in the future: clock skew
	chk := newGateChecker()
	m := &fakeMetrics{}
	h := startWith(t, store, chk, metricsConfig(m, 1, 1, testShutdown))

	waitFor(t, "a running check", func() bool { return chk.running.Load() == 1 })
	chk.gate <- struct{}{}
	waitFor(t, "the result recorded", func() bool { return store.snapshot().records == 1 })
	h.stop(t)

	m.get(func(m *fakeMetrics) {
		if len(m.lags) != 1 || m.lags[0] != 0 {
			t.Errorf("lags = %v, want [0]", m.lags)
		}
	})
}

func TestMetricsSelfLoopIsNotAStateChange(t *testing.T) {
	t.Parallel()
	store := newFakeStore(1)
	store.change = domain.StateChange{From: domain.StateUp, To: domain.StateUp}
	chk := newGateChecker()
	m := &fakeMetrics{}
	h := startWith(t, store, chk, metricsConfig(m, 1, 1, testShutdown))

	waitFor(t, "a running check", func() bool { return chk.running.Load() == 1 })
	chk.gate <- struct{}{}
	waitFor(t, "the result recorded", func() bool { return store.snapshot().records == 1 })
	h.stop(t)

	m.get(func(m *fakeMetrics) {
		if len(m.transitions) != 0 {
			t.Errorf("transitions = %v, want none for a self-loop", m.transitions)
		}
		if len(m.checks) != 1 {
			t.Errorf("checks = %d, want 1", len(m.checks))
		}
	})
}

func TestMetricsCancelledCheckIsNotObserved(t *testing.T) {
	t.Parallel()
	store := newFakeStore(1)
	chk := newGateChecker()
	m := &fakeMetrics{}
	h := startWith(t, store, chk, metricsConfig(m, 1, 1, testShutdown))

	waitFor(t, "a running check", func() bool { return chk.running.Load() == 1 })
	h.stop(t) // the shutdown timeout cancels the blocked check

	m.get(func(m *fakeMetrics) {
		if len(m.checks) != 0 {
			t.Errorf("checks = %+v, want none for a cancelled check", m.checks)
		}
		if m.activeIncs != 1 || m.activeDecs != 1 {
			t.Errorf("active checks inc/dec = %d/%d, want 1/1", m.activeIncs, m.activeDecs)
		}
	})
}

func TestMetricsClaimErrorIsCounted(t *testing.T) {
	t.Parallel()
	store := &flakyStore{fakeStore: newFakeStore(1)}
	chk := newGateChecker()
	m := &fakeMetrics{}
	rec, tp := newRecorder()
	cfg := metricsConfig(m, 1, 1, waitLimit)
	cfg.TracerProvider = tp
	h := startWith(t, store, chk, cfg)

	waitFor(t, "a check after the failed claim", func() bool { return chk.running.Load() == 1 })
	chk.gate <- struct{}{}
	waitFor(t, "the result recorded", func() bool { return store.snapshot().records == 1 })
	h.stop(t)

	m.get(func(m *fakeMetrics) {
		if m.claimErrors != 1 {
			t.Errorf("claim errors = %d, want 1", m.claimErrors)
		}
		if m.claimed != 1 {
			t.Errorf("claimed = %d, want 1", m.claimed)
		}
	})

	var failed int
	for _, s := range spansNamed(rec, "scheduler.claim") {
		if s.Status().Code == codes.Error {
			failed++
			if strings.Contains(s.Status().Description, "unreachable") {
				t.Errorf("claim span status carries the driver error: %q", s.Status().Description)
			}
		}
	}
	if failed != 1 {
		t.Errorf("%d failed scheduler.claim spans, want 1", failed)
	}
}

// TestTelemetryMetricsSatisfyInterface runs the scheduler against the real
// Prometheus metrics and reads the result back from the registry.
func TestTelemetryMetricsSatisfyInterface(t *testing.T) {
	t.Parallel()
	store := newFakeStore(1)
	chk := newGateChecker()
	metrics := telemetry.NewMetrics()
	h := startWith(t, store, chk, metricsConfig(metrics, 1, 1, testShutdown))

	waitFor(t, "a running check", func() bool { return chk.running.Load() == 1 })
	chk.gate <- struct{}{}
	waitFor(t, "the result recorded", func() bool { return store.snapshot().records == 1 })
	h.stop(t)

	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]float64{}
	for _, f := range families {
		for _, mt := range f.GetMetric() {
			switch {
			case mt.GetCounter() != nil:
				got[f.GetName()] += mt.GetCounter().GetValue()
			case mt.GetGauge() != nil:
				got[f.GetName()] += mt.GetGauge().GetValue()
			case mt.GetHistogram() != nil:
				got[f.GetName()] += float64(mt.GetHistogram().GetSampleCount())
			}
		}
	}
	want := map[string]float64{
		"pulsego_worker_claimed_monitors_total":  1,
		"pulsego_scheduler_lag_seconds":          1,
		"pulsego_monitor_checks_total":           1,
		"pulsego_monitor_check_duration_seconds": 1,
		"pulsego_worker_active_checks":           0,
		"pulsego_worker_claim_errors_total":      0,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %v, want %v", name, got[name], w)
		}
	}
}

func newRecorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	return rec, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
}

func spansNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func oneSpan(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	found := spansNamed(rec, name)
	if len(found) != 1 {
		t.Fatalf("%d spans named %q, want 1", len(found), name)
	}
	return found[0]
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value)
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func eventNames(s sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, e := range s.Events() {
		out = append(out, e.Name)
	}
	return out
}

// runTracedCheck checks one monitor against an httptest target with the real
// checker and returns the recorded spans.
func runTracedCheck(t *testing.T, change domain.StateChange) *tracetest.SpanRecorder {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rec, tp := newRecorder()
	chk := checker.New(security.Policy{AllowPrivate: true}, checker.WithTracerProvider(tp))
	t.Cleanup(chk.CloseIdleConnections)

	store := newFakeStore(1)
	store.url = srv.URL + "/health?token=secret"
	store.change = change
	cfg := metricsConfig(nil, 1, 1, waitLimit)
	cfg.TracerProvider = tp
	h := startWith(t, store, chk, cfg)

	waitFor(t, "the result recorded", func() bool { return store.snapshot().records == 1 })
	h.stop(t)
	return rec
}

func TestSpansForOneCheck(t *testing.T) {
	t.Parallel()
	rec := runTracedCheck(t, domain.StateChange{
		From: domain.StateDegraded, To: domain.StateDown, Incident: domain.IncidentOpen,
	})

	var claim sdktrace.ReadOnlySpan
	for _, s := range spansNamed(rec, "scheduler.claim") {
		if spanAttrs(s)["pulsego.claimed"].AsInt64() == 1 {
			claim = s
		}
	}
	if claim == nil {
		t.Fatal("no scheduler.claim span with pulsego.claimed=1")
	}

	check := oneSpan(t, rec, "scheduler.check")
	if check.Parent().IsValid() {
		t.Error("scheduler.check has a parent, want a new root")
	}
	if links := check.Links(); len(links) != 1 || links[0].SpanContext.SpanID() != claim.SpanContext().SpanID() {
		t.Errorf("scheduler.check links = %v, want one link to the claim span", links)
	}
	if _, ok := spanAttrs(check)["pulsego.monitor.id"]; !ok {
		t.Error("scheduler.check has no pulsego.monitor.id attribute")
	}

	httpSpan := oneSpan(t, rec, "checker.check")
	if httpSpan.Parent().SpanID() != check.SpanContext().SpanID() {
		t.Error("checker.check is not a child of scheduler.check")
	}
	if got := spanAttrs(httpSpan)["http.response.status_code"].AsInt64(); got != http.StatusOK {
		t.Errorf("checker.check status code = %d, want 200", got)
	}

	record := oneSpan(t, rec, "monitor.record")
	if record.Parent().SpanID() != check.SpanContext().SpanID() {
		t.Error("monitor.record is not a child of scheduler.check")
	}
	a := spanAttrs(record)
	if a["state.from"].AsString() != "degraded" || a["state.to"].AsString() != "down" {
		t.Errorf("monitor.record state = %v -> %v, want degraded -> down", a["state.from"], a["state.to"])
	}
	if ev := eventNames(record); len(ev) != 1 || ev[0] != "incident.opened" {
		t.Errorf("monitor.record events = %v, want [incident.opened]", ev)
	}

	// No span may carry the monitor URL or its query string.
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if v := kv.Value.String(); strings.Contains(v, "token") || strings.Contains(v, "/health") {
				t.Errorf("span %s attribute %s carries the URL: %q", s.Name(), kv.Key, v)
			}
		}
	}
}

func TestRecordSpanIncidentResolved(t *testing.T) {
	t.Parallel()
	rec := runTracedCheck(t, domain.StateChange{
		From: domain.StateDown, To: domain.StateUp, Incident: domain.IncidentResolve,
	})
	if ev := eventNames(oneSpan(t, rec, "monitor.record")); len(ev) != 1 || ev[0] != "incident.resolved" {
		t.Errorf("monitor.record events = %v, want [incident.resolved]", ev)
	}
}

func TestRecordSpanNoEventWithoutIncident(t *testing.T) {
	t.Parallel()
	rec := runTracedCheck(t, domain.StateChange{From: domain.StateUp, To: domain.StateUp})
	record := oneSpan(t, rec, "monitor.record")
	if ev := eventNames(record); len(ev) != 0 {
		t.Errorf("monitor.record events = %v, want none", ev)
	}
	if a := spanAttrs(record); a["state.from"].AsString() != "up" || a["state.to"].AsString() != "up" {
		t.Errorf("monitor.record state = %v -> %v, want up -> up", a["state.from"], a["state.to"])
	}
}

func TestRecordSpanLeaseLost(t *testing.T) {
	t.Parallel()
	rec, tp := newRecorder()
	store := newFakeStore(1)
	store.recordErr = scheduler.ErrLeaseLost
	chk := newGateChecker()
	cfg := metricsConfig(nil, 1, 1, testShutdown)
	cfg.TracerProvider = tp
	h := startWith(t, store, chk, cfg)

	waitFor(t, "a running check", func() bool { return chk.running.Load() == 1 })
	chk.gate <- struct{}{}
	waitFor(t, "the record attempt", func() bool { return store.snapshot().records == 1 })
	h.stop(t)

	record := oneSpan(t, rec, "monitor.record")
	if record.Status().Code != codes.Error || record.Status().Description != "lease lost" {
		t.Errorf("monitor.record status = %v, want error \"lease lost\"", record.Status())
	}
	if len(record.Events()) != 0 {
		t.Errorf("monitor.record events = %v, want none", eventNames(record))
	}
}
