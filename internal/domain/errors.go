package domain

import "errors"

// ErrNotFound is returned when a record does not exist, or exists but has been
// soft-deleted. The two cases are deliberately indistinguishable to callers:
// a deleted monitor is gone as far as the API is concerned.
var ErrNotFound = errors.New("not found")

// ErrMonitorDisabled is returned when an operation requires an enabled
// monitor, such as requesting a manual check, but the monitor is disabled.
var ErrMonitorDisabled = errors.New("monitor disabled")
