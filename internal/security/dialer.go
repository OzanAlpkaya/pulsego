package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// Default dial tuning; they match the checker's transport constants.
const (
	DialTimeout   = 10 * time.Second
	DialKeepAlive = 30 * time.Second
)

// Resolver resolves host names. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc dials a single, already validated address.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// DialerOption configures a Dialer.
type DialerOption func(*Dialer)

// WithResolver replaces net.DefaultResolver.
func WithResolver(r Resolver) DialerOption {
	return func(d *Dialer) { d.resolver = r }
}

// WithDialFunc replaces the inner dial, which then receives only validated
// IP:port literals. The default dial's socket-level Control re-check is not
// applied to a replacement; it exists for tests.
func WithDialFunc(f DialFunc) DialerOption {
	return func(d *Dialer) { d.dial = f }
}

// Dialer is an SSRF-safe DialContext: it resolves the host once, rejects it if
// any answer is blocked, and connects only to the validated IP literals, so a
// second (rebinding) resolution can never happen.
type Dialer struct {
	policy   Policy
	resolver Resolver
	dial     DialFunc
}

// NewDialer builds a Dialer for policy.
func NewDialer(policy Policy, opts ...DialerOption) *Dialer {
	d := &Dialer{policy: policy, resolver: net.DefaultResolver}
	for _, opt := range opts {
		opt(d)
	}
	if d.dial == nil {
		nd := &net.Dialer{Timeout: DialTimeout, KeepAlive: DialKeepAlive}
		if !policy.AllowPrivate {
			nd.Control = controlCheck
		}
		d.dial = nd.DialContext
	}
	return d
}

// DialContext dials addr ("host:port") after validating it against the policy.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var lookupNet string
	switch network {
	case "tcp":
		lookupNet = "ip"
	case "tcp4":
		lookupNet = "ip4"
	case "tcp6":
		lookupNet = "ip6"
	default:
		return nil, blocked("", "network not allowed")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, blocked("", "malformed address")
	}
	if host == "" {
		return nil, blocked("", "missing host")
	}
	if strings.Contains(host, "%") {
		return nil, blocked(host, "IPv6 zone identifier")
	}
	if err := d.policy.checkPort(host, port, false); err != nil {
		return nil, err
	}

	addrs, err := d.resolve(ctx, lookupNet, host)
	if err != nil {
		return nil, err
	}
	if !d.policy.AllowPrivate {
		for _, ip := range addrs {
			if reason, isBlocked := BlockedReason(ip); isBlocked {
				return nil, blocked(host, reason)
			}
		}
	}

	var errs []error
	for _, ip := range addrs {
		conn, err := d.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

func (d *Dialer) resolve(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}
	answers, err := d.resolver.LookupNetIP(ctx, network, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	out := make([]netip.Addr, 0, len(answers))
	for _, ip := range answers {
		out = append(out, ip.WithZone("").Unmap())
	}
	return out, nil
}

// controlCheck re-checks the socket address right before connect, as defense
// in depth against anything that slipped past the resolve-time check.
func controlCheck(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return blocked("", "unparseable socket address")
	}
	if reason, isBlocked := BlockedReason(ap.Addr()); isBlocked {
		return blocked(ap.Addr().String(), reason)
	}
	return nil
}
