// Package monitor holds the monitor service layer: validation of user-supplied
// monitor definitions, and the orchestration between transport and storage.
// The state machine that turns check results into up/degraded/down transitions
// joins it in Sprint 4.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// Pagination bounds for List.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Defaults fill in the fields a caller omitted when creating a monitor. They
// come from configuration (DEFAULT_CHECK_INTERVAL, DEFAULT_CHECK_TIMEOUT) so
// that an operator can set the house style for a deployment.
type Defaults struct {
	IntervalSeconds int32
	TimeoutMS       int32
}

// Service implements the monitor use cases.
type Service struct {
	store    Store
	defaults Defaults
	policy   security.Policy
}

// NewService wires a service to its store. policy (from
// ALLOW_PRIVATE_TARGETS) is applied to the url of every create and update.
func NewService(store Store, defaults Defaults, policy security.Policy) *Service {
	return &Service{store: store, defaults: defaults, policy: policy}
}

// CreateInput is a create request. Pointer fields are optional and fall back to
// the configured defaults.
type CreateInput struct {
	Name              string
	URL               string
	IntervalSeconds   *int32
	TimeoutMS         *int32
	ExpectedStatusMin *int16
	ExpectedStatusMax *int16
	FailureThreshold  *int16
	Enabled           *bool
}

// UpdateInput is a partial update. A nil pointer means "leave unchanged"; there
// is no way to unset a field, because none of them are nullable.
type UpdateInput struct {
	Name              *string
	URL               *string
	IntervalSeconds   *int32
	TimeoutMS         *int32
	ExpectedStatusMin *int16
	ExpectedStatusMax *int16
	FailureThreshold  *int16
	Enabled           *bool
}

// IsEmpty reports whether the update would change nothing.
func (in UpdateInput) IsEmpty() bool {
	return in.Name == nil && in.URL == nil && in.IntervalSeconds == nil &&
		in.TimeoutMS == nil && in.ExpectedStatusMin == nil && in.ExpectedStatusMax == nil &&
		in.FailureThreshold == nil && in.Enabled == nil
}

// Create validates the input, applies defaults, and stores a new monitor.
func (s *Service) Create(ctx context.Context, in CreateInput) (domain.Monitor, error) {
	attrs := attributes{
		name:              in.Name,
		rawURL:            in.URL,
		intervalSeconds:   deref(in.IntervalSeconds, s.defaults.IntervalSeconds),
		timeoutMS:         deref(in.TimeoutMS, s.defaults.TimeoutMS),
		expectedStatusMin: deref(in.ExpectedStatusMin, domain.DefaultExpectedStatusMin),
		expectedStatusMax: deref(in.ExpectedStatusMax, domain.DefaultExpectedStatusMax),
		failureThreshold:  deref(in.FailureThreshold, domain.DefaultFailureThreshold),
	}

	if err := attrs.validate(s.policy); err != nil {
		return domain.Monitor{}, err
	}

	return s.store.Create(ctx, CreateParams{
		Name:              trimmed(in.Name),
		URL:               trimmed(in.URL),
		IntervalSeconds:   attrs.intervalSeconds,
		TimeoutMS:         attrs.timeoutMS,
		ExpectedStatusMin: attrs.expectedStatusMin,
		ExpectedStatusMax: attrs.expectedStatusMax,
		FailureThreshold:  attrs.failureThreshold,
		Enabled:           deref(in.Enabled, true),
	})
}

// Get returns a single monitor, or domain.ErrNotFound if it is absent or
// soft-deleted.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (domain.Monitor, error) {
	return s.store.Get(ctx, id)
}

// List returns one page of monitors along with the cursor for the next page,
// which is nil once the last page has been returned.
func (s *Service) List(ctx context.Context, limit int32, cursor *Cursor) ([]domain.Monitor, *Cursor, error) {
	limit = clampLimit(limit)

	// One extra row answers "is there another page?" without a second query.
	monitors, err := s.store.List(ctx, ListParams{Limit: limit + 1, Cursor: cursor})
	if err != nil {
		return nil, nil, err
	}

	if len(monitors) <= int(limit) {
		return monitors, nil, nil
	}

	monitors = monitors[:limit]
	last := monitors[len(monitors)-1]
	return monitors, &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}

// RequestCheck asks for a monitor to be checked as soon as a worker is free,
// by making it due now. A missing or soft-deleted monitor is domain.ErrNotFound
// and a disabled one is domain.ErrMonitorDisabled.
//
// The read and the update are not one transaction, and need not be: a monitor
// disabled in between is merely made due, and the claim skips disabled
// monitors, so no check runs.
func (s *Service) RequestCheck(ctx context.Context, id uuid.UUID) (domain.Monitor, error) {
	current, err := s.store.Get(ctx, id)
	if err != nil {
		return domain.Monitor{}, err
	}
	if !current.Enabled {
		return domain.Monitor{}, domain.ErrMonitorDisabled
	}
	return s.store.RequestCheck(ctx, id)
}

// ListChecks returns one page of a monitor's check history, newest first,
// along with the cursor for the next page, which is nil on the last page. A
// missing or soft-deleted monitor is domain.ErrNotFound, so that an empty page
// always means "no checks yet" rather than "no such monitor".
func (s *Service) ListChecks(ctx context.Context, id uuid.UUID, limit int32, cursor *CheckCursor) ([]domain.Check, *CheckCursor, error) {
	if _, err := s.store.Get(ctx, id); err != nil {
		return nil, nil, err
	}

	limit = clampLimit(limit)

	checks, err := s.store.ListChecks(ctx, ListChecksParams{MonitorID: id, Limit: limit + 1, Cursor: cursor})
	if err != nil {
		return nil, nil, err
	}

	if len(checks) <= int(limit) {
		return checks, nil, nil
	}

	checks = checks[:limit]
	last := checks[len(checks)-1]
	return checks, &CheckCursor{CheckedAt: last.CheckedAt, ID: last.ID}, nil
}

// ListIncidents returns one page of a monitor's incident history, newest
// first, along with the cursor for the next page, which is nil on the last
// page. As with ListChecks, a missing or soft-deleted monitor is
// domain.ErrNotFound, so an empty page always means "never down".
func (s *Service) ListIncidents(ctx context.Context, id uuid.UUID, limit int32, cursor *IncidentCursor) ([]domain.Incident, *IncidentCursor, error) {
	if _, err := s.store.Get(ctx, id); err != nil {
		return nil, nil, err
	}

	limit = clampLimit(limit)

	incidents, err := s.store.ListIncidents(ctx, ListIncidentsParams{MonitorID: id, Limit: limit + 1, Cursor: cursor})
	if err != nil {
		return nil, nil, err
	}

	if len(incidents) <= int(limit) {
		return incidents, nil, nil
	}

	incidents = incidents[:limit]
	last := incidents[len(incidents)-1]
	return incidents, &IncidentCursor{StartedAt: last.StartedAt, ID: last.ID}, nil
}

// Stats summarises a monitor's availability and latency over one window. A
// missing or soft-deleted monitor is domain.ErrNotFound, so a window with no
// checks always means "not checked yet" rather than "no such monitor".
func (s *Service) Stats(ctx context.Context, id uuid.UUID, window domain.StatsWindow) (domain.Stats, error) {
	if _, err := s.store.Get(ctx, id); err != nil {
		return domain.Stats{}, err
	}

	raw, err := s.store.GetStats(ctx, id, window)
	if err != nil {
		return domain.Stats{}, err
	}
	return domain.NewStats(window, raw), nil
}

// Update merges the requested changes over the stored monitor and validates the
// result. Reading first is what lets a partial update be checked against the
// cross-field rules: shortening an interval can invalidate a timeout that was
// perfectly legal before.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (domain.Monitor, error) {
	current, err := s.store.Get(ctx, id)
	if err != nil {
		return domain.Monitor{}, err
	}

	attrs := attributes{
		name:              deref(in.Name, current.Name),
		rawURL:            deref(in.URL, current.URL),
		intervalSeconds:   deref(in.IntervalSeconds, current.IntervalSeconds),
		timeoutMS:         deref(in.TimeoutMS, current.TimeoutMS),
		expectedStatusMin: deref(in.ExpectedStatusMin, current.ExpectedStatusMin),
		expectedStatusMax: deref(in.ExpectedStatusMax, current.ExpectedStatusMax),
		failureThreshold:  deref(in.FailureThreshold, current.FailureThreshold),
		// Only a URL the request sets has to pass the SSRF policy; a stored
		// one that predates it must not stop the monitor being disabled.
		skipPolicy: in.URL == nil,
	}

	if err := attrs.validate(s.policy); err != nil {
		return domain.Monitor{}, err
	}

	params := UpdateParams{
		ID:                id,
		IntervalSeconds:   in.IntervalSeconds,
		TimeoutMS:         in.TimeoutMS,
		ExpectedStatusMin: in.ExpectedStatusMin,
		ExpectedStatusMax: in.ExpectedStatusMax,
		FailureThreshold:  in.FailureThreshold,
		Enabled:           in.Enabled,
	}
	// Strings are stored trimmed, so re-derive them from the validated values
	// rather than passing the raw request through.
	if in.Name != nil {
		name := trimmed(*in.Name)
		params.Name = &name
	}
	if in.URL != nil {
		target := trimmed(*in.URL)
		params.URL = &target
	}

	return s.store.Update(ctx, params)
}

// Delete soft-deletes a monitor. Deleting an already-deleted monitor returns
// domain.ErrNotFound: from the caller's point of view it was already gone.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.store.SoftDelete(ctx, id)
}

// DefaultsFromDurations converts configured durations into the units the
// monitors table stores, rejecting values the columns could not hold. The
// configuration package already enforces the same bounds; this is what keeps
// the service usable from anywhere else without that guarantee.
func DefaultsFromDurations(interval, timeout time.Duration) (Defaults, error) {
	if interval < domain.MinCheckInterval || interval > domain.MaxCheckInterval {
		return Defaults{}, fmt.Errorf("default check interval out of range: %s", interval)
	}
	if timeout < domain.MinCheckTimeout || timeout > domain.MaxCheckTimeout {
		return Defaults{}, fmt.Errorf("default check timeout out of range: %s", timeout)
	}

	// The range checks above bound both values well inside int32.
	return Defaults{
		IntervalSeconds: int32(interval / time.Second),     // #nosec G115 -- bounded above
		TimeoutMS:       int32(timeout / time.Millisecond), // #nosec G115 -- bounded above
	}, nil
}

// clampLimit bounds a requested page size to [1, MaxPageSize], treating a
// non-positive value as a request for the default.
func clampLimit(limit int32) int32 {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	default:
		return limit
	}
}

func deref[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

func trimmed(s string) string {
	return strings.TrimSpace(s)
}
