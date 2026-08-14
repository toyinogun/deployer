// Package ids generates the platform's primary keys: a fixed per entity prefix
// followed by a ULID, so an id names its own type and sorts by creation time.
package ids

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Prefix is the fixed head of an entity's id, for example "app" in "app_01J...".
type Prefix string

// The prefix for every entity in the platform data model (spec 0002).
const (
	Account         Prefix = "acc"
	APIToken        Prefix = "tok"
	App             Prefix = "app"
	Upload          Prefix = "upl"
	Deployment      Prefix = "dep"
	DeploymentEvent Prefix = "evt"
	Release         Prefix = "rel"
	AuditLog        Prefix = "aud"

	// Added by spec 0007, accounts and API tokens.
	Session    Prefix = "ses"
	EmailToken Prefix = "eml"

	// Added by spec 0015, invite only registration.
	Invite Prefix = "inv"
)

// entropy is monotonic so two ids drawn in the same millisecond still differ and
// still sort in the order they were drawn. It is not safe for concurrent use, so
// the mutex guards it rather than the caller having to. lastMS is the high water
// mark of every timestamp handed to it, see New.
var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
	lastMS  uint64
)

// New returns a fresh id for the given prefix, stamped with t.
//
// Ids are always distinct, and they always sort, as text, into the order they
// were drawn. The platform relies on that: deployment events are read back in
// id order within an instant, the build queue claims the lowest queued id, and
// listings page by id as a cursor.
//
// Holding that guarantee costs one high water mark. The monotonic entropy only
// climbs while the timestamp holds steady; an earlier timestamp reseeds it at
// random, and ordering within the run is then lost. Concurrent callers read the
// clock in one order and arrive here in another, so a stamp earlier than the
// last one is normal rather than a fault, and it is pinned to the last one
// instead of being allowed to move backwards. An id can therefore read a
// fraction of a millisecond later than the moment it was drawn. Nothing reads
// the time back out of an id; every row carries its own timestamp column.
func New(p Prefix, t time.Time) string {
	ms := ulid.Timestamp(t)
	mu.Lock()
	defer mu.Unlock()
	if ms < lastMS {
		ms = lastMS
	}
	lastMS = ms
	return string(p) + "_" + ulid.MustNew(ms, entropy).String()
}

// Parse splits an id into its prefix and ULID, checking both are well formed.
func Parse(id string) (Prefix, ulid.ULID, error) {
	prefix, rest, ok := strings.Cut(id, "_")
	if !ok {
		return "", ulid.ULID{}, fmt.Errorf("ids: %q has no prefix", id)
	}
	u, err := ulid.Parse(rest)
	if err != nil {
		return "", ulid.ULID{}, fmt.Errorf("ids: parsing %q: %w", id, err)
	}
	return Prefix(prefix), u, nil
}

// HasPrefix reports whether id is well formed and carries the given prefix.
func HasPrefix(id string, p Prefix) bool {
	got, _, err := Parse(id)
	return err == nil && got == p
}
