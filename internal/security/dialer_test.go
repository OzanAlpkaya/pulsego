package security

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
)

// fakeResolver answers from a fixed table, or with successive answers per host
// to simulate DNS rebinding. It counts lookups.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	calls   map[string]int
}

func newFakeResolver(table map[string][]string) *fakeResolver {
	r := &fakeResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}
	for host, ips := range table {
		r.answers[host] = [][]netip.Addr{parseAddrs(ips)}
	}
	return r
}

func parseAddrs(ips []string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ips))
	for _, s := range ips {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq, ok := r.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	n := r.calls[host]
	r.calls[host]++
	if n >= len(seq) {
		n = len(seq) - 1
	}
	return seq[n], nil
}

func (r *fakeResolver) lookups(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

// recordingDial records every address it is asked to dial and returns one end
// of a pipe, or connects to target when it is set.
type recordingDial struct {
	mu     sync.Mutex
	addrs  []string
	target string
}

func (d *recordingDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	if d.target != "" {
		var nd net.Dialer
		return nd.DialContext(ctx, network, d.target)
	}
	c1, c2 := net.Pipe()
	go func() { _ = c2.Close() }()
	return c1, nil
}

func (d *recordingDial) dialled() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

func TestDialerRejectsPrivateAnswer(t *testing.T) {
	t.Parallel()

	tests := []struct{ host, ip string }{
		{"loop.example", "127.0.0.1"},
		{"private.example", "10.0.0.5"},
		{"meta.example", "169.254.169.254"},
		{"mapped.example", "::ffff:127.0.0.1"},
		{"ula.example", "fd00::1"},
		{"localhost", "127.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			res := newFakeResolver(map[string][]string{tc.host: {tc.ip}})
			rec := &recordingDial{}
			d := NewDialer(Policy{}, WithResolver(res), WithDialFunc(rec.dial))
			conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort(tc.host, "443"))
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("DialContext = %v, want ErrBlockedTarget", err)
			}
			if got := rec.dialled(); len(got) != 0 {
				t.Fatalf("dialled %v, want nothing", got)
			}
		})
	}
}

func TestDialerRejectsBlockedLiteral(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:80", "[::1]:443", "169.254.169.254:80", "[::ffff:10.0.0.1]:80"} {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			res := newFakeResolver(nil)
			rec := &recordingDial{}
			d := NewDialer(Policy{}, WithResolver(res), WithDialFunc(rec.dial))
			if _, err := d.DialContext(context.Background(), "tcp", addr); !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("DialContext(%s) = %v, want ErrBlockedTarget", addr, err)
			}
			if got := rec.dialled(); len(got) != 0 {
				t.Fatalf("dialled %v, want nothing", got)
			}
		})
	}
}

func TestDialerRejectsMixedAnswers(t *testing.T) {
	t.Parallel()

	for _, order := range [][]string{
		{"93.184.215.14", "10.0.0.5"},
		{"10.0.0.5", "93.184.215.14"},
		{"2606:4700::1111", "::1"},
	} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			t.Parallel()
			res := newFakeResolver(map[string][]string{"mixed.example": order})
			rec := &recordingDial{}
			d := NewDialer(Policy{}, WithResolver(res), WithDialFunc(rec.dial))
			if _, err := d.DialContext(context.Background(), "tcp", "mixed.example:443"); !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("DialContext = %v, want ErrBlockedTarget", err)
			}
			if got := rec.dialled(); len(got) != 0 {
				t.Fatalf("dialled %v, want nothing", got)
			}
		})
	}
}

func TestDialerDialsValidatedIP(t *testing.T) {
	t.Parallel()

	// Rebinding resolver: public on the first lookup, loopback afterwards.
	res := newFakeResolver(nil)
	res.answers["rebind.example"] = [][]netip.Addr{
		parseAddrs([]string{"93.184.215.14"}),
		parseAddrs([]string{"127.0.0.1"}),
	}
	rec := &recordingDial{}
	d := NewDialer(Policy{}, WithResolver(res), WithDialFunc(rec.dial))

	conn, err := d.DialContext(context.Background(), "tcp", "rebind.example:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()

	if n := res.lookups("rebind.example"); n != 1 {
		t.Fatalf("lookups = %d, want 1", n)
	}
	if got := rec.dialled(); len(got) != 1 || got[0] != "93.184.215.14:443" {
		t.Fatalf("dialled %v, want [93.184.215.14:443]", got)
	}
}

func TestDialerTriesAnswersInOrder(t *testing.T) {
	t.Parallel()

	res := newFakeResolver(map[string][]string{"multi.example": {"93.184.215.14", "1.1.1.1"}})
	var dialled []string
	d := NewDialer(Policy{}, WithResolver(res), WithDialFunc(func(_ context.Context, _, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		if len(dialled) == 1 {
			return nil, errors.New("refused")
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}))
	conn, err := d.DialContext(context.Background(), "tcp", "multi.example:80")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
	if want := []string{"93.184.215.14:80", "1.1.1.1:80"}; strings.Join(dialled, " ") != strings.Join(want, " ") {
		t.Fatalf("dialled %v, want %v", dialled, want)
	}
}

func TestDialerNoAnswersIsDNSError(t *testing.T) {
	t.Parallel()

	res := newFakeResolver(map[string][]string{"empty.example": {}})
	d := NewDialer(Policy{}, WithResolver(res), WithDialFunc((&recordingDial{}).dial))
	_, err := d.DialContext(context.Background(), "tcp", "empty.example:443")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("DialContext = %v, want *net.DNSError", err)
	}
	_, err = d.DialContext(context.Background(), "tcp", "missing.example:443")
	if !errors.As(err, &dnsErr) {
		t.Fatalf("DialContext = %v, want *net.DNSError", err)
	}
}

func TestDialerPorts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		port    string
		private bool
		ok      bool
	}{
		{"80", false, true},
		{"443", false, true},
		{"8080", false, false},
		{"22", false, false},
		{"0", false, false},
		{"65536", false, false},
		{"", false, false},
		{"http", false, false},
		{"8080", true, true},
		{"1", true, true},
		{"65535", true, true},
		{"0", true, false},
		{"65536", true, false},
		{"-1", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.port, func(t *testing.T) {
			t.Parallel()
			res := newFakeResolver(map[string][]string{"public.example": {"93.184.215.14"}})
			rec := &recordingDial{}
			d := NewDialer(Policy{AllowPrivate: tc.private}, WithResolver(res), WithDialFunc(rec.dial))
			conn, err := d.DialContext(context.Background(), "tcp", "public.example:"+tc.port)
			if conn != nil {
				_ = conn.Close()
			}
			if tc.ok {
				if err != nil {
					t.Fatalf("port %q: %v", tc.port, err)
				}
				return
			}
			if !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("port %q: err = %v, want ErrBlockedTarget", tc.port, err)
			}
			if res.lookups("public.example") != 0 || len(rec.dialled()) != 0 {
				t.Fatal("rejected port must not resolve or dial")
			}
		})
	}
}

func TestDialerRejectsNonTCPAndZones(t *testing.T) {
	t.Parallel()

	d := NewDialer(Policy{AllowPrivate: true}, WithResolver(newFakeResolver(nil)), WithDialFunc((&recordingDial{}).dial))
	if _, err := d.DialContext(context.Background(), "udp", "93.184.215.14:443"); !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("udp: %v, want ErrBlockedTarget", err)
	}
	if _, err := d.DialContext(context.Background(), "tcp", "[fe80::1%eth0]:443"); !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("zone: %v, want ErrBlockedTarget", err)
	}
}

func TestDialerAllowPrivate(t *testing.T) {
	t.Parallel()

	res := newFakeResolver(map[string][]string{"internal.example": {"10.0.0.5"}})
	rec := &recordingDial{}
	d := NewDialer(Policy{AllowPrivate: true}, WithResolver(res), WithDialFunc(rec.dial))
	conn, err := d.DialContext(context.Background(), "tcp", "internal.example:8080")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
	if got := rec.dialled(); len(got) != 1 || got[0] != "10.0.0.5:8080" {
		t.Fatalf("dialled %v, want the resolved IP literal [10.0.0.5:8080]", got)
	}
	if n := res.lookups("internal.example"); n != 1 {
		t.Fatalf("lookups = %d, want 1", n)
	}
}

func TestDefaultDialAllowPrivateReachesLoopback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	d := NewDialer(Policy{AllowPrivate: true})
	conn, err := d.DialContext(context.Background(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
}

func TestControlCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		ok   bool
	}{
		{"93.184.215.14:443", true},
		{"[2606:4700::1111]:80", true},
		{"127.0.0.1:80", false},
		{"[::1]:443", false},
		{"[::ffff:127.0.0.1]:80", false},
		{"10.0.0.5:80", false},
		{"[fe80::1%eth0]:80", false},
		{"garbage", false},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			err := controlCheck("tcp4", tc.addr, nil)
			if tc.ok != (err == nil) {
				t.Fatalf("controlCheck(%s) = %v, want ok=%v", tc.addr, err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("controlCheck(%s) = %v, want ErrBlockedTarget", tc.addr, err)
			}
		})
	}
}
