package scheduler_test

import (
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
)

// TestQARetentionScheduleConstants (Sprint 4 QA, D8/D9): retention runs hourly
// and deletes in batches of 5000. Both are code constants, not configuration;
// a silent change would alter the documented behaviour.
func TestQARetentionScheduleConstants(t *testing.T) {
	if scheduler.RetentionInterval != time.Hour {
		t.Errorf("RetentionInterval = %v, want 1h", scheduler.RetentionInterval)
	}
	if scheduler.RetentionBatchSize != 5000 {
		t.Errorf("RetentionBatchSize = %d, want 5000", scheduler.RetentionBatchSize)
	}
}
