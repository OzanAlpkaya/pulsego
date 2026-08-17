package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

// fakeStore records what the service asked for and returns canned results, so
// the service's own behaviour can be tested without a database.
type fakeStore struct {
	monitors map[uuid.UUID]domain.Monitor
	page     []domain.Monitor

	createParams *monitor.CreateParams
	updateParams *monitor.UpdateParams
	listParams   *monitor.ListParams
	deletedID    *uuid.UUID

	err error
}

func newFakeStore() *fakeStore {
	return &fakeStore{monitors: make(map[uuid.UUID]domain.Monitor)}
}

func (f *fakeStore) Create(_ context.Context, p monitor.CreateParams) (domain.Monitor, error) {
	f.createParams = &p
	if f.err != nil {
		return domain.Monitor{}, f.err
	}
	return domain.Monitor{
		ID: uuid.New(), Name: p.Name, URL: p.URL,
		IntervalSeconds: p.IntervalSeconds, TimeoutMS: p.TimeoutMS,
		ExpectedStatusMin: p.ExpectedStatusMin, ExpectedStatusMax: p.ExpectedStatusMax,
		FailureThreshold: p.FailureThreshold, Enabled: p.Enabled,
		State: domain.StateUnknown,
	}, nil
}

func (f *fakeStore) Get(_ context.Context, id uuid.UUID) (domain.Monitor, error) {
	if f.err != nil {
		return domain.Monitor{}, f.err
	}
	m, ok := f.monitors[id]
	if !ok {
		return domain.Monitor{}, domain.ErrNotFound
	}
	return m, nil
}

func (f *fakeStore) List(_ context.Context, p monitor.ListParams) ([]domain.Monitor, error) {
	f.listParams = &p
	if f.err != nil {
		return nil, f.err
	}
	if int(p.Limit) < len(f.page) {
		return f.page[:p.Limit], nil
	}
	return f.page, nil
}

func (f *fakeStore) Update(_ context.Context, p monitor.UpdateParams) (domain.Monitor, error) {
	f.updateParams = &p
	if f.err != nil {
		return domain.Monitor{}, f.err
	}
	return f.monitors[p.ID], nil
}

func (f *fakeStore) SoftDelete(_ context.Context, id uuid.UUID) error {
	f.deletedID = &id
	return f.err
}

func testDefaults() monitor.Defaults {
	return monitor.Defaults{IntervalSeconds: 60, TimeoutMS: 5000}
}

func ptr[T any](v T) *T { return &v }

func TestCreateAppliesDefaults(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := monitor.NewService(store, testDefaults())

	_, err := svc.Create(context.Background(), monitor.CreateInput{
		Name: "  Example  ",
		URL:  "  https://example.com  ",
	})
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	got := store.createParams
	if got == nil {
		t.Fatal("Create() did not reach the store")
	}

	if got.Name != "Example" {
		t.Errorf("Name = %q, want the trimmed value %q", got.Name, "Example")
	}
	if got.URL != "https://example.com" {
		t.Errorf("URL = %q, want the trimmed value", got.URL)
	}
	if got.IntervalSeconds != 60 {
		t.Errorf("IntervalSeconds = %d, want the configured default 60", got.IntervalSeconds)
	}
	if got.TimeoutMS != 5000 {
		t.Errorf("TimeoutMS = %d, want the configured default 5000", got.TimeoutMS)
	}
	if got.ExpectedStatusMin != 200 || got.ExpectedStatusMax != 399 {
		t.Errorf("expected status range = %d-%d, want 200-399", got.ExpectedStatusMin, got.ExpectedStatusMax)
	}
	if got.FailureThreshold != 3 {
		t.Errorf("FailureThreshold = %d, want 3", got.FailureThreshold)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want monitors to be enabled by default")
	}
}

func TestCreateHonoursExplicitValues(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := monitor.NewService(store, testDefaults())

	_, err := svc.Create(context.Background(), monitor.CreateInput{
		Name:              "Example",
		URL:               "https://example.com",
		IntervalSeconds:   ptr(int32(120)),
		TimeoutMS:         ptr(int32(2500)),
		ExpectedStatusMin: ptr(int16(200)),
		ExpectedStatusMax: ptr(int16(204)),
		FailureThreshold:  ptr(int16(5)),
		Enabled:           ptr(false),
	})
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	got := store.createParams
	if got.IntervalSeconds != 120 || got.TimeoutMS != 2500 {
		t.Errorf("interval/timeout = %d/%d, want 120/2500", got.IntervalSeconds, got.TimeoutMS)
	}
	if got.ExpectedStatusMax != 204 || got.FailureThreshold != 5 {
		t.Errorf("status max/threshold = %d/%d, want 204/5", got.ExpectedStatusMax, got.FailureThreshold)
	}
	if got.Enabled {
		t.Error("Enabled = true, want the explicit false to be honoured")
	}
}

func TestCreateRejectsInvalidWithoutTouchingStore(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := monitor.NewService(store, testDefaults())

	_, err := svc.Create(context.Background(), monitor.CreateInput{Name: "", URL: "nonsense"})

	var verr *monitor.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Create() returned %v, want a *ValidationError", err)
	}
	if store.createParams != nil {
		t.Error("Create() reached the store despite failing validation")
	}
}

// TestUpdateValidatesAgainstMergedValues is the reason Update reads before it
// writes: a timeout that was legal at a 60s interval stops being legal when the
// interval is shortened to 10s in the same request.
func TestUpdateValidatesAgainstMergedValues(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	store := newFakeStore()
	store.monitors[id] = domain.Monitor{
		ID: id, Name: "Example", URL: "https://example.com",
		IntervalSeconds: 60, TimeoutMS: 30000,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 3,
	}
	svc := monitor.NewService(store, testDefaults())

	_, err := svc.Update(context.Background(), id, monitor.UpdateInput{
		IntervalSeconds: ptr(int32(10)),
	})

	var verr *monitor.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Update() returned %v, want a *ValidationError", err)
	}
	if store.updateParams != nil {
		t.Error("Update() reached the store despite failing validation")
	}
}

func TestUpdatePassesOnlyRequestedFields(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	store := newFakeStore()
	store.monitors[id] = domain.Monitor{
		ID: id, Name: "Example", URL: "https://example.com",
		IntervalSeconds: 60, TimeoutMS: 5000,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 3,
	}
	svc := monitor.NewService(store, testDefaults())

	if _, err := svc.Update(context.Background(), id, monitor.UpdateInput{
		Name: ptr("  Renamed  "),
	}); err != nil {
		t.Fatalf("Update() returned an unexpected error: %v", err)
	}

	got := store.updateParams
	if got == nil {
		t.Fatal("Update() did not reach the store")
	}
	if got.Name == nil || *got.Name != "Renamed" {
		t.Errorf("Name = %v, want the trimmed %q", got.Name, "Renamed")
	}
	for name, field := range map[string]any{
		"URL":               got.URL,
		"IntervalSeconds":   got.IntervalSeconds,
		"TimeoutMS":         got.TimeoutMS,
		"ExpectedStatusMin": got.ExpectedStatusMin,
		"FailureThreshold":  got.FailureThreshold,
		"Enabled":           got.Enabled,
	} {
		if !isNilPointer(field) {
			t.Errorf("%s was sent to the store despite not being requested", name)
		}
	}
}

func TestUpdatePropagatesNotFound(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := monitor.NewService(store, testDefaults())

	_, err := svc.Update(context.Background(), uuid.New(), monitor.UpdateInput{Name: ptr("x")})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Update() returned %v, want domain.ErrNotFound", err)
	}
}

func TestUpdateInputIsEmpty(t *testing.T) {
	t.Parallel()

	if !(monitor.UpdateInput{}).IsEmpty() {
		t.Error("IsEmpty() = false for a zero UpdateInput, want true")
	}
	if (monitor.UpdateInput{Enabled: ptr(false)}).IsEmpty() {
		t.Error("IsEmpty() = true when Enabled was set to false, want false")
	}
}

func TestListPagination(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	makePage := func(n int) []domain.Monitor {
		page := make([]domain.Monitor, 0, n)
		for i := range n {
			page = append(page, domain.Monitor{ID: uuid.New(), CreatedAt: base.Add(-time.Duration(i) * time.Minute)})
		}
		return page
	}

	t.Run("full page yields a cursor", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		store.page = makePage(4) // limit+1 rows available
		svc := monitor.NewService(store, testDefaults())

		got, next, err := svc.List(context.Background(), 3, nil)
		if err != nil {
			t.Fatalf("List() returned an unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("returned %d monitors, want the requested 3", len(got))
		}
		if next == nil {
			t.Fatal("next cursor is nil, want a cursor because more rows exist")
		}
		if next.ID != got[2].ID {
			t.Error("cursor does not point at the last returned row")
		}
		if store.listParams.Limit != 4 {
			t.Errorf("store limit = %d, want limit+1 so the extra row can reveal the next page",
				store.listParams.Limit)
		}
	})

	t.Run("partial page ends pagination", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		store.page = makePage(2)
		svc := monitor.NewService(store, testDefaults())

		got, next, err := svc.List(context.Background(), 3, nil)
		if err != nil {
			t.Fatalf("List() returned an unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("returned %d monitors, want 2", len(got))
		}
		if next != nil {
			t.Error("next cursor is set on the last page, want nil")
		}
	})

	t.Run("limit is clamped", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct{ requested, want int32 }{
			{requested: 0, want: monitor.DefaultPageSize},
			{requested: -5, want: monitor.DefaultPageSize},
			{requested: 1000, want: monitor.MaxPageSize},
		} {
			store := newFakeStore()
			svc := monitor.NewService(store, testDefaults())

			if _, _, err := svc.List(context.Background(), tc.requested, nil); err != nil {
				t.Fatalf("List() returned an unexpected error: %v", err)
			}
			if got := store.listParams.Limit - 1; got != tc.want {
				t.Errorf("limit %d became %d, want %d", tc.requested, got, tc.want)
			}
		}
	})
}

func TestDeleteDelegates(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	store := newFakeStore()
	svc := monitor.NewService(store, testDefaults())

	if err := svc.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete() returned an unexpected error: %v", err)
	}
	if store.deletedID == nil || *store.deletedID != id {
		t.Errorf("store received %v, want %v", store.deletedID, id)
	}
}

func TestDefaultsFromDurations(t *testing.T) {
	t.Parallel()

	got, err := monitor.DefaultsFromDurations(60*time.Second, 5000*time.Millisecond)
	if err != nil {
		t.Fatalf("DefaultsFromDurations() returned an unexpected error: %v", err)
	}
	if got.IntervalSeconds != 60 || got.TimeoutMS != 5000 {
		t.Errorf("got %+v, want {60 5000}", got)
	}

	for _, tc := range []struct {
		name              string
		interval, timeout time.Duration
	}{
		{name: "zero interval", interval: 0, timeout: 5 * time.Second},
		{name: "interval below minimum", interval: time.Second, timeout: 5 * time.Second},
		{name: "interval beyond maximum", interval: 25 * time.Hour, timeout: 5 * time.Second},
		{name: "zero timeout", interval: time.Minute, timeout: 0},
		{name: "timeout beyond maximum", interval: time.Minute, timeout: 90 * time.Second},
	} {
		if _, err := monitor.DefaultsFromDurations(tc.interval, tc.timeout); err == nil {
			t.Errorf("%s: DefaultsFromDurations() succeeded, want an error", tc.name)
		}
	}
}

// isNilPointer reports whether an any holding a typed pointer is nil, which a
// plain `field == nil` comparison would miss.
func isNilPointer(v any) bool {
	switch p := v.(type) {
	case *string:
		return p == nil
	case *int32:
		return p == nil
	case *int16:
		return p == nil
	case *bool:
		return p == nil
	default:
		return false
	}
}
