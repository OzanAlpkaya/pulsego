package security

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrTooManyRedirects is wrapped by the error RedirectPolicy returns once the
// redirect limit is exceeded. Callers classify it with errors.Is (the checker
// maps it to error kind redirect).
var ErrTooManyRedirects = errors.New("too many redirects")

// RedirectPolicy returns an http.Client CheckRedirect func that follows at
// most maxRedirects redirects and validates every hop's URL with p. A hop
// that fails validation returns an error wrapping ErrBlockedTarget; the
// Dialer still checks the resolved addresses of any hop that passes.
func RedirectPolicy(p Policy, maxRedirects int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if err := p.validate(req.URL); err != nil {
			return err
		}
		// via holds every request already sent, so the request about to be
		// sent is redirect number len(via).
		if len(via) > maxRedirects {
			return fmt.Errorf("%w: stopped after %d", ErrTooManyRedirects, maxRedirects)
		}
		return nil
	}
}
