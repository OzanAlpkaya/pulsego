package domain

import (
	"fmt"
	"math"
)

// IncidentAction tells the caller of Transition what, if anything, must
// happen to the monitor's incidents as a result of a state change.
type IncidentAction int

const (
	// IncidentNone means no incident is opened or resolved.
	IncidentNone IncidentAction = iota
	// IncidentOpen means a new incident must be opened. It is returned only
	// on the degraded→down edge.
	IncidentOpen
	// IncidentResolve means the open incident must be resolved. It is
	// returned only on the down→up edge.
	IncidentResolve
)

// String implements fmt.Stringer.
func (a IncidentAction) String() string {
	switch a {
	case IncidentNone:
		return "none"
	case IncidentOpen:
		return "open"
	case IncidentResolve:
		return "resolve"
	default:
		return fmt.Sprintf("IncidentAction(%d)", int(a))
	}
}

// StateChange is the outcome of applying one check result to a monitor.
// From and To may be equal: self-loops are valid transitions.
type StateChange struct {
	From                State
	To                  State
	ConsecutiveFailures int32
	Incident            IncidentAction
	// IncidentAlreadyOpen is never set by Transition. The store that records
	// a check result sets it when Incident is IncidentOpen but the monitor
	// already had an open incident, so none was opened. That is only
	// possible after a hand edit of state or incidents; the result is still
	// recorded, and the caller should log a warning.
	IncidentAlreadyOpen bool
}

// allowedTransitions is the complete set of edges of the state machine,
// self-loops included. Anything not listed here — notably up→down,
// unknown→down and down→degraded — can never be produced by Transition.
var allowedTransitions = map[State][]State{
	StateUnknown:  {StateUp, StateDegraded},
	StateUp:       {StateUp, StateDegraded},
	StateDegraded: {StateUp, StateDegraded, StateDown},
	StateDown:     {StateDown, StateUp},
}

// AllowedTransitions returns every allowed edge, keyed by the source state.
// The returned map is a copy and may be modified by the caller.
func AllowedTransitions() map[State][]State {
	out := make(map[State][]State, len(allowedTransitions))
	for from, tos := range allowedTransitions {
		out[from] = append([]State(nil), tos...)
	}
	return out
}

// IsAllowedTransition reports whether from→to is an edge of the state
// machine.
func IsAllowedTransition(from, to State) bool {
	for _, s := range allowedTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition computes the next state of a monitor from its current state,
// its consecutive failure count, its failure threshold, and the outcome of
// the latest check. It is pure: callers persist the result.
//
// A success resets the failure count to 0 and moves any state to up. A
// failure increments the count (saturating at math.MaxInt32) and moves
// unknown or up to degraded, degraded to down once the new count reaches
// the threshold, and keeps down at down. Because up→down is not an edge, a
// monitor needs max(threshold, 2) consecutive failures to reach down.
//
// IncidentOpen is returned only on degraded→down and IncidentResolve only
// on down→up. An invalid cur, or a threshold below 1, is an error.
func Transition(cur State, consecutiveFailures int32, threshold int16, success bool) (StateChange, error) {
	if !cur.Valid() {
		return StateChange{}, fmt.Errorf("transition: invalid state %q", cur)
	}
	if threshold < 1 {
		return StateChange{}, fmt.Errorf("transition: failure threshold %d is below 1", threshold)
	}

	change := StateChange{From: cur}

	if success {
		change.To = StateUp
		if cur == StateDown {
			change.Incident = IncidentResolve
		}
		return change, nil
	}

	failures := consecutiveFailures
	if failures < math.MaxInt32 {
		failures++
	}
	change.ConsecutiveFailures = failures

	switch cur {
	case StateUnknown, StateUp:
		change.To = StateDegraded
	case StateDegraded:
		if failures >= int32(threshold) {
			change.To = StateDown
			change.Incident = IncidentOpen
		} else {
			change.To = StateDegraded
		}
	case StateDown:
		change.To = StateDown
	}
	return change, nil
}
