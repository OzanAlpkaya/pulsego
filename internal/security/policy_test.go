package security

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestValidateURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		private bool
		ok      bool
	}{
		{"https public", "https://example.com/health", false, true},
		{"http public port 80", "http://example.com:80/", false, true},
		{"https public port 443", "https://example.com:443/", false, true},
		{"uppercase scheme", "HTTPS://example.com/", false, true},
		{"public IPv4 literal", "http://93.184.215.14/", false, true},
		{"public IPv6 literal", "http://[2606:4700::1111]/", false, true},
		{"query allowed", "https://example.com/?a=b", false, true},

		{"ftp scheme", "ftp://example.com/", false, false},
		{"file scheme", "file:///etc/passwd", false, false},
		{"gopher scheme", "gopher://example.com/", false, false},
		{"no scheme", "example.com", false, false},
		{"opaque", "http:example.com", false, false},
		{"empty", "", false, false},
		{"missing host", "http:///path", false, false},
		{"malformed", "http://exa mple.com/", false, false},
		{"bad percent escape", "http://example.com/%zz", false, false},
		{"userinfo", "http://user:pass@example.com/", false, false},
		{"user only", "http://user@example.com/", false, false},
		{"userinfo with AllowPrivate", "http://user:pass@example.com/", true, false},

		{"port 8080", "http://example.com:8080/", false, false},
		{"port 22", "http://example.com:22/", false, false},
		{"port 0", "http://example.com:0/", false, false},
		{"port too large", "http://example.com:65536/", false, false},
		{"port leading zero", "http://example.com:080/", false, false},
		{"port 8080 AllowPrivate", "http://example.com:8080/", true, true},
		{"port 0 AllowPrivate", "http://example.com:0/", true, false},
		{"port 65536 AllowPrivate", "http://example.com:65536/", true, false},

		{"zone id", "http://[fe80::1%25eth0]/", false, false},
		{"zone id AllowPrivate", "http://[fe80::1%25eth0]/", true, false},

		{"localhost", "http://localhost/", false, false},
		{"localhost trailing dot", "http://localhost./", false, false},
		{"localhost uppercase", "http://LOCALHOST/", false, false},
		{"sub.localhost", "http://api.localhost/", false, false},
		{"localhost AllowPrivate", "http://localhost:8080/", true, true},
		{"notlocalhost is fine", "http://notlocalhost.com/", false, true},

		{"short IPv4", "http://127.1/", false, false},
		{"decimal IPv4", "http://2130706433/", false, false},
		{"hex IPv4", "http://0x7f.0.0.1/", false, false},
		{"hex whole", "http://0x7f000001/", false, false},
		{"octal IPv4", "http://0177.0.0.1/", false, false},
		{"leading zero octet", "http://127.0.0.01/", false, false},
		{"numeric last label", "http://foo.123/", false, false},

		{"loopback", "http://127.0.0.1/", false, false},
		{"loopback trailing dot", "http://127.0.0.1./", false, false},
		{"loopback v6", "http://[::1]/", false, false},
		{"mapped loopback", "http://[::ffff:127.0.0.1]/", false, false},
		{"metadata", "http://169.254.169.254/latest/meta-data/", false, false},
		{"private 10", "http://10.0.0.5/", false, false},
		{"private 192.168", "https://192.168.1.1/", false, false},
		{"unspecified", "http://0.0.0.0/", false, false},
		{"ULA", "http://[fd00::1]/", false, false},
		{"loopback AllowPrivate", "http://127.0.0.1:8080/", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Policy{AllowPrivate: tc.private}.ValidateURL(tc.raw)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateURL(%q) = %v, want nil", tc.raw, err)
				}
				return
			}
			if !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("ValidateURL(%q) = %v, want ErrBlockedTarget", tc.raw, err)
			}
		})
	}
}

func TestBlockedReason(t *testing.T) {
	t.Parallel()

	blockedAddrs := []string{
		"0.0.0.0", "0.1.2.3", "::",
		"127.0.0.1", "127.255.255.254", "::1",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.0.1",
		"100.64.0.1", "100.127.255.255",
		"169.254.0.1", "169.254.169.254", "fe80::1", "febf::1",
		"224.0.0.1", "239.255.255.255", "ff02::1",
		"255.255.255.255", "240.0.0.1",
		"192.0.0.8",
		"198.18.0.1", "198.19.255.255",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1",
		"fc00::1", "fd12:3456::1", "fec0::1",
		"64:ff9b::7f00:1", "64:ff9b:1::1",
		"2002:7f00:1::1",
		"2001::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"fe80::1%eth0",
	}
	for _, s := range blockedAddrs {
		t.Run("blocked "+s, func(t *testing.T) {
			t.Parallel()
			reason, isBlocked := BlockedReason(netip.MustParseAddr(s))
			if !isBlocked || reason == "" {
				t.Fatalf("BlockedReason(%s) = %q, %v; want blocked", s, reason, isBlocked)
			}
		})
	}

	allowed := []string{
		"93.184.215.14", "1.1.1.1", "8.8.8.8",
		"172.15.255.255", "172.32.0.0", "100.63.255.255", "100.128.0.0",
		"198.17.255.255", "198.20.0.0", "192.0.1.1", "223.255.255.255",
		"2606:4700::1111", "2a00:1450:4001::1", "::ffff:93.184.215.14",
		"2001:4860:4860::8888",
	}
	for _, s := range allowed {
		t.Run("allowed "+s, func(t *testing.T) {
			t.Parallel()
			if reason, isBlocked := BlockedReason(netip.MustParseAddr(s)); isBlocked {
				t.Fatalf("BlockedReason(%s) = %q, true; want allowed", s, reason)
			}
		})
	}

	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		if _, isBlocked := BlockedReason(netip.Addr{}); !isBlocked {
			t.Fatal("zero Addr must be blocked")
		}
	})
}

func TestErrorsNeverLeakQueryOrUserinfo(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3tvalue"
	tests := []string{
		"http://user:" + secret + "@example.com/?token=" + secret,
		"http://127.0.0.1/?token=" + secret,
		"http://example.com:8080/" + secret + "?token=" + secret,
		"ftp://example.com/?token=" + secret,
		"http://exa mple.com/?token=" + secret,
		"http://localhost/#" + secret,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			err := Policy{}.ValidateURL(raw)
			if err == nil {
				t.Fatalf("ValidateURL(%q) = nil, want error", raw)
			}
			msg := err.Error()
			for _, bad := range []string{secret, "token", "user"} {
				if strings.Contains(msg, bad) {
					t.Fatalf("error %q leaks %q", msg, bad)
				}
			}
		})
	}
}
