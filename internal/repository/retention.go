package repository

import (
	"context"

	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
)

// RetentionStore deletes expired check history for scheduler.Retention.
type RetentionStore struct {
	q *sqlcgen.Queries
}

// NewRetentionStore builds a store over a connection pool (a *pgxpool.Pool in
// production).
func NewRetentionStore(db sqlcgen.DBTX) *RetentionStore {
	return &RetentionStore{q: sqlcgen.New(db)}
}

// DeleteExpiredChecks deletes up to batchSize monitor_checks rows whose
// checked_at is more than retentionDays before the database's now(), oldest
// first, and returns how many it deleted. Rows locked by a concurrent cleaner
// are skipped, not waited on. Incidents that reference a deleted check keep
// their row; the foreign key sets the reference to NULL.
func (s *RetentionStore) DeleteExpiredChecks(ctx context.Context, retentionDays, batchSize int32) (int64, error) {
	n, err := s.q.DeleteExpiredChecks(ctx, sqlcgen.DeleteExpiredChecksParams{
		RetentionDays: retentionDays,
		BatchSize:     batchSize,
	})
	if err != nil {
		return 0, translateError(err, "delete expired checks")
	}
	return n, nil
}
