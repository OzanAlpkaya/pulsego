package monitor

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// FieldError names one rejected field and says what was wrong with it.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError collects every problem with a request rather than stopping
// at the first, so a caller fixing a monitor definition needs one round trip.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

func (e *ValidationError) add(field, format string, args ...any) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// err returns the error only when something was actually rejected, so callers
// can write `return v.err()` unconditionally.
func (e *ValidationError) err() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}

// attributes are the fully resolved values of a monitor — defaults applied for
// a create, existing values merged in for an update. Validation always runs
// against these rather than against the request, because the cross-field rules
// are only meaningful once every value is known.
type attributes struct {
	name              string
	rawURL            string
	intervalSeconds   int32
	timeoutMS         int32
	expectedStatusMin int16
	expectedStatusMax int16
	failureThreshold  int16

	// skipPolicy leaves out the SSRF policy check on the URL while keeping
	// the syntactic ones. Update sets it when the request does not change
	// the URL, so a monitor stored before the policy existed can still be
	// renamed or disabled. The checker's security.Dialer still blocks such
	// targets at check time. The zero value checks, so creates stay strict.
	skipPolicy bool
}

func (a attributes) validate(policy security.Policy) error {
	v := &ValidationError{}

	a.validateName(v)
	a.validateURL(v, policy)
	a.validateInterval(v)
	a.validateTimeout(v)
	a.validateStatusRange(v)
	a.validateFailureThreshold(v)

	// A check that may run longer than the gap between checks would overlap
	// with itself. The individual bounds allow it (10s interval, 60s timeout),
	// so the combination has to be rejected here.
	if a.timeoutMS > 0 && a.intervalSeconds > 0 &&
		int64(a.timeoutMS) >= int64(a.intervalSeconds)*1000 {
		v.add("timeout_ms", "must be less than interval_seconds (%ds), otherwise checks would overlap",
			a.intervalSeconds)
	}

	return v.err()
}

func (a attributes) validateName(v *ValidationError) {
	switch name := strings.TrimSpace(a.name); {
	case name == "":
		v.add("name", "is required")
	case utf8.RuneCountInString(name) > domain.MaxNameLength:
		v.add("name", "must be at most %d characters", domain.MaxNameLength)
	}
}

// validateURL checks the syntax, then runs the SSRF policy's static check:
// scheme, port 80/443, and no literal private, loopback, link-local or
// reserved address, localhost or ambiguous numeric host. It deliberately does
// no DNS — resolving inside a request handler would be slow and a
// time-of-check/time-of-use trap. Host names are checked once resolved, by
// the checker's security.Dialer, which is the real guarantee.
func (a attributes) validateURL(v *ValidationError, policy security.Policy) {
	raw := strings.TrimSpace(a.rawURL)
	if raw == "" {
		v.add("url", "is required")
		return
	}
	if len(raw) > domain.MaxURLLength {
		v.add("url", "must be at most %d characters", domain.MaxURLLength)
		return
	}

	u, err := url.Parse(raw)
	if err != nil {
		v.add("url", "is not a valid URL")
		return
	}

	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		v.add("url", "must use the http or https scheme")
	case u.Host == "":
		v.add("url", "must include a host")
	case u.User != nil:
		v.add("url", "must not embed credentials")
	case u.Fragment != "":
		v.add("url", "must not include a fragment")
	case a.skipPolicy:
	default:
		if err := policy.ValidateURL(raw); err != nil {
			// The policy error names the host; the message stays generic so
			// the response shape does not depend on internal/security's
			// wording.
			if policy.AllowPrivate {
				v.add("url", "is not an allowed monitoring target")
			} else {
				v.add("url", "must be a public host on port 80 or 443; private, loopback, link-local and reserved addresses and localhost are not allowed")
			}
		}
	}
}

func (a attributes) validateInterval(v *ValidationError) {
	if a.intervalSeconds < domain.MinCheckIntervalSeconds || a.intervalSeconds > domain.MaxCheckIntervalSeconds {
		v.add("interval_seconds", "must be between %d and %d",
			domain.MinCheckIntervalSeconds, domain.MaxCheckIntervalSeconds)
	}
}

func (a attributes) validateTimeout(v *ValidationError) {
	if a.timeoutMS < domain.MinCheckTimeoutMS || a.timeoutMS > domain.MaxCheckTimeoutMS {
		v.add("timeout_ms", "must be between %d and %d",
			domain.MinCheckTimeoutMS, domain.MaxCheckTimeoutMS)
	}
}

func (a attributes) validateStatusRange(v *ValidationError) {
	inRange := func(code int16) bool {
		return code >= domain.MinStatusCode && code <= domain.MaxStatusCode
	}

	if !inRange(a.expectedStatusMin) {
		v.add("expected_status_min", "must be between %d and %d", domain.MinStatusCode, domain.MaxStatusCode)
	}
	if !inRange(a.expectedStatusMax) {
		v.add("expected_status_max", "must be between %d and %d", domain.MinStatusCode, domain.MaxStatusCode)
	}
	if inRange(a.expectedStatusMin) && inRange(a.expectedStatusMax) &&
		a.expectedStatusMin > a.expectedStatusMax {
		v.add("expected_status_min", "must not be greater than expected_status_max")
	}
}

func (a attributes) validateFailureThreshold(v *ValidationError) {
	if a.failureThreshold < domain.MinFailureThreshold || a.failureThreshold > domain.MaxFailureThreshold {
		v.add("failure_threshold", "must be between %d and %d",
			domain.MinFailureThreshold, domain.MaxFailureThreshold)
	}
}
