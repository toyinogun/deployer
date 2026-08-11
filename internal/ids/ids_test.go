package ids_test

import (
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
)

// The ordering tests below deliberately do not run in parallel: they share the
// package's one monotonic generator, and a concurrent test stamping a later
// millisecond resets its entropy, which is exactly what New documents.
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
