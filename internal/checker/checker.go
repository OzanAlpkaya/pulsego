package checker

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// Transport and request tuning. These are deliberately constants rather than
// configuration: they bound resource use per worker, not per monitor.
const (
	DialTimeout           = 10 * time.Second
	DialKeepAlive         = 30 * time.Second
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

// ErrTooManyRedirects is returned by the redirect policy once a check has
// followed MaxRedirects redirects. It is classified as redirect.
var ErrTooManyRedirects = fmt.Errorf("stopped after %d redirects", MaxRedirects)

// DialContextFunc dials a network connection. It is the hook through which a
// validating (SSRF-safe) dialer replaces the default one.
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Option configures a Checker.
type Option func(*options)

type options struct {
	dial DialContextFunc
}

// WithDialContext replaces the transport's dialer. The function must honour
// ctx; DialTimeout is not applied to a custom dialer.
func WithDialContext(dial DialContextFunc) Option {
	return func(o *options) { o.dial = dial }
}

// Checker runs HTTP checks. It is safe for concurrent use; create one per
// process so all checks share its connection pool.
type Checker struct {
	transport *http.Transport
	client    *http.Client
}

// New builds a Checker with the shared transport.
func New(opts ...Option) *Checker {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.dial == nil {
		dialer := &net.Dialer{Timeout: DialTimeout, KeepAlive: DialKeepAlive}
		o.dial = dialer.DialContext
	}

	transport := &http.Transport{
		Proxy:               nil, // ignore HTTP(S)_PROXY from the environment
		DialContext:         o.dial,
		TLSHandshakeTimeout: TLSHandshakeTimeout,
		MaxIdleConns:        MaxIdleConns,
		MaxIdleConnsPerHost: MaxIdleConnsPerHost,
		IdleConnTimeout:     IdleConnTimeout,
		ForceAttemptHTTP2:   ForceAttemptHTTP2,
	}

	return &Checker{
		transport: transport,
		client: &http.Client{
			Transport: transport,
			// No client-wide timeout: the per-check context deadline is the
			// only time limit.
			Timeout:       0,
			CheckRedirect: checkRedirect,
		},
	}
}

// CloseIdleConnections closes pooled connections that are not in use.
func (c *Checker) CloseIdleConnections() { c.transport.CloseIdleConnections() }

func checkRedirect(_ *http.Request, via []*http.Request) error {
	// via holds every request already sent, so the request about to be sent
	// is redirect number len(via).
	if len(via) > MaxRedirects {
		return ErrTooManyRedirects
	}
	return nil
}

// Check performs one GET against m.URL under m.Timeout().
//
// The returned error is non-nil only when ctx itself was cancelled or expired
// (for example on shutdown); that is not a check result and must not be
// recorded. Every other failure is reported in the outcome.
func (c *Checker) Check(ctx context.Context, m domain.Monitor) (domain.CheckOutcome, error) {
	if err := ctx.Err(); err != nil {
		return domain.CheckOutcome{}, err
	}

	checkCtx, cancel := context.WithTimeout(ctx, m.Timeout())
	defer cancel()

	start := time.Now()
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
