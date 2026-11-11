package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// Metric label values used when an input falls outside its bounded set.
// Every label passes through a normaliser before it reaches Prometheus, so
// no caller can create an unbounded number of series by accident.
const (
	labelOther     = "OTHER"
	labelUnmatched = "unmatched"
	labelUnknown   = "unknown"
	labelNone      = "none"
	labelSuccess   = "success"
	labelFailure   = "failure"
	labelOK        = "ok"
	labelError     = "error"
	opOther        = "other"
)

// maxRouteLength bounds the route label. chi patterns in this service are far
// shorter; anything longer is not a pattern we registered.
const maxRouteLength = 128

// Histogram buckets, in seconds, fixed by the Sprint 5 plan (D3).
var (
	checkDurationBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	schedulerLagBuckets  = []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}
)

// Metrics owns the ten PulseGo Prometheus metrics on a private registry, so
// nothing registered by a dependency on the global registry leaks into
// /metrics. Every recording method normalises its labels.
//
// A nil *Metrics is valid: every method becomes a no-op, which lets tests and
// optional wiring skip metrics without nil checks at each call site.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests       *prometheus.CounterVec
	httpDuration       *prometheus.HistogramVec
	checks             *prometheus.CounterVec
	checkDuration      *prometheus.HistogramVec
	stateChanges       *prometheus.CounterVec
	claimedMonitors    prometheus.Counter
	activeChecks       prometheus.Gauge
	claimErrors        prometheus.Counter
	schedulerLag       prometheus.Histogram
	dbOperationLatency *prometheus.HistogramVec
}

// NewMetrics creates and registers all ten metrics plus the Go runtime and
// process collectors. Label combinations drawn from fixed domain sets are
// pre-initialised, so dashboards see zero-valued series before the first
// event instead of missing ones.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulsego_http_requests_total",
			Help: "HTTP requests handled by the public API, by method, route pattern and status code.",
		}, []string{"method", "route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pulsego_http_request_duration_seconds",
			Help:    "Duration of HTTP requests handled by the public API.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulsego_monitor_checks_total",
			Help: "Monitor checks performed, by result and error kind.",
		}, []string{"result", "error_kind"}),
		checkDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pulsego_monitor_check_duration_seconds",
			Help:    "Duration of monitor checks, by result.",
			Buckets: checkDurationBuckets,
		}, []string{"result"}),
		stateChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulsego_monitor_state_total",
			Help: "Monitor state changes caused by recorded check results.",
		}, []string{"from", "to"}),
		claimedMonitors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pulsego_worker_claimed_monitors_total",
			Help: "Monitors claimed by the worker for checking.",
		}),
		activeChecks: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pulsego_worker_active_checks",
			Help: "Checks currently in flight in this worker.",
		}),
		claimErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "pulsego_worker_claim_errors_total",
			Help: "Failed attempts to claim due monitors.",
		}),
		schedulerLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "pulsego_scheduler_lag_seconds",
			Help:    "Delay between a monitor's next_check_at and the moment it was claimed.",
			Buckets: schedulerLagBuckets,
		}),
		dbOperationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pulsego_db_operation_duration_seconds",
			Help:    "Duration of database operations, by sqlc query name and outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation", "outcome"}),
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests,
		m.httpDuration,
		m.checks,
		m.checkDuration,
		m.stateChanges,
		m.claimedMonitors,
		m.activeChecks,
		m.claimErrors,
		m.schedulerLag,
		m.dbOperationLatency,
	)

	m.preinitialise()
	return m
}

// preinitialise creates the zero-valued series whose label sets are fully
// known from the domain.
func (m *Metrics) preinitialise() {
	m.checks.WithLabelValues(labelSuccess, labelNone)
	for _, kind := range domain.ErrorKinds() {
		m.checks.WithLabelValues(labelFailure, string(kind))
	}
	m.checkDuration.WithLabelValues(labelSuccess)
	m.checkDuration.WithLabelValues(labelFailure)
	for from, tos := range domain.AllowedTransitions() {
		for _, to := range tos {
			if from != to {
				m.stateChanges.WithLabelValues(string(from), string(to))
			}
		}
	}
}

// Handler serves the registry in the Prometheus exposition format. It belongs
// on the internal listener only.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// Registry exposes the private registry, for tests.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveHTTPRequest records metrics 1 and 2 for one request. route must be
// the chi route pattern, never the raw path; an empty route means no route
// matched.
func (m *Metrics) ObserveHTTPRequest(method, route string, code int, d time.Duration) {
	if m == nil {
		return
	}
	method = NormalizeMethod(method)
	route = NormalizeRoute(route)
	m.httpRequests.WithLabelValues(method, route, normalizeCode(code)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// ObserveCheck records metrics 3 and 4 for one finished check. kind is
// ignored when success is true; a failure without a valid kind is counted as
// unknown.
func (m *Metrics) ObserveCheck(success bool, kind *domain.ErrorKind, d time.Duration) {
	if m == nil {
		return
	}
	result, errorKind := labelSuccess, labelNone
	if !success {
		result, errorKind = labelFailure, normalizeErrorKind(kind)
	}
	m.checks.WithLabelValues(result, errorKind).Inc()
	m.checkDuration.WithLabelValues(result).Observe(d.Seconds())
}

// StateChanged records metric 5. Self-loops (from == to) are not changes and
// are ignored.
func (m *Metrics) StateChanged(from, to domain.State) {
	if m == nil || from == to {
		return
	}
	m.stateChanges.WithLabelValues(normalizeState(from), normalizeState(to)).Inc()
}

// AddClaimed records metric 6: n monitors were claimed. Non-positive n is
// ignored.
func (m *Metrics) AddClaimed(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.claimedMonitors.Add(float64(n))
}

// ClaimError records metric 8: one claim attempt failed.
func (m *Metrics) ClaimError() {
	if m == nil {
		return
	}
	m.claimErrors.Inc()
}

// ActiveChecksInc marks one check as started (metric 7).
func (m *Metrics) ActiveChecksInc() {
	if m == nil {
		return
	}
	m.activeChecks.Inc()
}

// ActiveChecksDec marks one check as finished (metric 7).
func (m *Metrics) ActiveChecksDec() {
	if m == nil {
		return
	}
	m.activeChecks.Dec()
}

// ObserveSchedulerLag records metric 9 for one claimed monitor. A negative
// lag (a claim slightly before next_check_at, through clock skew) counts as
// zero.
func (m *Metrics) ObserveSchedulerLag(d time.Duration) {
	if m == nil {
		return
	}
	m.schedulerLag.Observe(max(d, 0).Seconds())
}

// ObserveDBOperation records metric 10. op is the sqlc query name; anything
// that does not look like one is recorded as "other".
func (m *Metrics) ObserveDBOperation(op string, err error, d time.Duration) {
	if m == nil {
		return
	}
	m.dbOperationLatency.WithLabelValues(normalizeOperation(op), dbOutcome(err)).Observe(d.Seconds())
}

// NormalizeMethod maps an HTTP method onto the bounded label set: the
// standard methods the API can see, else OTHER. Matching is case-sensitive,
// as in HTTP itself.
func NormalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	default:
		return labelOther
	}
}

// NormalizeRoute returns the chi route pattern, or "unmatched" when no route
// matched. A value that cannot be a registered pattern (not starting with a
// slash, or implausibly long) is also "unmatched", so a raw path passed by
// mistake cannot explode the series count with arbitrarily long values.
func NormalizeRoute(route string) string {
	if route == "" || route[0] != '/' || len(route) > maxRouteLength {
		return labelUnmatched
	}
	return route
}

func normalizeCode(code int) string {
	if code < 100 || code > 599 {
		return labelUnknown
	}
	return strconv.Itoa(code)
}

func normalizeErrorKind(kind *domain.ErrorKind) string {
	if kind == nil || !kind.Valid() {
		return labelUnknown
	}
	return string(*kind)
}

func normalizeState(s domain.State) string {
	if !s.Valid() {
		return labelUnknown
	}
	return string(s)
}

// maxOperationLength bounds the operation label; sqlc names are short Go
// identifiers.
const maxOperationLength = 64

// normalizeOperation accepts a Go-identifier-shaped sqlc query name and maps
// everything else to "other".
func normalizeOperation(op string) string {
	if op == "" || len(op) > maxOperationLength {
		return opOther
	}
	for i, r := range op {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r == '_' || (r >= '0' && r <= '9'):
			if i == 0 {
				return opOther
			}
		default:
			return opOther
		}
	}
	return op
}

func dbOutcome(err error) string {
	if err == nil {
		return labelOK
	}
	return labelError
}
