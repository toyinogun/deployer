package ids_test

import (
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
)

// The ordering tests below deliberately do not run in parallel: they share the
// package's one monotonic generator, so running them alone keeps each one
// measuring its own ids. New holds ordering whoever else is drawing ids
// alongside them, which is what TestOrderingSurvivesAnEarlierStamp covers.
func TestIdsInTheSameMillisecondAreDistinctAndOrdered(t *testing.T) {
	// A fixed instant is the hard case: every id shares a timestamp, so only the
	// monotonic entropy separates them.
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	const n = 1000
	got := make([]string, n)
	for i := range got {
		got[i] = ids.New(ids.Deployment, at)
	}

	seen := make(map[string]bool, n)
	for _, id := range got {
		if seen[id] {
			t.Fatalf("duplicate id %q generated in the same millisecond", id)
		}
		seen[id] = true
	}

	if !slices.IsSorted(got) {
		t.Error("ids generated in order do not sort in that order as text")
	}
}

func TestSortingIdsAsTextSortsThemByTime(t *testing.T) {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	var got []string
	for i := range 20 {
		got = append(got, ids.New(ids.Deployment, base.Add(time.Duration(i)*time.Second)))
	}
	shuffled := slices.Clone(got)
	sort.Sort(sort.Reverse(sort.StringSlice(shuffled)))
	slices.Sort(shuffled)
	if !slices.Equal(shuffled, got) {
		t.Error("sorting ids as text did not restore creation order")
	}
}

// Two callers holding their own clocks reach New in whatever order the scheduler
// picks, so an earlier stamp lands between two later ones. That used to reseed
// the entropy and leave the later pair out of order, which read back as rotated
// deployment events and a queue claiming the wrong row.
//
// covers: AC-3
func TestOrderingSurvivesAnEarlierStamp(t *testing.T) {
	later := time.Date(2026, 8, 11, 12, 10, 0, 0, time.UTC)
	earlier := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	for i := range 200 {
		before := ids.New(ids.DeploymentEvent, later)
		ids.New(ids.AuditLog, earlier)
		after := ids.New(ids.DeploymentEvent, later)
		if before >= after {
			t.Fatalf("round %d: an id drawn later sorts first: %q >= %q", i, before, after)
		}
	}
}

// The store draws ids from many goroutines at once, each holding its own clock,
// which is the shape the ordering fault appeared in. Every goroutine's own run of
// ids must still sort, and no two ids anywhere may collide.
//
// covers: AC-3
func TestConcurrentCallersKeepDistinctOrderedIds(t *testing.T) {
	const callers, each = 8, 250
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	runs := make([][]string, callers)
	for c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each caller sits at its own instant, so callers reach New out of
			// order however the scheduler interleaves them.
			at := base.Add(time.Duration(c) * time.Minute)
			run := make([]string, each)
			for i := range run {
				run[i] = ids.New(ids.Deployment, at)
			}
			runs[c] = run
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, callers*each)
	for c, run := range runs {
		if !slices.IsSorted(run) {
			t.Errorf("caller %d: its own ids do not sort into the order it drew them", c)
		}
		for _, id := range run {
			if seen[id] {
				t.Fatalf("caller %d drew a duplicate id %q", c, id)
			}
			seen[id] = true
		}
	}
	if len(seen) != callers*each {
		t.Errorf("got %d distinct ids, want %d", len(seen), callers*each)
	}
}

// Holding the timestamp at its high water mark must not pin it there for good:
// once the clock passes the mark, a later id has to carry the later time, or
// every id after the first backwards stamp would bunch onto one instant.
//
// This test deliberately works far in the future, past every other instant in
// this package, so nothing earlier can hold its stamps down. It does raise the
// generator's mark for whatever runs after it, which is harmless: the other
// tests asserting order compare their own ids to each other, and holding those
// at one instant keeps them ordered.
//
// covers: AC-3
func TestTheClockStillMovesForward(t *testing.T) {
	base := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

	first := ids.New(ids.Deployment, base)
	second := ids.New(ids.Deployment, base.Add(time.Hour))

	_, a, err := ids.Parse(first)
	if err != nil {
		t.Fatalf("parsing %q: %v", first, err)
	}
	_, b, err := ids.Parse(second)
	if err != nil {
		t.Fatalf("parsing %q: %v", second, err)
	}
	if a.Time() >= b.Time() {
		t.Errorf("an id an hour later carries stamp %d, not past the earlier %d", b.Time(), a.Time())
	}
}

// An id drawn at an instant the generator has already passed keeps its place in
// line rather than jumping back to where its own clock says it belongs.
//
// covers: AC-3
func TestABackwardsStampNeverSortsBackwards(t *testing.T) {
	late := time.Date(2098, 6, 1, 0, 0, 0, 0, time.UTC)
	early := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	first := ids.New(ids.Deployment, late)
	stale := ids.New(ids.Deployment, early)

	if stale <= first {
		t.Errorf("an id drawn second sorts first: %q <= %q", stale, first)
	}
}

func TestPrefixes(t *testing.T) {
	at := time.Now()
	for _, p := range []ids.Prefix{
		ids.Account, ids.APIToken, ids.App, ids.Upload,
		ids.Deployment, ids.DeploymentEvent, ids.Release, ids.AuditLog,
	} {
		id := ids.New(p, at)
		if !strings.HasPrefix(id, string(p)+"_") {
			t.Errorf("%q does not carry prefix %q", id, p)
		}
		if !ids.HasPrefix(id, p) {
			t.Errorf("HasPrefix(%q, %q) = false", id, p)
		}
		if ids.HasPrefix(id, ids.Prefix("zzz")) {
			t.Errorf("%q wrongly matched a foreign prefix", id)
		}
	}
}

func TestParseRejectsRubbish(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "nopfx", "app_notaulid", "app_"} {
		if _, _, err := ids.Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted a malformed id", in)
		}
	}
}

func TestStampIsUTCAndSortable(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("UTC+5", 5*60*60)
	earlier := ids.Stamp(time.Date(2026, 8, 11, 23, 0, 0, 0, zone))
	later := ids.Stamp(time.Date(2026, 8, 11, 19, 0, 0, 0, time.UTC))
	if !strings.HasSuffix(earlier, "Z") {
		t.Errorf("Stamp did not convert to UTC: %q", earlier)
	}
	// 23:00 at UTC+5 is 18:00 UTC, so it must sort before 19:00 UTC.
	if earlier >= later {
		t.Errorf("text ordering disagrees with time ordering: %q >= %q", earlier, later)
	}
}

func TestFixedClock(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	c := &ids.FixedClock{T: at}
	if !c.Now().Equal(at) {
		t.Fatalf("FixedClock.Now() = %v, want %v", c.Now(), at)
	}
	c.Advance(2 * time.Hour)
	if !c.Now().Equal(at.Add(2 * time.Hour)) {
		t.Errorf("Advance did not move the clock: %v", c.Now())
	}
}
