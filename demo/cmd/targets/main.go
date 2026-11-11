// Command targets serves deterministic HTTP endpoints for PulseGo demos.
//
// It is local demo tooling only: PulseGo refuses loopback targets unless the
// api and worker run with ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true.
//
// Endpoints:
//
//	GET  /steady         200 after a random 15–60 ms (-base-min, -base-max)
//	GET  /slow           200 after a random 300–800 ms, so p95 is non-trivial
//	GET  /flaky          200 while "up", 503 while "down" (base latency)
//	GET  /unstable       503 on every -unstable-every'th request, else 200
//	                     (base latency)
//	POST /control/flaky  ?state=up|down flips /flaky; answers the new state
//	GET  /control/flaky  answers the current state
//
// /flaky can also flip on its own schedule (-flaky-up, -flaky-down); with
// -flaky-down=0 (the default) it only changes through the control endpoint,
// which keeps incident timing deterministic for the capture tool.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18082", "listen address (keep it on loopback)")
	baseMin := flag.Duration("base-min", 15*time.Millisecond, "minimum latency of the fast endpoints")
	baseMax := flag.Duration("base-max", 60*time.Millisecond, "maximum latency of the fast endpoints")
	slowMin := flag.Duration("slow-min", 300*time.Millisecond, "minimum /slow latency")
	slowMax := flag.Duration("slow-max", 800*time.Millisecond, "maximum /slow latency")
	unstableEvery := flag.Int("unstable-every", 4, "/unstable fails one request in this many (0 never fails)")
	flakyUp := flag.Duration("flaky-up", 0, "with -flaky-down, how long /flaky stays up per cycle")
	flakyDown := flag.Duration("flaky-down", 0, "how long /flaky stays down per cycle (0 = control endpoint only)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *slowMax < *slowMin || *baseMax < *baseMin {
		log.Error("a -*-max latency must not be below its -*-min")
		os.Exit(2)
	}

	t := &targets{
		base:          latency{*baseMin, *baseMax},
		slow:          latency{*slowMin, *slowMax},
		unstableEvery: int64(*unstableEvery),
		log:           log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *flakyDown > 0 {
		if *flakyUp <= 0 {
			log.Error("flaky-up must be positive when flaky-down is set")
			os.Exit(2)
		}
		go t.cycleFlaky(ctx, *flakyUp, *flakyDown)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           t.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("demo targets listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

// latency is a uniform random delay in [min, max].
type latency struct{ min, max time.Duration }

func (l latency) draw() time.Duration {
	d := l.min
	if span := l.max - l.min; span > 0 {
		d += rand.N(span) // #nosec G404 -- demo latency jitter, not security
	}
	return d
}

type targets struct {
	base, slow    latency
	unstableEvery int64
	log           *slog.Logger

	flakyDown atomic.Bool
	unstableN atomic.Int64
	mu        sync.Mutex // serialises scheduled and manual flips for clean logs
}

func (t *targets) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /steady", delayed(t.base, func(w http.ResponseWriter, _ *http.Request) {
		text(w, http.StatusOK, "ok")
	}))
	mux.HandleFunc("GET /slow", delayed(t.slow, func(w http.ResponseWriter, _ *http.Request) {
		text(w, http.StatusOK, "ok")
	}))
	mux.HandleFunc("GET /flaky", delayed(t.base, func(w http.ResponseWriter, _ *http.Request) {
		if t.flakyDown.Load() {
			text(w, http.StatusServiceUnavailable, "down")
			return
		}
		text(w, http.StatusOK, "ok")
	}))
	mux.HandleFunc("GET /unstable", delayed(t.base, func(w http.ResponseWriter, _ *http.Request) {
		n := t.unstableN.Add(1)
		if t.unstableEvery > 0 && n%t.unstableEvery == 0 {
			text(w, http.StatusServiceUnavailable, "down")
			return
		}
		text(w, http.StatusOK, "ok")
	}))
	mux.HandleFunc("GET /control/flaky", func(w http.ResponseWriter, _ *http.Request) {
		text(w, http.StatusOK, t.flakyState())
	})
	mux.HandleFunc("POST /control/flaky", func(w http.ResponseWriter, r *http.Request) {
		switch s := r.URL.Query().Get("state"); s {
		case "up", "down":
			t.setFlaky(s == "down", "control")
			text(w, http.StatusOK, s)
		default:
			text(w, http.StatusBadRequest, "state must be up or down")
		}
	})
	return mux
}

// delayed waits a random latency before calling next; a request cancelled
// meanwhile gets no answer.
func delayed(l latency, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(l.draw()):
			next(w, r)
		case <-r.Context().Done():
		}
	}
}

func (t *targets) flakyState() string {
	if t.flakyDown.Load() {
		return "down"
	}
	return "up"
}

func (t *targets) setFlaky(down bool, source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flakyDown.Store(down)
	t.log.Info("flaky switched", "state", t.flakyState(), "source", source)
}

func (t *targets) cycleFlaky(ctx context.Context, up, down time.Duration) {
	for {
		for _, phase := range []struct {
			down bool
			wait time.Duration
		}{{false, up}, {true, down}} {
			t.setFlaky(phase.down, "schedule")
			select {
			case <-ctx.Done():
				return
			case <-time.After(phase.wait):
			}
		}
	}
}

func text(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, body) // #nosec G705 -- fixed strings served as text/plain
}
