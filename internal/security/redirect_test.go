package security

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const publicIP = "93.184.215.14"

// redirectClient returns a client whose every connection that passes the
// dialer lands on srv, plus the recorder of dialled addresses.
func redirectClient(t *testing.T, srv *httptest.Server, maxRedirects int) (*http.Client, *recordingDial) {
	t.Helper()
	res := newFakeResolver(map[string][]string{
		"public.example":   {publicIP},
		"internal.example": {"10.0.0.5"},
	})
	rec := &recordingDial{target: srv.Listener.Addr().String()}
	d := NewDialer(Policy{}, WithResolver(res), WithDialFunc(rec.dial))
	client := &http.Client{
		Transport:     &http.Transport{DialContext: d.DialContext, Proxy: nil},
		CheckRedirect: RedirectPolicy(Policy{}, maxRedirects),
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, rec
}

func get(ctx context.Context, c *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func TestRedirectToPrivateIsBlocked(t *testing.T) {
	t.Parallel()

	targets := map[string]string{
		"/loopback": "http://127.0.0.1/",
		"/metadata": "http://169.254.169.254/latest/meta-data/",
		"/internal": "http://internal.example/",
		"/port":     "http://public.example:8080/",
		"/userinfo": "http://user:pw@public.example/",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loc, ok := targets[r.URL.Path]; ok {
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	for path := range targets {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			client, rec := redirectClient(t, srv, 5)
			err := get(context.Background(), client, "http://public.example"+path)
			if !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("GET %s = %v, want ErrBlockedTarget", path, err)
			}
			for _, addr := range rec.dialled() {
				if addr != publicIP+":80" {
					t.Fatalf("blocked hop was dialled: %s", addr)
				}
			}
			if len(rec.dialled()) == 0 {
				t.Fatal("the first (public) hop was never dialled")
			}
		})
	}

	t.Run("public hop is followed", func(t *testing.T) {
		t.Parallel()
		client, _ := redirectClient(t, srv, 5)
		if err := get(context.Background(), client, "http://public.example/ok"); err != nil {
			t.Fatalf("GET /ok: %v", err)
		}
	})
}

func TestRedirectLimit(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		http.Redirect(w, r, fmt.Sprintf("http://public.example/%d", n+1), http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	const maxRedirects = 5
	client, _ := redirectClient(t, srv, maxRedirects)
	err := get(context.Background(), client, "http://public.example/0")
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("GET = %v, want ErrTooManyRedirects", err)
	}
	if errors.Is(err, ErrBlockedTarget) {
		t.Fatal("redirect limit must not be classified as blocked")
	}
	if got := hits.Load(); got != maxRedirects+1 {
		t.Fatalf("server hits = %d, want %d (initial + %d redirects)", got, maxRedirects+1, maxRedirects)
	}
}

func TestRedirectPolicyErrorOmitsQuery(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3tvalue"
	check := RedirectPolicy(Policy{}, 5)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://user:"+secret+"@127.0.0.1/?token="+secret, nil)
	err := check(req, []*http.Request{req})
	if !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("err = %v, want ErrBlockedTarget", err)
	}
	if msg := err.Error(); strings.Contains(msg, secret) || strings.Contains(msg, "token") {
		t.Fatalf("error %q leaks the query or userinfo", msg)
	}
}
