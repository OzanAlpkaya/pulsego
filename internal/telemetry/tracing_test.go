package telemetry_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// The tests in this file and in pgxtracer_test.go replace the global tracer
// provider and propagator, so none of them runs in parallel.

// restoreGlobals puts the OpenTelemetry globals back after a test.
func restoreGlobals(t *testing.T) {
	t.Helper()
	tp, prop := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(prop)
	})
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// otlpReceiver is a minimal OTLP/HTTP trace receiver: it decodes every
// protobuf export request it gets.
type otlpReceiver struct {
	mu       sync.Mutex
	paths    []string
	requests []*collectortrace.ExportTraceServiceRequest
}

func (r *otlpReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := &collectortrace.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(body, msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.requests = append(r.requests, msg)
	r.mu.Unlock()

	resp, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(resp)
}

func TestTracingExportsToOTLPReceiver(t *testing.T) {
	restoreGlobals(t)

	recv := &otlpReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()

	// The base URL has no path: the exporter must still post to /v1/traces.
	shutdown, err := telemetry.SetupTracing(context.Background(), telemetry.TracingConfig{
		Enabled:     true,
		Endpoint:    srv.URL,
		ServiceName: "pulsego-test",
		Environment: "test",
	}, discardLogger())
	if err != nil {
		t.Fatalf("SetupTracing() error: %v", err)
	}

	_, span := telemetry.Tracer().Start(context.Background(), "test.span")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown() error: %v", err)
	}

	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.requests) == 0 {
		t.Fatal("receiver got no export request")
	}
	for _, p := range recv.paths {
		if p != "/v1/traces" {
			t.Errorf("export posted to %q, want /v1/traces", p)
		}
	}

	var sawSpan bool
	for _, req := range recv.requests {
		for _, rs := range req.GetResourceSpans() {
			attrs := map[string]string{}
			for _, kv := range rs.GetResource().GetAttributes() {
				attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
			if attrs["service.name"] != "pulsego-test" {
				t.Errorf("service.name = %q, want pulsego-test", attrs["service.name"])
			}
			if attrs["deployment.environment"] != "test" {
				t.Errorf("deployment.environment = %q, want test", attrs["deployment.environment"])
			}
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					if s.GetName() == "test.span" {
						sawSpan = true
					}
				}
			}
		}
	}
	if !sawSpan {
		t.Error("span test.span not exported")
	}

	// The W3C propagator is installed globally.
	fields := otel.GetTextMapPropagator().Fields()
	if !containsAll(fields, "traceparent", "baggage") {
		t.Errorf("propagator fields = %v, want traceparent and baggage", fields)
	}
}

func TestTracingDisabledMakesNoRequests(t *testing.T) {
	restoreGlobals(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	// The standard variable must not switch export on behind our back.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

	shutdown, err := telemetry.SetupTracing(context.Background(), telemetry.TracingConfig{
		Enabled:  false,
		Endpoint: srv.URL,
	}, discardLogger())
	if err != nil {
		t.Fatalf("SetupTracing() error: %v", err)
	}

	_, span := telemetry.Tracer().Start(context.Background(), "ignored")
	if span.SpanContext().IsValid() {
		t.Error("disabled tracing produced a recordable span")
	}
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("disabled tracing made %d requests, want 0", n)
	}
}

func TestTracingUnreachableEndpointDoesNotBlock(t *testing.T) {
	restoreGlobals(t)

	// Reserve a port and close it, so connections are refused.
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	endpoint := "http://" + ln.Addr().String()
	_ = ln.Close()

	start := time.Now()
	shutdown, err := telemetry.SetupTracing(context.Background(), telemetry.TracingConfig{
		Enabled:     true,
		Endpoint:    endpoint,
		ServiceName: "pulsego-test",
		Environment: "test",
	}, discardLogger())
	if err != nil {
		t.Fatalf("SetupTracing() error: %v", err)
	}
	for range 100 {
		_, span := telemetry.Tracer().Start(context.Background(), "unexported")
		span.End()
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("setup and span creation took %v with an unreachable collector, want well under 1s", elapsed)
	}

	// Shutdown is bounded by the caller's deadline even though the exporter
	// keeps retrying; its error is for logging only.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	shutdownStart := time.Now()
	_ = shutdown(ctx)
	if elapsed := time.Since(shutdownStart); elapsed > 3*time.Second {
		t.Errorf("shutdown took %v, want it bounded by the 500ms deadline", elapsed)
	}
}

func TestSetupTracingRejectsInvalidEndpoint(t *testing.T) {
	restoreGlobals(t)

	shutdown, err := telemetry.SetupTracing(context.Background(), telemetry.TracingConfig{
		Enabled:  true,
		Endpoint: "otel-collector:4318",
	}, discardLogger())
	if err == nil {
		t.Fatal("SetupTracing() succeeded with a scheme-less endpoint, want an error")
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil, want a callable no-op")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown error: %v", err)
	}
}

func TestTracesEndpointURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://otel-collector:4318", want: "http://otel-collector:4318/v1/traces"},
		{in: "http://otel-collector:4318/", want: "http://otel-collector:4318/v1/traces"},
		{in: "https://otel.example.com/otlp", want: "https://otel.example.com/otlp/v1/traces"},
		{in: "https://otel.example.com/otlp/", want: "https://otel.example.com/otlp/v1/traces"},
		{in: "http://localhost:4318", want: "http://localhost:4318/v1/traces"},
		{in: "", wantErr: true},
		{in: "otel-collector:4318", wantErr: true},
		{in: "grpc://otel-collector:4317", wantErr: true},
		{in: "http://:4318", wantErr: true},
		{in: "http://bad host", wantErr: true},
	}
	for _, tt := range tests {
		got, err := telemetry.TracesEndpointURL(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("TracesEndpointURL(%q) = %q, want an error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("TracesEndpointURL(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}
