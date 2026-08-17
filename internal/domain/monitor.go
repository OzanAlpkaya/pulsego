// Package domain holds the core types of the monitoring model. It has no
// dependencies on transport, storage, or configuration packages so that the
// same definitions can be shared by the API, the worker, and the tests.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// State is the availability state of a monitor as derived from its recent
// check results. The set of values is mirrored by a CHECK constraint on
// monitors.state, so adding one here requires a migration.
type State string

const (
	// StateUnknown is the state of a monitor that has never been checked.
	StateUnknown State = "unknown"
	// StateUp means the last check succeeded and no failures are pending.
	StateUp State = "up"
	// StateDegraded means at least one check has failed, but the failure
	// threshold has not been reached yet.
	StateDegraded State = "degraded"
	// StateDown means consecutive failures reached the monitor's threshold.
	StateDown State = "down"
)

// States lists every valid state, in lifecycle order.
func States() []State {
	return []State{StateUnknown, StateUp, StateDegraded, StateDown}
}

// Valid reports whether s is a state the database will accept.
func (s State) Valid() bool {
	switch s {
	case StateUnknown, StateUp, StateDegraded, StateDown:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (s State) String() string { return string(s) }

// Limits applied to every configurable monitor field. They mirror the CHECK
// constraints in db/migrations/00001_create_monitors.sql — changing a bound
// here without changing the migration only moves the rejection from the
// application to the database.
const (
	// The seconds and milliseconds forms are the authoritative ones: they are
	// the units the monitors columns store, so nothing has to convert a
	// duration at a validation boundary.
	MinCheckIntervalSeconds int32 = 10
	MaxCheckIntervalSeconds int32 = 24 * 60 * 60
	MinCheckTimeoutMS       int32 = 100
	MaxCheckTimeoutMS       int32 = 60 * 1000

	MinCheckInterval = time.Duration(MinCheckIntervalSeconds) * time.Second
	MaxCheckInterval = time.Duration(MaxCheckIntervalSeconds) * time.Second

	MinCheckTimeout = time.Duration(MinCheckTimeoutMS) * time.Millisecond
	MaxCheckTimeout = time.Duration(MaxCheckTimeoutMS) * time.Millisecond

	MinFailureThreshold = 1
	MaxFailureThreshold = 10

	MinStatusCode = 100
	MaxStatusCode = 599

	MaxNameLength = 120
	MaxURLLength  = 2048
)

// Defaults applied to a monitor when the caller does not specify a value.
const (
	DefaultCheckInterval     = 60 * time.Second
	DefaultCheckTimeout      = 5 * time.Second
	DefaultExpectedStatusMin = 200
	DefaultExpectedStatusMax = 399
	DefaultFailureThreshold  = 3
)

// Monitor is a single registered endpoint and everything the scheduler needs
// to decide when to check it next. Nullable database columns are pointers so
// that "never checked" is distinguishable from a zero value.
type Monitor struct {
	ID   uuid.UUID
	Name string
	URL  string

	IntervalSeconds   int32
	TimeoutMS         int32
	ExpectedStatusMin int16
	ExpectedStatusMax int16
	FailureThreshold  int16
	Enabled           bool

	State               State
	ConsecutiveFailures int32
	LastCheckedAt       *time.Time
	LastStatusCode      *int32
	LastLatencyMS       *int32

	NextCheckAt time.Time
	LeaseOwner  *string
	LeaseUntil  *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// Interval returns the check interval as a duration.
func (m Monitor) Interval() time.Duration {
	return time.Duration(m.IntervalSeconds) * time.Second
}

// Timeout returns the per-request timeout as a duration.
func (m Monitor) Timeout() time.Duration {
	return time.Duration(m.TimeoutMS) * time.Millisecond
}

// Deleted reports whether the monitor has been soft-deleted.
func (m Monitor) Deleted() bool { return m.DeletedAt != nil }

// Leased reports whether a worker currently holds a non-expired lease on the
// monitor, as of now.
func (m Monitor) Leased(now time.Time) bool {
	return m.LeaseUntil != nil && m.LeaseUntil.After(now)
}
