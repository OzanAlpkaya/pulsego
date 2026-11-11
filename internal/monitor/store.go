package monitor

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// Store is the persistence this package needs. It is declared here, in the
// consuming package, so that internal/repository depends on the service rather
// than the other way round.
type Store interface {
	Create(ctx context.Context, p CreateParams) (domain.Monitor, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Monitor, error)
	List(ctx context.Context, p ListParams) ([]domain.Monitor, error)
	Update(ctx context.Context, p UpdateParams) (domain.Monitor, error)
	SoftDelete(ctx context.Context, id uuid.UUID) error

	// RequestCheck makes a monitor due now, returning domain.ErrNotFound if
	// it is absent or soft-deleted.
	RequestCheck(ctx context.Context, id uuid.UUID) (domain.Monitor, error)
	// ListChecks returns one page of a monitor's check history, newest first.
	ListChecks(ctx context.Context, p ListChecksParams) ([]domain.Check, error)
}

// CreateParams holds fully resolved values: defaults are applied and validation
// has passed before a Store ever sees them.
type CreateParams struct {
	Name              string
	URL               string
	IntervalSeconds   int32
	TimeoutMS         int32
	ExpectedStatusMin int16
	ExpectedStatusMax int16
	FailureThreshold  int16
	Enabled           bool
}

// UpdateParams carries only the fields the caller asked to change; a nil
// pointer leaves the stored value untouched.
type UpdateParams struct {
	ID                uuid.UUID
	Name              *string
	URL               *string
	IntervalSeconds   *int32
	TimeoutMS         *int32
	ExpectedStatusMin *int16
	ExpectedStatusMax *int16
	FailureThreshold  *int16
	Enabled           *bool
}

// ListParams describes one page. Cursor is nil for the first page.
type ListParams struct {
	Limit  int32
	Cursor *Cursor
}

// Cursor is the keyset position of the last row of a page. It is a typed value
// here; turning it into an opaque string is the transport layer's job.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListChecksParams describes one page of a monitor's check history. Cursor is
// nil for the first page.
type ListChecksParams struct {
	MonitorID uuid.UUID
	Limit     int32
	Cursor    *CheckCursor
}

// CheckCursor is the keyset position of the last check of a page. Check ids
// are database identities, so they are int64 rather than UUIDs.
type CheckCursor struct {
	CheckedAt time.Time
	ID        int64
}
