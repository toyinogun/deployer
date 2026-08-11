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
)

// entropy is monotonic so two ids drawn in the same millisecond still differ and
// still sort in the order they were drawn. It is not safe for concurrent use, so
// the mutex guards it rather than the caller having to.
var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// New returns a fresh id for the given prefix, stamped with t.
//
// Ids are always distinct. They also sort, as text, into the order they were
// created, as long as t never moves backwards: a timestamp earlier than the last
// one reseeds the entropy, so ordering within that millisecond is lost even
// though uniqueness is not. Every caller passes a Clock, and the wall clock only
// goes backwards on a time correction, so this is a property of the id, not a
// promise the platform relies on for correctness.
func New(p Prefix, t time.Time) string {
	mu.Lock()
	defer mu.Unlock()
	return string(p) + "_" + ulid.MustNew(ulid.Timestamp(t), entropy).String()
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
