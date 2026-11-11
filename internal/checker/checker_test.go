package checker

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

func testMonitor(url string) domain.Monitor {
	return domain.Monitor{
		URL:               url,
		TimeoutMS:         2000,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 399,
	}
}

func mustCheck(t *testing.T, c *Checker, m domain.Monitor) domain.CheckOutcome {
	t.Helper()
	out, err := c.Check(context.Background(), m)
	if err != nil {
		t.Fatalf("Check() error = %v, want nil", err)
	}
	return out
}

func assertFailure(t *testing.T, out domain.CheckOutcome, want domain.ErrorKind) {
	t.Helper()
	if out.Success {
		t.Fatalf("Success = true, want false (%s)", want)
	}
	if out.ErrorKind == nil || *out.ErrorKind != want {
		got := "<nil>"
		if out.ErrorKind != nil {
			got = string(*out.ErrorKind)
		}
		msg := "<nil>"
		if out.ErrorMessage != nil {
			msg = *out.ErrorMessage
		}
		t.Fatalf("ErrorKind = %s, want %s (message %q)", got, want, msg)
	}
	if out.ErrorMessage == nil || *out.ErrorMessage == "" {
		t.Fatalf("ErrorMessage is empty, want a message")
	}
}

func TestCheckSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	out := mustCheck(t, New(), testMonitor(srv.URL))
	if !out.Success {
		t.Fatalf("Success = false, want true (kind %v)", out.ErrorKind)
	}
	if out.StatusCode == nil || *out.StatusCode != http.StatusNoContent {
		t.Fatalf("StatusCode = %v, want 204", out.StatusCode)
	}
	if out.ErrorKind != nil || out.ErrorMessage != nil {
		t.Fatalf("ErrorKind/ErrorMessage = %v/%v, want nil/nil", out.ErrorKind, out.ErrorMessage)
	}
	if out.LatencyMS < 0 {
		t.Fatalf("LatencyMS = %d, want >= 0", out.LatencyMS)
	}
}

func TestCheckUnexpectedStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	out := mustCheck(t, New(), testMonitor(srv.URL))
	assertFailure(t, out, domain.ErrorKindUnexpectedStatus)
	if out.StatusCode == nil || *out.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode = %v, want 503", out.StatusCode)
	}
	if got, want := *out.ErrorMessage, "status 503 outside expected range 200-399"; got != want {
		t.Fatalf("ErrorMessage = %q, want %q", got, want)
	}
}

func TestCheckTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	m := testMonitor(srv.URL)
	m.TimeoutMS = 200

	start := time.Now()
	out := mustCheck(t, New(), m)
	elapsed := time.Since(start)

	assertFailure(t, out, domain.ErrorKindTimeout)
	if elapsed >= time.Second {
		t.Fatalf("Check took %v, want well under 2s", elapsed)
	}
	if out.LatencyMS < 200 || out.LatencyMS >= 1000 {
		t.Fatalf("LatencyMS = %d, want about the 200ms timeout", out.LatencyMS)
	}
	if out.StatusCode != nil {
		t.Fatalf("StatusCode = %d, want nil", *out.StatusCode)
	}
}

func TestCheckConnectionRefused(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	out := mustCheck(t, New(), testMonitor("http://"+addr+"/"))
	assertFailure(t, out, domain.ErrorKindConnection)
}

func TestCheckUntrustedTLS(t *testing.T) {
	t.Parallel()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // silence handshake errors
	srv.StartTLS()
	t.Cleanup(srv.Close)

	out := mustCheck(t, New(), testMonitor(srv.URL))
	assertFailure(t, out, domain.ErrorKindTLS)
}

// redirectServer redirects /n to /n-1 and answers 200 at /0.
func redirectServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || n < 0 {
			http.Redirect(w, r, r.URL.Path, http.StatusFound) // loop forever
			return
		}
		if n == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/"+strconv.Itoa(n-1), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckRedirectLoop(t *testing.T) {
	t.Parallel()

	srv := redirectServer(t)
	out := mustCheck(t, New(), testMonitor(srv.URL+"/loop?token=secret"))
	assertFailure(t, out, domain.ErrorKindRedirect)
	if strings.Contains(*out.ErrorMessage, "secret") || strings.Contains(*out.ErrorMessage, srv.URL) {
		t.Fatalf("ErrorMessage %q contains the URL", *out.ErrorMessage)
	}
}

func TestCheckRedirectLimit(t *testing.T) {
	t.Parallel()

	srv := redirectServer(t)
	c := New()

	out := mustCheck(t, c, testMonitor(srv.URL+"/"+strconv.Itoa(MaxRedirects)))
	if !out.Success {
		t.Fatalf("%d redirects: Success = false, want true (kind %v)", MaxRedirects, *out.ErrorKind)
	}

	out = mustCheck(t, c, testMonitor(srv.URL+"/"+strconv.Itoa(MaxRedirects+1)))
	assertFailure(t, out, domain.ErrorKindRedirect)
}

func TestCheckSendsUserAgentAndGET(t *testing.T) {
	t.Parallel()

	type seen struct{ method, ua string }
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Method, r.Header.Get("User-Agent")}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	mustCheck(t, New(), testMonitor(srv.URL))
	s := <-got
	if s.method != http.MethodGet {
		t.Errorf("method = %q, want GET", s.method)
	}
	if s.ua != "PulseGo/1.0" {
		t.Errorf("User-Agent = %q, want PulseGo/1.0", s.ua)
	}
}

func TestCheckBodyNotRetained(t *testing.T) {
	t.Parallel()

	const marker = "BODY-MARKER-do-not-store"
	const total = 64 << 20 // far beyond the drain limit and socket buffers

	written := make(chan int64, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/small" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, marker)
			return
		}
		w.WriteHeader(http.StatusOK)
		chunk := []byte(strings.Repeat(marker, 1024))
		var n int64
		for n < total {
			k, err := w.Write(chunk)
			n += int64(k)
			if err != nil {
				break
			}
		}
		written <- n
	}))
	t.Cleanup(srv.Close)

	c := New()

	out := mustCheck(t, c, testMonitor(srv.URL+"/small"))
	assertFailure(t, out, domain.ErrorKindUnexpectedStatus)
	if strings.Contains(*out.ErrorMessage, marker) {
		t.Fatalf("ErrorMessage %q contains the response body", *out.ErrorMessage)
	}

	out = mustCheck(t, c, testMonitor(srv.URL+"/large"))
	if !out.Success {
		t.Fatalf("Success = false, want true (kind %v)", *out.ErrorKind)
	}
	select {
	case n := <-written:
		if n >= total {
			t.Fatalf("server wrote the whole %d-byte body; the checker read past its drain limit", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish: the connection was not closed after draining")
	}
}

func TestCheckParentCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	out, err := New().Check(ctx, testMonitor(srv.URL))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Check() error = %v, want context.Canceled", err)
	}
	if out != (domain.CheckOutcome{}) {
		t.Fatalf("Check() outcome = %+v, want zero value", out)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("Check took %v after cancellation", elapsed)
	}

	// An already-cancelled context never sends a request.
	out, err = New().Check(ctx, testMonitor("http://invalid.invalid/"))
	if !errors.Is(err, context.Canceled) || out != (domain.CheckOutcome{}) {
		t.Fatalf("Check(cancelled) = %+v, %v; want zero outcome and context.Canceled", out, err)
	}
}

func TestCheckUsesDialHook(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var calls atomic.Int32
	var d net.Dialer
	c := New(WithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls.Add(1)
		return d.DialContext(ctx, network, addr)
	}))

	out := mustCheck(t, c, testMonitor(srv.URL))
	if !out.Success {
		t.Fatalf("Success = false, want true")
	}
	if calls.Load() == 0 {
		t.Fatal("custom dialer was not used")
	}

	refused := New(WithDialContext(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("blocked by policy")
	}))
	out = mustCheck(t, refused, testMonitor(srv.URL))
	assertFailure(t, out, domain.ErrorKindUnknown)
}

func TestCheckInvalidURL(t *testing.T) {
	t.Parallel()

	out := mustCheck(t, New(), testMonitor("http://exa mple.com/?token=secret"))
	assertFailure(t, out, domain.ErrorKindUnknown)
	if strings.Contains(*out.ErrorMessage, "secret") {
		t.Fatalf("ErrorMessage %q contains the URL", *out.ErrorMessage)
	}
}

func TestTransportSettings(t *testing.T) {
	t.Parallel()

	c := New()
	tr := c.transport
	if tr.Proxy != nil {
		t.Error("Proxy is set, want nil")
	}
	if tr.DialContext == nil {
		t.Error("DialContext is nil")
	}
	if tr.TLSHandshakeTimeout != 10*time.Second || tr.MaxIdleConns != 100 ||
		tr.MaxIdleConnsPerHost != 4 || tr.IdleConnTimeout != 90*time.Second || !tr.ForceAttemptHTTP2 {
		t.Errorf("transport settings = %+v", tr)
	}
	if c.client.Timeout != 0 {
		t.Errorf("client Timeout = %v, want 0", c.client.Timeout)
	}
	if c.client.Transport != tr {
		t.Error("client does not use the shared transport")
	}
}
