package scheduler

import "errors"

// ErrLeaseLost reports that a worker no longer holds the lease on a monitor it
// claimed: the lease expired and another worker reclaimed the monitor, or the
// monitor was soft-deleted mid-check. A result recorded under a lost lease is
// discarded, which is what keeps a late worker from writing a duplicate.
var ErrLeaseLost = errors.New("scheduler: lease lost")
