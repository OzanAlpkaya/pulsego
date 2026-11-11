package scheduler

import "time"

// SetRetentionTicks replaces the runner's RetentionInterval ticker with ticks,
// so tests decide when each run after the first one happens. It must be
// called before Run.
func (r *Retention) SetRetentionTicks(ticks <-chan time.Time) {
	r.newTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
}
