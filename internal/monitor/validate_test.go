package monitor

import (
	"errors"
	"strings"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// validAttrs is the baseline every case mutates one field of.
func validAttrs() attributes {
	return attributes{
		name:              "Example",
		rawURL:            "https://example.com/health",
		intervalSeconds:   60,
		timeoutMS:         5000,
		expectedStatusMin: 200,
		expectedStatusMax: 399,
		failureThreshold:  3,
	}
}

func TestAttributesValidateAcceptsBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*attributes)
	}{
		{name: "baseline", mutate: func(*attributes) {}},
		{name: "minimum interval", mutate: func(a *attributes) { a.intervalSeconds = 10; a.timeoutMS = 100 }},
		{name: "maximum interval", mutate: func(a *attributes) { a.intervalSeconds = 86400 }},
		{name: "minimum timeout", mutate: func(a *attributes) { a.timeoutMS = 100 }},
		{name: "maximum timeout", mutate: func(a *attributes) { a.timeoutMS = 60000; a.intervalSeconds = 61 }},
		{name: "minimum status codes", mutate: func(a *attributes) { a.expectedStatusMin = 100; a.expectedStatusMax = 100 }},
		{name: "maximum status codes", mutate: func(a *attributes) { a.expectedStatusMin = 599; a.expectedStatusMax = 599 }},
		{name: "minimum failure threshold", mutate: func(a *attributes) { a.failureThreshold = 1 }},
		{name: "maximum failure threshold", mutate: func(a *attributes) { a.failureThreshold = 10 }},
		{name: "name at maximum length", mutate: func(a *attributes) { a.name = strings.Repeat("a", 120) }},
		{name: "multibyte name counted in runes", mutate: func(a *attributes) { a.name = strings.Repeat("ü", 120) }},
		{name: "http scheme", mutate: func(a *attributes) { a.rawURL = "http://example.com" }},
		{name: "url with query", mutate: func(a *attributes) { a.rawURL = "https://example.com/x?a=1&b=2" }},
		{name: "url with default https port", mutate: func(a *attributes) { a.rawURL = "https://example.com:443/health" }},
		{name: "public IP literal", mutate: func(a *attributes) { a.rawURL = "http://93.184.215.14/" }},
		{name: "surrounding whitespace is tolerated", mutate: func(a *attributes) { a.name = "  Example  " }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := validAttrs()
			tt.mutate(&a)

			if err := a.validate(security.Policy{}); err != nil {
				t.Errorf("validate() returned an unexpected error: %v", err)
			}
		})
	}
}

func TestAttributesValidateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*attributes)
		wantField string
		wantText  string
	}{
		{
			name:      "empty name",
			mutate:    func(a *attributes) { a.name = "" },
			wantField: "name", wantText: "is required",
		},
		{
			name:      "whitespace-only name",
			mutate:    func(a *attributes) { a.name = "   " },
			wantField: "name", wantText: "is required",
		},
		{
			name:      "name one rune too long",
			mutate:    func(a *attributes) { a.name = strings.Repeat("a", 121) },
			wantField: "name", wantText: "at most 120 characters",
		},
		{
			name:      "empty url",
			mutate:    func(a *attributes) { a.rawURL = "" },
			wantField: "url", wantText: "is required",
		},
		{
			name:      "url too long",
			mutate:    func(a *attributes) { a.rawURL = "https://example.com/" + strings.Repeat("a", 2048) },
			wantField: "url", wantText: "at most 2048 characters",
		},
		{
			name:      "unsupported scheme",
			mutate:    func(a *attributes) { a.rawURL = "ftp://example.com" },
			wantField: "url", wantText: "http or https",
		},
		{
			name:      "file scheme",
			mutate:    func(a *attributes) { a.rawURL = "file:///etc/passwd" },
			wantField: "url", wantText: "http or https",
		},
		{
			name:      "no host",
			mutate:    func(a *attributes) { a.rawURL = "https:///health" },
			wantField: "url", wantText: "must include a host",
		},
		{
			name:      "embedded credentials",
			mutate:    func(a *attributes) { a.rawURL = "https://user:pass@example.com" },
			wantField: "url", wantText: "must not embed credentials",
		},
		{
			name:      "fragment",
			mutate:    func(a *attributes) { a.rawURL = "https://example.com/#section" },
			wantField: "url", wantText: "must not include a fragment",
		},
		{
			name:      "interval below minimum",
			mutate:    func(a *attributes) { a.intervalSeconds = 9 },
			wantField: "interval_seconds", wantText: "between 10 and 86400",
		},
		{
			name:      "interval above maximum",
			mutate:    func(a *attributes) { a.intervalSeconds = 86401 },
			wantField: "interval_seconds", wantText: "between 10 and 86400",
		},
		{
			name:      "timeout below minimum",
			mutate:    func(a *attributes) { a.timeoutMS = 99 },
			wantField: "timeout_ms", wantText: "between 100 and 60000",
		},
		{
			name:      "timeout above maximum",
			mutate:    func(a *attributes) { a.timeoutMS = 60001 },
			wantField: "timeout_ms", wantText: "between 100 and 60000",
		},
		{
			name:      "status min below range",
			mutate:    func(a *attributes) { a.expectedStatusMin = 99 },
			wantField: "expected_status_min", wantText: "between 100 and 599",
		},
		{
			name:      "status max above range",
			mutate:    func(a *attributes) { a.expectedStatusMax = 600 },
			wantField: "expected_status_max", wantText: "between 100 and 599",
		},
		{
			name:      "inverted status range",
			mutate:    func(a *attributes) { a.expectedStatusMin, a.expectedStatusMax = 400, 200 },
			wantField: "expected_status_min", wantText: "not be greater than expected_status_max",
		},
		{
			name:      "failure threshold below minimum",
			mutate:    func(a *attributes) { a.failureThreshold = 0 },
			wantField: "failure_threshold", wantText: "between 1 and 10",
		},
		{
			name:      "failure threshold above maximum",
			mutate:    func(a *attributes) { a.failureThreshold = 11 },
			wantField: "failure_threshold", wantText: "between 1 and 10",
		},
		{
			name:      "timeout equal to interval",
			mutate:    func(a *attributes) { a.intervalSeconds = 10; a.timeoutMS = 10000 },
			wantField: "timeout_ms", wantText: "less than interval_seconds",
		},
		{
			name:      "timeout longer than interval",
			mutate:    func(a *attributes) { a.intervalSeconds = 10; a.timeoutMS = 30000 },
			wantField: "timeout_ms", wantText: "less than interval_seconds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a := validAttrs()
			tt.mutate(&a)

			err := a.validate(security.Policy{})
			if err == nil {
				t.Fatalf("validate() succeeded, want a rejection of %s", tt.wantField)
			}

			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("validate() returned %T, want *ValidationError", err)
			}

			for _, field := range verr.Fields {
				if field.Field == tt.wantField && strings.Contains(field.Message, tt.wantText) {
					return
				}
			}
			t.Errorf("no error on %q containing %q; got %+v", tt.wantField, tt.wantText, verr.Fields)
		})
	}
}

// TestValidateReportsEveryField matters because a caller fixing a monitor
// definition should need one round trip, not one per bad field.
func TestValidateReportsEveryField(t *testing.T) {
	t.Parallel()

	a := attributes{
		name:              "",
		rawURL:            "not a url at all",
		intervalSeconds:   1,
		timeoutMS:         1,
		expectedStatusMin: 700,
		expectedStatusMax: 800,
		failureThreshold:  99,
	}

	err := a.validate(security.Policy{})
	if err == nil {
		t.Fatal("validate() succeeded, want an error")
	}

	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("validate() returned %T, want *ValidationError", err)
	}

	seen := make(map[string]bool, len(verr.Fields))
	for _, f := range verr.Fields {
		seen[f.Field] = true
	}

	for _, want := range []string{
		"name", "url", "interval_seconds", "timeout_ms",
		"expected_status_min", "expected_status_max", "failure_threshold",
	} {
		if !seen[want] {
			t.Errorf("no error reported for %q; got %+v", want, verr.Fields)
		}
	}
}

// TestValidateURLAppliesSSRFPolicy covers the static SSRF check on the url
// field: rejected under the production policy, accepted (bar the rules that
// always apply) under the development one.
func TestValidateURLAppliesSSRFPolicy(t *testing.T) {
	t.Parallel()

	private := []string{
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://localhost:80/",
		"http://app.localhost/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.1/",
		"http://2130706433/",
		"https://example.com:8443/health",
	}
	for _, raw := range private {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			a := validAttrs()
			a.rawURL = raw
			err := a.validate(security.Policy{})
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("validate(strict) = %v, want a *ValidationError", err)
			}
			if len(verr.Fields) != 1 || verr.Fields[0].Field != "url" ||
				!strings.Contains(verr.Fields[0].Message, "must be a public host") {
				t.Fatalf("fields = %+v, want one url error", verr.Fields)
			}

			if err := a.validate(security.Policy{AllowPrivate: true}); err != nil {
				t.Fatalf("validate(AllowPrivate) = %v, want nil", err)
			}
		})
	}
}
