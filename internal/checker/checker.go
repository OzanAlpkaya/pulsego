package checker

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/security"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// spanCheck is the client span around one check (Sprint 5 D4). It carries the
// target host only: the path and query of a monitor URL may hold secrets.
const spanCheck = "checker.check"

// Transport and request tuning. These are deliberately constants rather than
// configuration: they bound resource use per worker, not per monitor. Dial
// timeouts live in internal/security, whose Dialer the transport uses.
const (
	TLSHandshakeTimeout   = 10 * time.Second
	MaxIdleConns          = 100
	MaxIdleConnsPerHost   = 4
	IdleConnTimeout       = 90 * time.Second
	MaxRedirects          = 5
	MaxDrainBytes         = 64 << 10
	UserAgent             = "PulseGo/1.0"
	ForceAttemptHTTP2     = true
	expectedRangeTemplate = "status %d outside expected range %d-%d"
)

// Option configures a Checker.
type Option func(*options)

type options struct {
	dialerOpts     []security.DialerOption
	tracerProvider trace.TracerProvider
}

// WithTracerProvider records checker.check spans on tp instead of the global
// provider. It exists for tests.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProvider = tp }
}

// WithDialerOptions passes options (a test resolver or inner dial) to the
// SSRF-validating dialer. They cannot remove the validation itself.
func WithDialerOptions(opts ...security.DialerOption) Option {
	return func(o *options) { o.dialerOpts = append(o.dialerOpts, opts...) }
}

// Checker runs HTTP checks. It is safe for concurrent use; create one per
// process so all checks share its connection pool.
type Checker struct {
	policy    security.Policy
	transport *http.Transport
	client    *http.Client
	// tracerProvider is nil for the global provider, looked up per span.
	tracerProvider trace.TracerProvider
}

// New builds a Checker with the shared transport. policy is required so an
// unprotected checker cannot be built by accident: every connection goes
// through security.Dialer, and every redirect hop through
// security.RedirectPolicy.
func New(policy security.Policy, opts ...Option) *Checker {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	dialer := security.NewDialer(policy, o.dialerOpts...)

	transport := &http.Transport{
		Proxy:               nil, // ignore HTTP(S)_PROXY from the environment
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: TLSHandshakeTimeout,
		MaxIdleConns:        MaxIdleConns,
		MaxIdleConnsPerHost: MaxIdleConnsPerHost,
		IdleConnTimeout:     IdleConnTimeout,
		ForceAttemptHTTP2:   ForceAttemptHTTP2,
	}

	return &Checker{
		policy:         policy,
		tracerProvider: o.tracerProvider,
		transport:      transport,
		client: &http.Client{
			Transport: transport,
			// No client-wide timeout: the per-check context deadline is the
			// only time limit.
			Timeout:       0,
			CheckRedirect: security.RedirectPolicy(policy, MaxRedirects),
		},
	}
}

// CloseIdleConnections closes pooled connections that are not in use.
func (c *Checker) CloseIdleConnections() { c.transport.CloseIdleConnections() }

// Check performs one GET against m.URL under m.Timeout().
//
// The returned error is non-nil only when ctx itself was cancelled or expired
// (for example on shutdown); that is not a check result and must not be
// recorded. Every other failure is reported in the outcome.
func (c *Checker) Check(ctx context.Context, m domain.Monitor) (domain.CheckOutcome, error) {
	if err := ctx.Err(); err != nil {
		return domain.CheckOutcome{}, err
	}

	ctx, span := telemetry.TracerFrom(c.tracerProvider).Start(ctx, spanCheck,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", http.MethodGet)))
	defer span.End()
	if host := urlHost(m.URL); host != "" {
		span.SetAttributes(attribute.String("server.address", host))
	}

	out, err := c.check(ctx, m)
	switch {
	case err != nil:
		span.SetStatus(codes.Error, "check cancelled")
	default:
		if out.StatusCode != nil {
			span.SetAttributes(attribute.Int("http.response.status_code", int(*out.StatusCode)))
		}
		if !out.Success && out.ErrorKind != nil {
			// The kind only: the error message may quote the target.
			span.SetAttributes(attribute.String("pulsego.error_kind", out.ErrorKind.String()))
			span.SetStatus(codes.Error, out.ErrorKind.String())
		}
	}
	return out, err
}

// urlHost returns the host name of raw without port, path, query or userinfo,
// or "" if raw does not parse.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (c *Checker) check(ctx context.Context, m domain.Monitor) (domain.CheckOutcome, error) {
	checkCtx, cancel := context.WithTimeout(ctx, m.Timeout())
	defer cancel()

	start := time.Now()
	// Static validation first, so a blocked URL is never dialled. The dialer
	// still checks every resolved address.
	if err := c.policy.ValidateURL(m.URL); err != nil {
		return failure(classify(err), errorMessage(err), time.Since(start)), nil
	}
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, m.URL, nil)
	if err != nil {
		return failure(domain.ErrorKindUnknown, errorMessage(err), time.Since(start)), nil
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		if perr := ctx.Err(); perr != nil {
			return domain.CheckOutcome{}, perr
		}
		return failure(classify(err), errorMessage(err), latency), nil
	}

	// Drain a bounded amount so the connection can be reused, then discard.
	// The body is never retained. A drain error does not change the result:
	// the status line has already arrived.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxDrainBytes))
	_ = resp.Body.Close()

	status := int32(resp.StatusCode) //nolint:gosec // G115: HTTP status codes are three digits.
	if resp.StatusCode < int(m.ExpectedStatusMin) || resp.StatusCode > int(m.ExpectedStatusMax) {
		out := failure(domain.ErrorKindUnexpectedStatus,
			fmt.Sprintf(expectedRangeTemplate, resp.StatusCode, m.ExpectedStatusMin, m.ExpectedStatusMax),
			latency)
		out.StatusCode = &status
		return out, nil
	}

	return domain.CheckOutcome{
		Success:    true,
		StatusCode: &status,
		LatencyMS:  latencyMS(latency),
	}, nil
}

func failure(kind domain.ErrorKind, msg string, latency time.Duration) domain.CheckOutcome {
	return domain.CheckOutcome{
		Success:      false,
		LatencyMS:    latencyMS(latency),
		ErrorKind:    &kind,
		ErrorMessage: &msg,
	}
}

func latencyMS(d time.Duration) int32 {
	ms := d.Milliseconds()
	switch {
	case ms < 0:
		return 0
	case ms > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(ms)
	}
}
