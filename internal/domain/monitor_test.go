package domain_test

import (
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

func TestStateValid(t *testing.T) {
	t.Parallel()

	for _, state := range domain.States() {
		if !state.Valid() {
			t.Errorf("State(%q).Valid() = false, want true", state)
		}
	}

	for _, state := range []domain.State{"", "UP", "flapping", "unknown "} {
		if state.Valid() {
			t.Errorf("State(%q).Valid() = true, want false", state)
		}
	}
}

func TestMonitorDurations(t *testing.T) {
	t.Parallel()

	m := domain.Monitor{IntervalSeconds: 90, TimeoutMS: 2500}

	if got, want := m.Interval(), 90*time.Second; got != want {
		t.Errorf("Interval() = %v, want %v", got, want)
	}
	if got, want := m.Timeout(), 2500*time.Millisecond; got != want {
		t.Errorf("Timeout() = %v, want %v", got, want)
	}
}

func TestMonitorLeased(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)

	tests := []struct {
		name       string
		leaseUntil *time.Time
		want       bool
	}{
		{name: "no lease", leaseUntil: nil, want: false},
		{name: "lease in the future", leaseUntil: &future, want: true},
		{name: "lease expired", leaseUntil: &past, want: false},
		{name: "lease expiring exactly now", leaseUntil: &now, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := domain.Monitor{LeaseUntil: tt.leaseUntil}
			if got := m.Leased(now); got != tt.want {
				t.Errorf("Leased() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMonitorDeleted(t *testing.T) {
	t.Parallel()

	deletedAt := time.Now()

	if (domain.Monitor{}).Deleted() {
		t.Error("Deleted() = true for a monitor with no deleted_at, want false")
	}
	if !(domain.Monitor{DeletedAt: &deletedAt}).Deleted() {
		t.Error("Deleted() = false for a soft-deleted monitor, want true")
	}
}
