package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// TypeNotReady is the problem type of a failed readiness probe.
const TypeNotReady = "urn:pulsego:problem:not-ready"

// readyzTimeout bounds the database ping of one readiness probe (D6), so a
// hung database makes the probe fail rather than hang.
const readyzTimeout = 2 * time.Second

// Pinger is the dependency check behind /readyz. *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// errNoPinger reports readiness wiring that was left out. Failing closed is
// deliberate: a process that cannot check its database is not ready.
var errNoPinger = errors.New("no readiness pinger configured")

// Readyz reports whether the process can serve traffic: it pings PostgreSQL
// with a 2s timeout. Unlike /healthz it depends on the database, so it is what
// a load balancer should use to route traffic, never to restart a process.
//
// A failure is a 503 problem without error detail; the cause goes to the log
// at warn, because a driver error can name hosts and users.
func Readyz(p Pinger, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := errNoPinger
		if p != nil {
			ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
			err = p.Ping(ctx)
			cancel()
		}

		if err != nil {
			log.WarnContext(r.Context(), "readiness check failed", slog.Any("error", err))
			writeProblem(w, r, log, Problem{
				Type:   TypeNotReady,
				Title:  "Not Ready",
				Status: http.StatusServiceUnavailable,
				Detail: "A dependency of the service is unavailable.",
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}
}
