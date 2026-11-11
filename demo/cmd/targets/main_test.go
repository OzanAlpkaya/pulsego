package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestServer(t *testing.T, unstableEvery int64) *httptest.Server {
	t.Helper()
	tg := &targets{
		base:          latency{0, time.Millisecond},
		slow:          latency{10 * time.Millisecond, 20 * time.Millisecond},
		unstableEvery: unstableEvery,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(tg.routes())
	t.Cleanup(srv.Close)
	return srv
}

func status(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

func TestSteadyAndSlow(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, 0)
	if got := status(t, http.MethodGet, srv.URL+"/steady"); got != http.StatusOK {
		t.Fatalf("/steady = %d, want 200", got)
	}
	start := time.Now()
	if got := status(t, http.MethodGet, srv.URL+"/slow"); got != http.StatusOK {
		t.Fatalf("/slow = %d, want 200", got)
	}
	if d := time.Since(start); d < 10*time.Millisecond {
		t.Fatalf("/slow answered in %s, want at least slow-min", d)
	}
}

func TestFlakyControl(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, 0)
	steps := []struct {
		state string
		want  int
	}{{"down", http.StatusServiceUnavailable}, {"up", http.StatusOK}}
	if got := status(t, http.MethodGet, srv.URL+"/flaky"); got != http.StatusOK {
		t.Fatalf("/flaky starts at %d, want 200", got)
	}
	for _, s := range steps {
		if got := status(t, http.MethodPost, srv.URL+"/control/flaky?state="+s.state); got != http.StatusOK {
			t.Fatalf("set %s = %d", s.state, got)
		}
		if got := status(t, http.MethodGet, srv.URL+"/flaky"); got != s.want {
			t.Fatalf("after %s /flaky = %d, want %d", s.state, got, s.want)
		}
	}
	if got := status(t, http.MethodPost, srv.URL+"/control/flaky?state=sideways"); got != http.StatusBadRequest {
		t.Fatalf("bad state = %d, want 400", got)
	}
	if got := status(t, http.MethodGet, srv.URL+"/control/flaky?state=down"); got != http.StatusOK {
		t.Fatalf("GET control = %d, want 200", got)
	}
	if got := status(t, http.MethodGet, srv.URL+"/flaky"); got != http.StatusOK {
		t.Fatalf("GET on control must not switch /flaky, got %d", got)
	}
}

func TestUnstableFailsOneInN(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, 3)
	var fails int
	for i := 0; i < 9; i++ {
		if status(t, http.MethodGet, srv.URL+"/unstable") == http.StatusServiceUnavailable {
			fails++
		}
	}
	if fails != 3 {
		t.Fatalf("got %d failures in 9 requests, want 3", fails)
	}
}
