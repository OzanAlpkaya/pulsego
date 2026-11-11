package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// wrapURL wraps err the way http.Client.Do does.
func wrapURL(err error) error {
	return &url.Error{Op: "Get", URL: "https://example.com/?token=secret", Err: err}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	t.Parallel()

	dial := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: err}}
	}

	tests := []struct {
		name string
		err  error
		want domain.ErrorKind
	}{
		{"deadline exceeded", wrapURL(context.DeadlineExceeded), domain.ErrorKindTimeout},
		{"wrapped deadline", wrapURL(fmt.Errorf("read: %w", context.DeadlineExceeded)), domain.ErrorKindTimeout},
		{"net.Error timeout", wrapURL(&net.OpError{Op: "read", Err: timeoutErr{}}), domain.ErrorKindTimeout},
		{"dns timeout", wrapURL(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true}}), domain.ErrorKindTimeout},
		{"dns timeout via deadline", wrapURL(&net.DNSError{Err: context.DeadlineExceeded.Error(), Name: "example.com", UnwrapErr: context.DeadlineExceeded}), domain.ErrorKindTimeout},
		{"dns not found", wrapURL(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}}), domain.ErrorKindDNS},
		{"dns bare", &net.DNSError{Err: "server misbehaving", Name: "example.com"}, domain.ErrorKindDNS},
		{"redirect", wrapURL(ErrTooManyRedirects), domain.ErrorKindRedirect},
		{"cert verification", wrapURL(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), domain.ErrorKindTLS},
		{"x509 hostname", wrapURL(x509.HostnameError{Host: "example.com", Certificate: &x509.Certificate{}}), domain.ErrorKindTLS},
		{"x509 invalid", wrapURL(x509.CertificateInvalidError{Reason: x509.Expired, Cert: &x509.Certificate{}}), domain.ErrorKindTLS},
		{"tls record header in OpError", wrapURL(&net.OpError{Op: "remote error", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}), domain.ErrorKindTLS},
		{"tls alert", wrapURL(&net.OpError{Op: "remote error", Err: tls.AlertError(40)}), domain.ErrorKindTLS},
		{"op error", wrapURL(&net.OpError{Op: "dial", Err: errors.New("something")}), domain.ErrorKindConnection},
		{"refused", wrapURL(dial(syscall.ECONNREFUSED)), domain.ErrorKindConnection},
		{"bare refused", fmt.Errorf("x: %w", syscall.ECONNREFUSED), domain.ErrorKindConnection},
		{"reset", wrapURL(syscall.ECONNRESET), domain.ErrorKindConnection},
		{"host unreachable", wrapURL(syscall.EHOSTUNREACH), domain.ErrorKindConnection},
		{"net unreachable", wrapURL(syscall.ENETUNREACH), domain.ErrorKindConnection},
		{"eof", wrapURL(io.EOF), domain.ErrorKindConnection},
		{"unexpected eof", wrapURL(io.ErrUnexpectedEOF), domain.ErrorKindConnection},
		{"unknown", wrapURL(errors.New("unsupported protocol scheme")), domain.ErrorKindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classify(tt.err); got != tt.want {
				t.Fatalf("classify(%v) = %s, want %s", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorMessageStripsURL(t *testing.T) {
	t.Parallel()

	msg := errorMessage(wrapURL(syscall.ECONNREFUSED))
	if strings.Contains(msg, "secret") || strings.Contains(msg, "example.com") {
		t.Fatalf("errorMessage = %q, contains the URL", msg)
	}
	if msg != syscall.ECONNREFUSED.Error() {
		t.Fatalf("errorMessage = %q, want %q", msg, syscall.ECONNREFUSED.Error())
	}

	// Not a *url.Error: the message passes through.
	if got := errorMessage(errors.New("plain")); got != "plain" {
		t.Fatalf("errorMessage = %q, want %q", got, "plain")
	}
}

func TestErrorMessageTruncation(t *testing.T) {
	t.Parallel()

	const limit = domain.MaxErrorMessageLength

	tests := []struct {
		name    string
		in      string
		wantLen int
	}{
		{"short", "hello", 5},
		{"exact", strings.Repeat("a", limit), limit},
		{"ascii over", strings.Repeat("a", limit+100), limit},
		// "a" + 3-byte runes: the rune spanning byte 512 must be dropped.
		{"rune boundary", "a" + strings.Repeat("€", 200), 1 + 3*170},
		// 4-byte runes, 512 is a boundary.
		{"four byte exact", strings.Repeat("😀", 200), limit},
		{"four byte offset", "ab" + strings.Repeat("😀", 200), 2 + 4*127},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := errorMessage(wrapURL(errors.New(tt.in)))
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tt.wantLen)
			}
			if len(got) > limit {
				t.Fatalf("len = %d, exceeds %d", len(got), limit)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8")
			}
			if !strings.HasPrefix(tt.in, got) {
				t.Fatalf("result is not a prefix of the input")
			}
		})
	}

	if got := domain.TruncateUTF8("ok\xffbad", limit); !utf8.ValidString(got) {
		t.Fatalf("TruncateUTF8(invalid UTF-8) = %q, not valid UTF-8", got)
	}
}
