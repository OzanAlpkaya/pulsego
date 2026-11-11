package checker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/security"
)

const publicIP = "93.184.215.14"

// fakeResolver answers from a fixed table; unknown names are NXDOMAIN.
type fakeResolver map[string][]string

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	answers, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

// recordingDial records every address the security dialer hands to its inner
// dial (only ever validated IP literals) and connects to target instead.
type recordingDial struct {
	target string

	mu     sync.Mutex
	dialed []string
}

func (d *recordingDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.target)
}

func (d *recordingDial) addrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

// strictChecker builds a checker with the production policy whose dials all
// land on srv (when they pass validation) and are recorded.
func strictChecker(t *testing.T, srv *httptest.Server) (*Checker, *recordingDial) {
	t.Helper()
	rec := &recordingDial{target: srv.Listener.Addr().String()}
	c := New(security.Policy{}, WithDialerOptions(
		security.WithResolver(fakeResolver{
			"public.example":   {publicIP},
			"internal.example": {"10.0.0.5"},
			"mixed.example":    {publicIP, "127.0.0.1"},
		}),
		security.WithDialFunc(rec.dial),
	))
	t.Cleanup(c.CloseIdleConnections)
	return c, rec
}

func assertNoURLLeak(t *testing.T, out domain.CheckOutcome) {
	t.Helper()
	if strings.Contains(*out.ErrorMessage, "token") || strings.Contains(*out.ErrorMessage, "secret") ||
		strings.Contains(*out.ErrorMessage, "http://") {
		t.Fatalf("ErrorMessage %q contains the URL or its query", *out.ErrorMessage)
	}
}

// TestCheckerBlocksLoopbackTarget proves the production policy refuses
// loopback and other non-public targets without ever connecting — including a
// real listener on 127.0.0.1 with the default dialer.
func TestCheckerBlocksLoopbackTarget(t *testing.T) {
	t.Parallel()

	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Default dialer, real loopback server: blocked statically.
	out := mustCheck(t, New(security.Policy{}), testMonitor(srv.URL+"/?token=secret"))
	assertFailure(t, out, domain.ErrorKindBlocked)
	assertNoURLLeak(t, out)

	c, rec := strictChecker(t, srv)
	for _, target := range []string{
		"http://127.0.0.1/?token=secret",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://localhost/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.1.2.3/",
		"http://127.1/",
		"http://public.example:8080/", // non-default port
		"http://internal.example/?token=secret",
		"http://mixed.example/", // any blocked answer rejects the host
	} {
		out := mustCheck(t, c, testMonitor(target))
		assertFailure(t, out, domain.ErrorKindBlocked)
		assertNoURLLeak(t, out)
	}
	if got := rec.addrs(); len(got) != 0 {
		t.Fatalf("blocked targets were dialled: %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("server received %d requests, want 0", hits)
	}
}

// TestCheckRedirectToPrivateIsBlocked follows a redirect from an allowed
// public host to private targets: the check fails as blocked and the blocked
// hop is never dialled.
func TestCheckRedirectToPrivateIsBlocked(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loc := r.URL.Query().Get("to"); loc != "" {
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	for _, loc := range []string{
		"http://127.0.0.1/?token=secret",
		"http://169.254.169.254/latest/meta-data/",
		"http://internal.example/",
		// Only the static per-hop check (RedirectPolicy) catches this one:
		// the dialer would accept the public host.
		"http://user:secret@public.example/",
	} {
		t.Run(loc, func(t *testing.T) {
			t.Parallel()
			c, rec := strictChecker(t, srv)
			out := mustCheck(t, c, testMonitor("http://public.example/?to="+loc))
			assertFailure(t, out, domain.ErrorKindBlocked)
			assertNoURLLeak(t, out)
			for _, a := range rec.addrs() {
				if a != publicIP+":80" {
					t.Fatalf("dialled %s; only the public hop %s:80 may be dialled", a, publicIP)
				}
			}
		})
	}

	// Control: a redirect to another public hop succeeds under the same policy.
	c, _ := strictChecker(t, srv)
	out := mustCheck(t, c, testMonitor("http://public.example/?to=http://public.example/ok"))
	if !out.Success {
		t.Fatalf("public redirect: Success = false (kind %v, message %v)", *out.ErrorKind, *out.ErrorMessage)
	}
}

func TestClassifyBlockedFirst(t *testing.T) {
	t.Parallel()

	// A blocked error that also looks like a dial failure is still blocked.
	err := wrapURL(&net.OpError{Op: "dial", Net: "tcp", Err: errors.Join(security.ErrBlockedTarget, context.DeadlineExceeded)})
	if got := classify(err); got != domain.ErrorKindBlocked {
		t.Fatalf("classify = %s, want blocked", got)
	}
}
