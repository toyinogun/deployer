package identity

import (
	"sync"
	"time"
)

// The rate limiting shape, entire. It lives in memory and is lost on restart,
// which is proportionate: the perimeter is a tailnet, so this exists to slow a
// script down rather than to stop a distributed attack.
const (
	// bucketCapacity is how many unauthenticated calls a client may make at once.
	bucketCapacity = 10
	// bucketRefill is how often one token comes back.
	bucketRefill = 6 * time.Second

	// failuresBeforeLockout is how many wrong passwords an address gets for free.
	failuresBeforeLockout = 5
	// lockoutBase is the first penalty, doubling with each further failure.
	lockoutBase = 30 * time.Second
	// lockoutCeiling caps the doubling, so an address is never locked out for good.
	lockoutCeiling = 15 * time.Minute

	// idleEviction is how long an untouched entry is kept before the next sweep
	// drops it, so a long running pod does not grow a map per address it ever saw.
	idleEviction = time.Hour
)

// bucket is one client's allowance.
type bucket struct {
	tokens float64
	seen   time.Time
}

// attempts is one address's failed sign in run.
type attempts struct {
	failures int
	until    time.Time
	seen     time.Time
}

// Limiter holds both limits: a token bucket per client address, and a doubling
// lockout per email address. Both are in process, both are lost on restart, and
// both are safe for concurrent use.
type Limiter struct {
	clock Clock

	mu       sync.Mutex
	buckets  map[string]*bucket
	failures map[string]*attempts
	swept    time.Time
}

// NewLimiter returns an empty limiter reading time from clock.
func NewLimiter(c Clock) *Limiter {
	return &Limiter{
		clock:    c,
		buckets:  map[string]*bucket{},
		failures: map[string]*attempts{},
	}
}

// Allow spends one token for a client address, reporting whether there was one.
// An empty key is always allowed: a caller the platform cannot tell apart from
// another is not a bucket worth keeping (AC-24).
func (l *Limiter) Allow(client string) bool {
	if client == "" {
		return true
	}
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	b, ok := l.buckets[client]
	if !ok {
		b = &bucket{tokens: bucketCapacity}
		l.buckets[client] = b
	} else {
		refilled := now.Sub(b.seen).Seconds() / bucketRefill.Seconds()
		b.tokens = min(bucketCapacity, b.tokens+refilled)
	}
	b.seen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// LockedOut reports whether an address is inside its penalty window, and how long
// is left. The address is the key rather than the client, so one person's typo
// spree cannot lock out the whole tailnet (AC-23).
func (l *Limiter) LockedOut(email string) (time.Duration, bool) {
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	a, ok := l.failures[email]
	if !ok || !now.Before(a.until) {
		return 0, false
	}
	return a.until.Sub(now), true
}

// Failed records one wrong sign in for an address, extending the penalty window
// once the free attempts are spent. The delay starts at 30 seconds and doubles
// with each further failure, to a ceiling of 15 minutes.
func (l *Limiter) Failed(email string) {
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	a, ok := l.failures[email]
	if !ok {
		a = &attempts{}
		l.failures[email] = a
	}
	a.failures++
	a.seen = now
	if a.failures < failuresBeforeLockout {
		return
	}
	delay := lockoutBase << (a.failures - failuresBeforeLockout)
	if delay > lockoutCeiling || delay <= 0 {
		delay = lockoutCeiling
	}
	a.until = now.Add(delay)
}

// Succeeded clears an address's run, so one good sign in undoes the backoff.
func (l *Limiter) Succeeded(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, email)
}

// sweep drops entries nobody has touched in an hour. It runs at most once an
// hour and the caller already holds the lock, so it costs nothing on a hot path.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < idleEviction {
		return
	}
	l.swept = now
	for k, b := range l.buckets {
		if now.Sub(b.seen) > idleEviction {
			delete(l.buckets, k)
		}
	}
	for k, a := range l.failures {
		if now.Sub(a.seen) > idleEviction && !now.Before(a.until) {
			delete(l.failures, k)
		}
	}
}
