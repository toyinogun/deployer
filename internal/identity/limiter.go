package identity

import (
	"sync"
	"time"
)

// Settings are the five numbers a limiter runs on. They were package constants
// until spec 0022, which put a second limiter on the deploy path: the sign in
// limiter keeps exactly the values below and the deploy path gets its own, so a
// burst of uploads can never spend a person's sign in budget or lock them out of
// the console (AC-15).
type Settings struct {
	// BucketCapacity is how many calls a client may make at once.
	BucketCapacity float64
	// BucketRefill is how often one token comes back.
	BucketRefill time.Duration
	// FailuresBeforeLockout is how many bad credentials an entry gets for free.
	FailuresBeforeLockout int
	// LockoutBase is the first penalty, doubling with each further failure.
	LockoutBase time.Duration
	// LockoutCeiling caps the doubling, so nothing is ever locked out for good.
	LockoutCeiling time.Duration
}

// SignInSettings is what the browser and JSON sign in surfaces run on. These are
// the values that were package constants before spec 0022 and they are unchanged
// by it, so that refactor moved no sign in behaviour.
func SignInSettings() Settings {
	return Settings{
		BucketCapacity:        10,
		BucketRefill:          6 * time.Second,
		FailuresBeforeLockout: 5,
		LockoutBase:           30 * time.Second,
		LockoutCeiling:        15 * time.Minute,
	}
}

// DeployPathSettings is what the upload endpoint and the MCP endpoint run on.
//
// The bucket is wider and refills faster than the sign in one because the shape
// of the traffic is different: an agent polls deployment_status through a build
// that runs for minutes, and tripping a limit there would break an ordinary
// deploy. Thirty at once and one back every two seconds leaves that comfortable
// while still bounding a flood at thirty a minute sustained.
//
// The lockout numbers match the sign in ones, because the shape of that attack
// is the same whether the credential being guessed is a password or a token
// (spec 0022, Value sourcing).
func DeployPathSettings() Settings {
	return Settings{
		BucketCapacity:        30,
		BucketRefill:          2 * time.Second,
		FailuresBeforeLockout: 5,
		LockoutBase:           30 * time.Second,
		LockoutCeiling:        15 * time.Minute,
	}
}

// idleEviction is how long an untouched entry is kept before the next sweep
// drops it, so a long running pod does not grow a map per address it ever saw.
const idleEviction = time.Hour

// bucket is one client's allowance.
type bucket struct {
	tokens float64
	seen   time.Time
}

// attempts is one key's run of bad credentials.
type attempts struct {
	failures int
	until    time.Time
	seen     time.Time
}

// Limiter holds both limits: a token bucket per client address, and a doubling
// lockout per key. Both are in process, both are safe for concurrent use, and
// both are lost on restart.
//
// The restart bound is real and is recorded rather than hidden. ArgoCD restarts
// the pod on each sync, so a run of failures and a spent bucket both vanish
// whenever the platform is synced. Against a 256 bit random token that is not a
// feasible brute force window, so the job these do is slowing a script down and
// bounding what a flood costs, not stopping a distributed attack. The perimeter
// stopped being a tailnet when spec 0021 published the console and spec 0022
// published the deploy path; this is what holds instead, and it is deliberately
// soft (spec 0022, AC-23).
type Limiter struct {
	clock    Clock
	settings Settings

	mu       sync.Mutex
	buckets  map[string]*bucket
	failures map[string]*attempts
	swept    time.Time
}

// NewLimiter returns an empty limiter reading time from clock and running on s.
func NewLimiter(c Clock, s Settings) *Limiter {
	return &Limiter{
		clock:    c,
		settings: s,
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
		b = &bucket{tokens: l.settings.BucketCapacity}
		l.buckets[client] = b
	} else {
		refilled := now.Sub(b.seen).Seconds() / l.settings.BucketRefill.Seconds()
		b.tokens = min(l.settings.BucketCapacity, b.tokens+refilled)
	}
	b.seen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// LockedOut reports whether key is inside its penalty window, and how long is
// left.
//
// What key means depends on which limiter this is, and the two are not the same
// thing. On the sign in limiter it is the email address being guessed at, which
// is what stops one person's typo spree locking out everyone behind a shared
// network address (spec 0021, AC-23). On the deploy path limiter it is the
// visitor's network address, because a bearer token names no account until it
// resolves and there is nothing else to key on (spec 0022, AC-16). Read every
// mention of "address" in this file against the caller before trusting it.
func (l *Limiter) LockedOut(key string) (time.Duration, bool) {
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	a, ok := l.failures[key]
	if !ok || !now.Before(a.until) {
		return 0, false
	}
	return a.until.Sub(now), true
}

// Failed records one bad credential against key, extending the penalty window
// once the free attempts are spent. The delay starts at the settings' base and
// doubles with each further failure, to their ceiling.
func (l *Limiter) Failed(key string) {
	now := l.clock.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	a, ok := l.failures[key]
	if !ok {
		a = &attempts{}
		l.failures[key] = a
	}
	a.failures++
	a.seen = now
	if a.failures < l.settings.FailuresBeforeLockout {
		return
	}
	delay := l.settings.LockoutBase << (a.failures - l.settings.FailuresBeforeLockout)
	if delay > l.settings.LockoutCeiling || delay <= 0 {
		delay = l.settings.LockoutCeiling
	}
	a.until = now.Add(delay)
}

// Succeeded clears key's run, so one good credential undoes the backoff.
func (l *Limiter) Succeeded(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
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
