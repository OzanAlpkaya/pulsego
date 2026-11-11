package security

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ErrBlockedTarget is wrapped by every rejection this package produces, so
// callers can classify it with errors.Is. Messages built around it carry the
// reason and the host only — never the full URL, its query, or userinfo.
var ErrBlockedTarget = errors.New("blocked target")

// Policy decides which outbound targets are allowed.
//
// AllowPrivate (from ALLOW_PRIVATE_TARGETS) disables the address-range,
// localhost and ambiguous-numeric-host checks and allows any port 1–65535. It
// is meant for local development only.
type Policy struct {
	AllowPrivate bool
}

// Default ports allowed when AllowPrivate is false.
const (
	portHTTP  = 80
	portHTTPS = 443
)

func blocked(host, reason string) error {
	if host == "" {
		return fmt.Errorf("%w: %s", ErrBlockedTarget, reason)
	}
	return fmt.Errorf("%w: %s (host %q)", ErrBlockedTarget, reason, host)
}

// ValidateURL checks raw statically, without DNS. Passing it does not make a
// target safe: host names are only checked once resolved, by Dialer.
func (p Policy) ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// The parse error embeds the input, which may hold secrets.
		return blocked("", "malformed URL")
	}
	return p.validate(u)
}

func (p Policy) validate(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return blocked("", "scheme must be http or https")
	}
	if u.Opaque != "" {
		return blocked("", "malformed URL")
	}
	host := u.Hostname()
	if host == "" {
		return blocked("", "missing host")
	}
	if u.User != nil {
		return blocked(host, "credentials in URL")
	}
	if strings.Contains(host, "%") {
		return blocked(host, "IPv6 zone identifier")
	}
	if err := p.checkPort(host, u.Port(), true); err != nil {
		return err
	}
	return p.checkHost(host)
}

// checkPort validates a port string. When optional is true an empty port is
// accepted (the scheme default applies).
func (p Policy) checkPort(host, port string, optional bool) error {
	if port == "" {
		if optional {
			return nil
		}
		return blocked(host, "missing port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return blocked(host, "invalid port")
	}
	if !p.AllowPrivate && n != portHTTP && n != portHTTPS {
		return blocked(host, "port not allowed")
	}
	return nil
}

func (p Policy) checkHost(host string) error {
	if p.AllowPrivate {
		return nil
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if addr, err := netip.ParseAddr(name); err == nil {
		if reason, isBlocked := BlockedReason(addr); isBlocked {
			return blocked(host, reason)
		}
		return nil
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return blocked(host, "localhost")
	}
	if isAmbiguousNumeric(name) {
		return blocked(host, "ambiguous numeric host")
	}
	return nil
}

// isAmbiguousNumeric reports whether name would be read as an IPv4 address by
// some parser even though it is not a canonical dotted quad (127.1,
// 2130706433, 0x7f.0.0.1, 0177.0.0.1, 127.0.0.01). Per the WHATWG URL rules a
// host whose last label is numeric is an IPv4 address; no real TLD is numeric.
func isAmbiguousNumeric(name string) bool {
	last := name
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		last = name[i+1:]
	}
	if last == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") {
		return isHex(last[2:])
	}
	return isDigits(last)
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true // "0x" alone is also read as 0 by inet_aton-style parsers
}

type blockedRange struct {
	prefix netip.Prefix
	reason string
}

var blockedRanges = func() []blockedRange {
	ranges := []struct{ cidr, reason string }{
		{"0.0.0.0/8", "unspecified address"},
		{"::/128", "unspecified address"},
		{"127.0.0.0/8", "loopback address"},
		{"::1/128", "loopback address"},
		// Must stay after ::/128 and ::1/128: the first match wins.
		{"::/96", "IPv4-compatible IPv6 address (deprecated)"},
		{"100::/64", "discard-only address"},
		{"2001:10::/28", "ORCHID address (deprecated)"},
		{"10.0.0.0/8", "private address"},
		{"172.16.0.0/12", "private address"},
		{"192.168.0.0/16", "private address"},
		{"100.64.0.0/10", "shared address space"},
		{"169.254.0.0/16", "link-local address"},
		{"fe80::/10", "link-local address"},
		{"224.0.0.0/4", "multicast address"},
		{"ff00::/8", "multicast address"},
		{"255.255.255.255/32", "broadcast address"},
		{"240.0.0.0/4", "reserved address"},
		{"192.0.0.0/24", "IETF protocol assignment"},
		{"198.18.0.0/15", "benchmarking address"},
		{"192.0.2.0/24", "documentation address"},
		{"198.51.100.0/24", "documentation address"},
		{"203.0.113.0/24", "documentation address"},
		{"2001:db8::/32", "documentation address"},
		{"fc00::/7", "unique local address"},
		{"fec0::/10", "site-local address"},
		{"64:ff9b::/96", "NAT64 address"},
		{"64:ff9b:1::/48", "NAT64 address"},
		{"2002::/16", "6to4 address"},
		{"2001::/32", "Teredo address"},
	}
	out := make([]blockedRange, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, blockedRange{netip.MustParsePrefix(r.cidr), r.reason})
	}
	return out
}()

// BlockedReason reports whether addr is in a range outbound checks must never
// reach, and why. IPv4-mapped IPv6 addresses are unmapped and zones dropped
// before matching; an invalid address is blocked. Ranges are matched in table
// order and the first match supplies the reason.
func BlockedReason(addr netip.Addr) (reason string, isBlocked bool) {
	if !addr.IsValid() {
		return "invalid address", true
	}
	addr = addr.WithZone("").Unmap()
	for _, r := range blockedRanges {
		if r.prefix.Contains(addr) {
			return r.reason, true
		}
	}
	return "", false
}
