package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// tracedChecker returns a checker whose spans go to a private recorder, so the
// test does not touch the global provider and may run in parallel.
func tracedChecker(t *testing.T, policy security.Policy) (*Checker, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	c := New(policy, WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))))
	t.Cleanup(c.CloseIdleConnections)
	return c, rec
}

func onlySpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := rec.Ended()
	if len(ended) != 1 || ended[0].Name() != spanCheck {
		t.Fatalf("ended spans = %d, want exactly one %q", len(ended), spanCheck)
	}
	return ended[0]
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value)
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

// assertNoURLParts fails if any attribute or the status carries the path or
// query of the target URL.
func assertNoURLParts(t *testing.T, s sdktrace.ReadOnlySpan, parts ...string) {
	t.Helper()
	texts := []string{s.Status().Description}
	for _, kv := range s.Attributes() {
		texts = append(texts, kv.Value.String())
	}
	for _, text := range texts {
		for _, p := range parts {
			if strings.Contains(text, p) {
				t.Errorf("span carries %q in %q", p, text)
			}
		}
	}
}

func TestCheckSpanOnSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c, rec := tracedChecker(t, allowLoopback)
	parentCtx, parent := sdktrace.NewTracerProvider().Tracer("test").Start(context.Background(), "parent")
	if _, err := c.Check(parentCtx, testMonitor(srv.URL+"/secret-path?token=hunter2")); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	parent.End()

	s := onlySpan(t, rec)
	if s.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", s.SpanKind())
	}
	if s.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("checker.check is not a child of the caller's span")
	}
	a := spanAttrs(s)
	if got := a["server.address"].AsString(); got != "127.0.0.1" {
		t.Errorf("server.address = %q, want 127.0.0.1 (host only)", got)
	}
	if got := a["http.response.status_code"].AsInt64(); got != http.StatusNoContent {
		t.Errorf("http.response.status_code = %d, want 204", got)
	}
	if _, ok := a["pulsego.error_kind"]; ok {
		t.Error("pulsego.error_kind set on a successful check")
	}
	if s.Status().Code == codes.Error {
		t.Errorf("status = error on a successful check")
	}
	assertNoURLParts(t, s, "secret-path", "hunter2", srv.URL)
}

func TestCheckSpanOnUnexpectedStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c, rec := tracedChecker(t, allowLoopback)
	mustCheck(t, c, testMonitor(srv.URL+"/x?k=v"))

	s := onlySpan(t, rec)
	a := spanAttrs(s)
	if got := a["http.response.status_code"].AsInt64(); got != http.StatusServiceUnavailable {
		t.Errorf("http.response.status_code = %d, want 503", got)
	}
	if got := a["pulsego.error_kind"].AsString(); got != "unexpected_status" {
		t.Errorf("pulsego.error_kind = %q, want unexpected_status", got)
	}
	if s.Status().Code != codes.Error {
		t.Errorf("status = %v, want error", s.Status().Code)
	}
}

func TestCheckSpanOnBlockedTarget(t *testing.T) {
	t.Parallel()
	c, rec := tracedChecker(t, security.Policy{})
	out := mustCheck(t, c, testMonitor("http://127.0.0.1/admin?key=s3cret"))
	if out.Success {
		t.Fatal("loopback target was not blocked")
	}

	s := onlySpan(t, rec)
	a := spanAttrs(s)
	if got := a["pulsego.error_kind"].AsString(); got != "blocked" {
		t.Errorf("pulsego.error_kind = %q, want blocked", got)
	}
	if _, ok := a["http.response.status_code"]; ok {
		t.Error("status code set although nothing was requested")
	}
	assertNoURLParts(t, s, "admin", "s3cret")
}
