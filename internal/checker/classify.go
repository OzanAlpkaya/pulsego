package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"syscall"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// classify maps a transport error to an error kind. The caller has already
// ruled out cancellation of its own context. The order of the checks is part
// of the contract: a DNS lookup cut off by the deadline is a timeout, and a
// TLS failure wrapped in a *net.OpError is tls, not connection.
func classify(err error) domain.ErrorKind {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return domain.ErrorKindTimeout
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return domain.ErrorKindDNS
	}

	if errors.Is(err, ErrTooManyRedirects) {
		return domain.ErrorKindRedirect
	}

	if isTLSError(err) {
		return domain.ErrorKindTLS
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return domain.ErrorKindConnection
	}

	return domain.ErrorKindUnknown
}

func isTLSError(err error) bool {
	var (
		certVerify   *tls.CertificateVerificationError
		recordHeader tls.RecordHeaderError
		alert        tls.AlertError
		unknownAuth  x509.UnknownAuthorityError
		hostname     x509.HostnameError
		invalid      x509.CertificateInvalidError
		systemRoots  x509.SystemRootsError
		constraint   x509.ConstraintViolationError
		critical     x509.UnhandledCriticalExtension
		insecureAlg  x509.InsecureAlgorithmError
	)
	return errors.As(err, &certVerify) ||
		errors.As(err, &recordHeader) ||
		errors.As(err, &alert) ||
		errors.As(err, &unknownAuth) ||
		errors.As(err, &hostname) ||
		errors.As(err, &invalid) ||
		errors.As(err, &systemRoots) ||
		errors.As(err, &constraint) ||
		errors.As(err, &critical) ||
		errors.As(err, &insecureAlg)
}

// errorMessage renders err for storage. The *url.Error wrapper is removed
// first so the target URL, whose query string may carry a secret, is never
// stored. The result is valid UTF-8 of at most domain.MaxErrorMessageLength
// bytes.
func errorMessage(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	return domain.TruncateUTF8(err.Error(), domain.MaxErrorMessageLength)
}
