package identity_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// TestWithNoMailSenderABoundMintIsRefused is AC-7. A nil Mailer is a supported
// state and it means this platform cannot send an invite, so a mint carrying an
// address is refused mail_unavailable and writes nothing. An unbound mint on the
// same platform still works, which is what keeps a platform with no mail
// configured usable at all.
//
// The refusal is also proof of the documented precedence: the nil mailer is
// checked before the accounts table is read, so a platform that could not act on
// the answer never asks the question.
// covers: AC-7
func TestWithNoMailSenderABoundMintIsRefused(t *testing.T) {
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

	// Nil, which is exactly what a platform with no DEPLOYER_RESEND_API_KEY runs.
	svc := identity.NewService(store.ForIdentity(st), nil, clock, identity.Options{
		ConsoleURL: "https://console.example.test",
		Hasher:     identity.NewHasherWith(2, 64, 1),
	})

	_, err = svc.IssueInvite(t.Context(), "", "for Sam", "sam@example.test")
	code, refusal := identity.CodeOf(err)
	if !refusal || code != identity.CodeMailUnavailable {
		t.Fatalf("a bound mint with no sender: got %v (code %s), want mail_unavailable", err, code)
	}

	rows, err := st.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing invites: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused mint wrote %d rows", len(rows))
	}

	// The unbound path is untouched, so the platform still hands out links.
	issued, err := svc.IssueInvite(t.Context(), "", "for whoever", "")
	if err != nil {
		t.Fatalf("an unbound mint with no sender: %v", err)
	}
	if issued.Link == "" {
		t.Error("an unbound mint produced no link")
	}
	if issued.Email != "" || issued.Sent {
		t.Errorf("an unbound mint reported email=%q sent=%v", issued.Email, issued.Sent)
	}
}
