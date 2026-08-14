package identity_test

import (
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
)

// TestInviteStateOfDerivesTheOneState pins the four way branch an admin's list
// reads. Nothing here is stored, so this function is the whole rule: it decides
// what a row says it is, and expiry in particular is reached without any write
// at all.
// covers: AC-8
func TestInviteStateOfDerivesTheOneState(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	future, past := stamp(now.Add(time.Hour)), stamp(now.Add(-time.Hour))

	for _, tc := range []struct {
		name string
		row  identity.InviteRow
		want identity.InviteState
	}{
		{"a fresh one is live", identity.InviteRow{ExpiresAt: future}, identity.InviteLive},
		{"one past its expiry", identity.InviteRow{ExpiresAt: past}, identity.InviteExpired},
		{"a revoked one", identity.InviteRow{ExpiresAt: future, RevokedAt: stamp(now)}, identity.InviteRevoked},
		{"a spent one", identity.InviteRow{ExpiresAt: future, ConsumedAt: stamp(now)}, identity.InviteSpent},
		// The order matters once time passes a row that already ended: what
		// happened to it is what an admin needs to read, not that it also ran out.
		{"spent, then expired", identity.InviteRow{ExpiresAt: past, ConsumedAt: stamp(now)}, identity.InviteSpent},
		{"revoked, then expired", identity.InviteRow{ExpiresAt: past, RevokedAt: stamp(now)}, identity.InviteRevoked},
		// Expiring exactly now is expired: the boundary closes rather than
		// leaving a code good for one more instant.
		{"expiring on the tick", identity.InviteRow{ExpiresAt: stamp(now)}, identity.InviteExpired},
		// A stamp the platform cannot read back is treated as expired, because
		// refusing a code it cannot reason about is the safe direction.
		{"an unreadable expiry", identity.InviteRow{ExpiresAt: "not a time"}, identity.InviteExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := identity.InviteStateOf(tc.row, now); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheckNoteBoundsTheOneCallerSuppliedValue pins the bound and the code it is
// refused with. The code is what a caller branches on, so an over long note
// cannot answer with one that names an address.
// covers: AC-6
func TestCheckNoteBoundsTheOneCallerSuppliedValue(t *testing.T) {
	if got, err := identity.CheckNote("  for Sam  "); err != nil || got != "for Sam" {
		t.Errorf("a note came back %q %v, want it trimmed", got, err)
	}

	// The bound counts runes rather than bytes, so a note of multi byte
	// characters is not refused for being under the limit in what a person sees.
	atLimit := ""
	for range identity.NoteLimit {
		atLimit += "é"
	}
	if _, err := identity.CheckNote(atLimit); err != nil {
		t.Errorf("a note of %d characters was refused: %v", identity.NoteLimit, err)
	}

	_, err := identity.CheckNote(atLimit + "é")
	code, isRefusal := identity.CodeOf(err)
	if !isRefusal || code != identity.CodeNoteTooLong {
		t.Errorf("an over long note answered %q (refusal %v), want note_too_long", code, isRefusal)
	}
}
