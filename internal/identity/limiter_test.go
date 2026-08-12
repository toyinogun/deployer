package identity_test

import (
	"sync"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
)

// dial is a clock a test moves by hand, so the backoff rules are proved in
// microseconds rather than waited out in minutes.
type dial struct {
	mu  sync.Mutex
	now time.Time
}

func newDial() *dial {
	return &dial{now: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
}

func (d *dial) Now() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.now
}

func (d *dial) advance(by time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = d.now.Add(by)
}

// TestLockoutStartsAtThirtySecondsAndDoubles is AC-23 past the first window.
// The HTTP suite proves the fifth failure locks the address out; this proves
// what the sixth and seventh cost, which is the part a caller cannot time.
//
// covers: AC-23
func TestLockoutStartsAtThirtySecondsAndDoubles(t *testing.T) {
	const email = "a@example.com"
	clock := newDial()
	l := identity.NewLimiter(clock)

	for i := range 4 {
		l.Failed(email)
		if _, locked := l.LockedOut(email); locked {
			t.Fatalf("failure %d locked the address out, want the first four free", i+1)
		}
	}

	l.Failed(email)
	left, locked := l.LockedOut(email)
	if !locked {
		t.Fatal("the fifth failure did not lock the address out")
	}
	if left != 30*time.Second {
		t.Errorf("the first penalty is %s, want 30s", left)
	}

	// Waiting the window out clears it, and the next failure costs double.
	clock.advance(30 * time.Second)
	if _, locked := l.LockedOut(email); locked {
		t.Fatal("the address is still locked out after its window passed")
	}
	l.Failed(email)
	left, locked = l.LockedOut(email)
	if !locked {
		t.Fatal("the sixth failure did not lock the address out")
	}
	if left != time.Minute {
		t.Errorf("the second penalty is %s, want it doubled to 1m", left)
	}

	clock.advance(time.Minute)
	l.Failed(email)
	if left, _ := l.LockedOut(email); left != 2*time.Minute {
		t.Errorf("the third penalty is %s, want it doubled to 2m", left)
	}
}

// TestLockoutIsCappedSoAnAddressIsNeverLostForGood proves the doubling stops at
// the ceiling rather than shifting into a negative or absurd delay. Without the
// cap a determined script permanently locks somebody out of their own account.
//
// covers: AC-23
func TestLockoutIsCappedSoAnAddressIsNeverLostForGood(t *testing.T) {
	const email = "a@example.com"
	clock := newDial()
	l := identity.NewLimiter(clock)

	// Far past the point where 30s doubled would overflow a duration.
	for range 80 {
		l.Failed(email)
	}
	left, locked := l.LockedOut(email)
	if !locked {
		t.Fatal("a long failure run left the address unlocked")
	}
	if left > 15*time.Minute {
		t.Errorf("the penalty is %s, want it capped at 15m", left)
	}
	if left <= 0 {
		t.Errorf("the penalty is %s, so the doubling wrapped and the lockout is inert", left)
	}
}

// TestOneGoodSignInClearsTheRun is the second half of AC-23: a person who
// mistypes four times and then gets it right starts from nothing again.
//
// covers: AC-23
func TestOneGoodSignInClearsTheRun(t *testing.T) {
	const email = "a@example.com"
	l := identity.NewLimiter(newDial())

	for range 4 {
		l.Failed(email)
	}
	l.Succeeded(email)

	for i := range 4 {
		l.Failed(email)
		if _, locked := l.LockedOut(email); locked {
			t.Fatalf("failure %d after a good sign in locked the address out, so the run was not cleared", i+1)
		}
	}
}

// TestLockoutIsPerAddress proves one person's typo spree cannot lock a second
// person out, which is why the key is the address and not the client.
//
// covers: AC-23
func TestLockoutIsPerAddress(t *testing.T) {
	l := identity.NewLimiter(newDial())
	for range 6 {
		l.Failed("a@example.com")
	}
	if _, locked := l.LockedOut("a@example.com"); !locked {
		t.Fatal("the address that failed is not locked out")
	}
	if _, locked := l.LockedOut("b@example.com"); locked {
		t.Error("an address that never failed is locked out")
	}
}

// TestBucketSpendsTenThenRefillsOneEverySixSeconds is AC-24.
//
// covers: AC-24
func TestBucketSpendsTenThenRefillsOneEverySixSeconds(t *testing.T) {
	const client = "100.64.0.5"
	clock := newDial()
	l := identity.NewLimiter(clock)

	for i := range 10 {
		if !l.Allow(client) {
			t.Fatalf("call %d was refused, want ten before the bucket empties", i+1)
		}
	}
	if l.Allow(client) {
		t.Fatal("the eleventh call was allowed, want it refused")
	}

	clock.advance(6 * time.Second)
	if !l.Allow(client) {
		t.Error("one token did not come back after six seconds")
	}
	if l.Allow(client) {
		t.Error("a second token came back from a single refill period")
	}
}

// TestBucketRefillIsCappedAtCapacity proves an idle client cannot bank an
// unbounded allowance and then spend it in one burst.
//
// covers: AC-24
func TestBucketRefillIsCappedAtCapacity(t *testing.T) {
	const client = "100.64.0.5"
	clock := newDial()
	l := identity.NewLimiter(clock)

	if !l.Allow(client) {
		t.Fatal("the first call was refused")
	}
	clock.advance(24 * time.Hour)

	for i := range 10 {
		if !l.Allow(client) {
			t.Fatalf("call %d after a long idle was refused, want a full bucket", i+1)
		}
	}
	if l.Allow(client) {
		t.Error("a day idle banked more than the bucket holds")
	}
}

// TestBucketsAreSeparatePerClient is the claim AC-24 can only be proved on a
// direct call, because through the ingress every caller resolves to the real
// last hop.
//
// covers: AC-24
func TestBucketsAreSeparatePerClient(t *testing.T) {
	l := identity.NewLimiter(newDial())
	for range 10 {
		l.Allow("100.64.0.5")
	}
	if l.Allow("100.64.0.5") {
		t.Fatal("the spent client is still being served")
	}
	if !l.Allow("100.64.0.9") {
		t.Error("a different client address was refused out of somebody else's bucket")
	}
}

// TestAnUnidentifiableClientIsAlwaysAllowed is the failure mode the AC-24 value
// sourcing row exists to catch. Callers the platform cannot tell apart must not
// collapse into one shared bucket, because then a single script starves every
// other caller behind the same blank key.
//
// covers: AC-24
func TestAnUnidentifiableClientIsAlwaysAllowed(t *testing.T) {
	l := identity.NewLimiter(newDial())
	for i := range 50 {
		if !l.Allow("") {
			t.Fatalf("call %d with no client address was refused, so blank keys share one bucket", i+1)
		}
	}
}

// TestSweepDropsIdleEntriesRatherThanGrowingForever proves the map does not grow
// once per address a long running pod ever saw. A swept entry is indistinguishable
// from one that was never there, so a cleared lockout is the observable.
func TestSweepDropsIdleEntriesRatherThanGrowingForever(t *testing.T) {
	const email = "a@example.com"
	clock := newDial()
	l := identity.NewLimiter(clock)

	for range 4 {
		l.Failed(email)
	}
	// Past the idle window and past any penalty, then touch the limiter so the
	// sweep runs.
	clock.advance(2 * time.Hour)
	l.Failed("someone-else@example.com")

	for i := range 4 {
		l.Failed(email)
		if _, locked := l.LockedOut(email); locked {
			t.Fatalf("failure %d locked the address out, so its stale run survived the sweep", i+1)
		}
	}
}

// TestLimiterIsSafeUnderConcurrentUse is the -race half: both maps are reached
// from every request goroutine at once.
func TestLimiterIsSafeUnderConcurrentUse(t *testing.T) {
	l := identity.NewLimiter(newDial())
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Allow("100.64.0.5")
			l.Failed("a@example.com")
			l.LockedOut("a@example.com")
			if i%10 == 0 {
				l.Succeeded("a@example.com")
			}
		}()
	}
	wg.Wait()
}
