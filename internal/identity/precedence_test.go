package identity_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// silentMailer accepts every message and keeps none. A precedence test cares
// only whether a sender exists, because that is the one thing IssueInvite reads
// it for before it decides which refusal wins.
type silentMailer struct{}

func (silentMailer) Send(context.Context, string, string, string) error { return nil }

// mintService opens a real SQLite file with one registered address on it and
// returns a Service that either has a sender or does not, plus the store behind
// it for the tests that need to put an account on it themselves.
func mintService(t *testing.T, withMailer bool) (*identity.Service, *store.Store) {
	t.Helper()
	clock := &ids.FixedClock{T: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(store.Options{
		Path:  filepath.Join(t.TempDir(), "deployer.db"),
		Clock: clock,
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := st.CreateIdentityAccount(t.Context(), store.NewIdentityAccount{
		Email: "taken@example.test", PasswordHash: "argon2id$fake", DisplayName: "Taken",
	}); err != nil {
		t.Fatalf("registering the address that is already taken: %v", err)
	}

	var mailer identity.Mailer
	if withMailer {
		mailer = silentMailer{}
	}
	return identity.NewService(store.ForIdentity(st), mailer, clock, identity.Options{
		ConsoleURL: "https://console.example.test",
		Hasher:     identity.NewHasherWith(2, 64, 1),
	}), st
}

// TestTheMintRefusalsKeepTheirOrder pins the precedence IssueInvite documents:
// note, then address format, then the nil mailer, then the address already
// having an account. Each refusal has a test of its own already, and each one
// passes just as happily with the checks in any order, because a single bad
// value can only produce one answer. Only a submission that is wrong in two ways
// at once can say which check ran first.
//
// The order is not arbitrary. The cheapest and most caller specific answer comes
// first, and the nil mailer deliberately precedes the account read, so a
// platform that could not send the invite anyway never reads the accounts table
// to answer a question it cannot act on. A refactor that reordered these would
// leak that read, and nothing else in the suite would notice.
// covers: AC-2, AC-3, AC-7
func TestTheMintRefusalsKeepTheirOrder(t *testing.T) {
	longNote := strings.Repeat("x", identity.NoteLimit+1)

	for _, c := range []struct {
		name       string
		withMailer bool
		note       string
		email      string
		want       identity.Code
	}{
		{"the note beats a malformed address", true, longNote, "not an address", identity.CodeNoteTooLong},
		{"the note beats a taken address", true, longNote, "taken@example.test", identity.CodeNoteTooLong},
		{"the note beats a missing sender", false, longNote, "sam@example.test", identity.CodeNoteTooLong},
		{"the address format beats a missing sender", false, "", "not an address", identity.CodeEmailInvalid},
		{"a missing sender beats a taken address", false, "", "taken@example.test", identity.CodeMailUnavailable},
		{"a taken address on its own", true, "", "taken@example.test", identity.CodeAddressRegistered},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, _ := mintService(t, c.withMailer)
			_, err := svc.IssueInvite(t.Context(), "", c.note, c.email)
			code, refusal := identity.CodeOf(err)
			if !refusal || code != c.want {
				t.Fatalf("got %v (code %s), want %s", err, code, c.want)
			}
		})
	}
}

// TestAnInviterWithNoNameFallsBackToThePlatform is the branch of inviterName
// the harnesses never reach, because every test admin they build has a display
// name. A message that arrives saying less is better than one that does not
// arrive, so a name the platform cannot resolve must not fail the send.
//
// The other half of that branch, a lookup that errors, is unreachable from here
// on purpose: `created_by` carries a foreign key, so an invite naming an admin
// who does not exist is refused by the database before any message is composed.
// covers: AC-4
func TestAnInviterWithNoNameFallsBackToThePlatform(t *testing.T) {
	svc, st := mintService(t, true)

	// A real admin with nothing to be called, which is the shape the fallback
	// exists for.
	nameless, err := st.CreateIdentityAccount(t.Context(), store.NewIdentityAccount{
		Email: "nameless@example.test", PasswordHash: "argon2id$fake",
	})
	if err != nil {
		t.Fatalf("registering an admin with no display name: %v", err)
	}

	issued, err := svc.IssueInvite(t.Context(), nameless.ID, "for Sam", "sam@example.test")
	if err != nil {
		t.Fatalf("minting for an admin with no display name: %v", err)
	}
	if !issued.Sent {
		t.Error("an inviter with no name stopped the send")
	}
}
