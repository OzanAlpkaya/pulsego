package domain_test

import (
	"math"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

func TestTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cur          domain.State
		failures     int32
		threshold    int16
		success      bool
		wantTo       domain.State
		wantFailures int32
		wantIncident domain.IncidentAction
	}{
		// The nine allowed edges.
		{
			name: "unknown to up on success", cur: domain.StateUnknown,
			threshold: 3, success: true,
			wantTo: domain.StateUp, wantFailures: 0, wantIncident: domain.IncidentNone,
		},
		{
			name: "unknown to degraded on failure", cur: domain.StateUnknown,
			threshold: 3, success: false,
			wantTo: domain.StateDegraded, wantFailures: 1, wantIncident: domain.IncidentNone,
		},
		{
			name: "up to up on success", cur: domain.StateUp,
			threshold: 3, success: true,
			wantTo: domain.StateUp, wantFailures: 0, wantIncident: domain.IncidentNone,
		},
		{
			name: "up to degraded on failure", cur: domain.StateUp,
			threshold: 3, success: false,
			wantTo: domain.StateDegraded, wantFailures: 1, wantIncident: domain.IncidentNone,
		},
		{
			name: "degraded to up on success resets failures", cur: domain.StateDegraded,
			failures: 2, threshold: 3, success: true,
			wantTo: domain.StateUp, wantFailures: 0, wantIncident: domain.IncidentNone,
		},
		{
			name: "degraded to degraded below threshold", cur: domain.StateDegraded,
			failures: 1, threshold: 3, success: false,
			wantTo: domain.StateDegraded, wantFailures: 2, wantIncident: domain.IncidentNone,
		},
		{
			name: "degraded to down at threshold opens incident", cur: domain.StateDegraded,
			failures: 2, threshold: 3, success: false,
			wantTo: domain.StateDown, wantFailures: 3, wantIncident: domain.IncidentOpen,
		},
		{
			name: "down to down increments failures", cur: domain.StateDown,
			failures: 3, threshold: 3, success: false,
			wantTo: domain.StateDown, wantFailures: 4, wantIncident: domain.IncidentNone,
		},
		{
			name: "down to up resolves incident", cur: domain.StateDown,
			failures: 7, threshold: 3, success: true,
			wantTo: domain.StateUp, wantFailures: 0, wantIncident: domain.IncidentResolve,
		},

		// Threshold 1: down takes max(threshold, 2) = 2 failures.
		{
			name: "threshold 1 first failure from up is degraded", cur: domain.StateUp,
			threshold: 1, success: false,
			wantTo: domain.StateDegraded, wantFailures: 1, wantIncident: domain.IncidentNone,
		},
		{
			name: "threshold 1 first failure from unknown is degraded", cur: domain.StateUnknown,
			threshold: 1, success: false,
			wantTo: domain.StateDegraded, wantFailures: 1, wantIncident: domain.IncidentNone,
		},
		{
			name: "threshold 1 second failure is down", cur: domain.StateDegraded,
			failures: 1, threshold: 1, success: false,
			wantTo: domain.StateDown, wantFailures: 2, wantIncident: domain.IncidentOpen,
		},

		// Lowering the threshold mid-outage takes effect on the next failure.
		{
			name: "degraded above lowered threshold goes down", cur: domain.StateDegraded,
			failures: 5, threshold: 2, success: false,
			wantTo: domain.StateDown, wantFailures: 6, wantIncident: domain.IncidentOpen,
		},

		// Saturation.
		{
			name: "down saturates at MaxInt32", cur: domain.StateDown,
			failures: math.MaxInt32, threshold: 3, success: false,
			wantTo: domain.StateDown, wantFailures: math.MaxInt32, wantIncident: domain.IncidentNone,
		},
		{
			name: "down reaches MaxInt32", cur: domain.StateDown,
			failures: math.MaxInt32 - 1, threshold: 3, success: false,
			wantTo: domain.StateDown, wantFailures: math.MaxInt32, wantIncident: domain.IncidentNone,
		},
		{
			name: "success from saturated down resets", cur: domain.StateDown,
			failures: math.MaxInt32, threshold: 3, success: true,
			wantTo: domain.StateUp, wantFailures: 0, wantIncident: domain.IncidentResolve,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := domain.Transition(tc.cur, tc.failures, tc.threshold, tc.success)
			if err != nil {
				t.Fatalf("Transition() error = %v, want nil", err)
			}
			if got.From != tc.cur {
				t.Errorf("From = %q, want %q", got.From, tc.cur)
			}
			if got.To != tc.wantTo {
				t.Errorf("To = %q, want %q", got.To, tc.wantTo)
			}
			if got.ConsecutiveFailures != tc.wantFailures {
				t.Errorf("ConsecutiveFailures = %d, want %d", got.ConsecutiveFailures, tc.wantFailures)
			}
			if got.Incident != tc.wantIncident {
				t.Errorf("Incident = %v, want %v", got.Incident, tc.wantIncident)
			}
		})
	}
}

func TestTransitionCoversEveryAllowedEdge(t *testing.T) {
	t.Parallel()

	edges := 0
	for _, tos := range domain.AllowedTransitions() {
		edges += len(tos)
	}
	if edges != 9 {
		t.Fatalf("AllowedTransitions() has %d edges, want 9", edges)
	}
	for _, e := range [][2]domain.State{
		{domain.StateUp, domain.StateDown},
		{domain.StateUnknown, domain.StateDown},
		{domain.StateDown, domain.StateDegraded},
		{domain.StateUp, domain.StateUnknown},
		{domain.StateUnknown, domain.StateUnknown},
	} {
		if domain.IsAllowedTransition(e[0], e[1]) {
			t.Errorf("IsAllowedTransition(%q, %q) = true, want false", e[0], e[1])
		}
	}
}

func TestTransitionErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cur       domain.State
		threshold int16
	}{
		{name: "empty state", cur: "", threshold: 3},
		{name: "unrecognised state", cur: "broken", threshold: 3},
		{name: "wrong case state", cur: "UP", threshold: 3},
		{name: "zero threshold", cur: domain.StateUp, threshold: 0},
		{name: "negative threshold", cur: domain.StateDegraded, threshold: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, success := range []bool{true, false} {
				got, err := domain.Transition(tc.cur, 0, tc.threshold, success)
				if err == nil {
					t.Errorf("Transition(%q, 0, %d, %v) error = nil, want an error", tc.cur, tc.threshold, success)
				}
				if got != (domain.StateChange{}) {
					t.Errorf("Transition() on error = %+v, want zero StateChange", got)
				}
			}
		})
	}
}

func TestTransitionNeverLeavesAllowedEdges(t *testing.T) {
	t.Parallel()

	for _, cur := range domain.States() {
		for _, success := range []bool{true, false} {
			for failures := int32(0); failures <= 12; failures++ {
				for threshold := int16(1); threshold <= 10; threshold++ {
					got, err := domain.Transition(cur, failures, threshold, success)
					if err != nil {
						t.Fatalf("Transition(%q, %d, %d, %v) error = %v", cur, failures, threshold, success, err)
					}
					if got.From != cur {
						t.Errorf("Transition(%q, %d, %d, %v).From = %q", cur, failures, threshold, success, got.From)
					}
					if !domain.IsAllowedTransition(got.From, got.To) {
						t.Errorf("Transition(%q, %d, %d, %v) produced disallowed edge %s→%s",
							cur, failures, threshold, success, got.From, got.To)
					}

					wantIncident := domain.IncidentNone
					switch {
					case got.From == domain.StateDegraded && got.To == domain.StateDown:
						wantIncident = domain.IncidentOpen
					case got.From == domain.StateDown && got.To == domain.StateUp:
						wantIncident = domain.IncidentResolve
					}
					if got.Incident != wantIncident {
						t.Errorf("Transition(%q, %d, %d, %v) edge %s→%s Incident = %v, want %v",
							cur, failures, threshold, success, got.From, got.To, got.Incident, wantIncident)
					}

					wantFailures := failures + 1
					if success {
						wantFailures = 0
					}
					if got.ConsecutiveFailures != wantFailures {
						t.Errorf("Transition(%q, %d, %d, %v).ConsecutiveFailures = %d, want %d",
							cur, failures, threshold, success, got.ConsecutiveFailures, wantFailures)
					}
					if success && got.To != domain.StateUp {
						t.Errorf("Transition(%q, %d, %d, true).To = %q, want up", cur, failures, threshold, got.To)
					}
				}
			}
		}
	}
}

func TestIncidentActionString(t *testing.T) {
	t.Parallel()

	tests := map[domain.IncidentAction]string{
		domain.IncidentNone:       "none",
		domain.IncidentOpen:       "open",
		domain.IncidentResolve:    "resolve",
		domain.IncidentAction(42): "IncidentAction(42)",
	}
	for action, want := range tests {
		if got := action.String(); got != want {
			t.Errorf("IncidentAction(%d).String() = %q, want %q", int(action), got, want)
		}
	}
}
