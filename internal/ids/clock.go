package ids

import "time"

// Clock is the store's only source of the current time, so a test can pin it.
// Spec 0002 forbids CURRENT_TIMESTAMP: every timestamp comes through here.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the real wall clock in UTC.
type SystemClock struct{}

// Now returns the current time in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock returns a time that only advances when a test advances it.
type FixedClock struct {
	T time.Time
}

// Now returns the pinned time in UTC.
func (c *FixedClock) Now() time.Time { return c.T.UTC() }

// Advance moves the pinned time forward by d.
func (c *FixedClock) Advance(d time.Duration) { c.T = c.T.Add(d) }

// Stamp formats t the way every timestamp column in the database stores it:
// RFC3339 with nanoseconds, in UTC, so text ordering matches time ordering.
func Stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
