package monitor

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
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
}

func (a attributes) validate() error {
	v := &ValidationError{}

	a.validateName(v)
	a.validateURL(v)
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

// validateURL checks syntax only. Whether the target is safe to contact —
// loopback, private ranges, DNS rebinding — is decided by internal/security at
// check time, not here: resolving DNS inside a request handler would be both
// slow and a time-of-check/time-of-use trap.
func (a attributes) validateURL(v *ValidationError) {
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
