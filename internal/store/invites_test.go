package store_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// mintInvite puts one live invite in the table and returns it.
func mintInvite(t *testing.T, s *store.Store, hash string, expires time.Time) store.Invite {
	t.Helper()
	inv, err := s.CreateInvite(t.Context(), store.NewInvite{
		CodeHash: hash, ExpiresAt: ids.Stamp(expires),
	})
	if err != nil {
		t.Fatalf("minting an invite: %v", err)
	}
	return inv
}

// account is one registration the spend transaction will attempt.
func account(email string) store.NewIdentityAccount {
	return store.NewIdentityAccount{
		Email: email, PasswordHash: "argon2id$fake", DisplayName: "someone",
	}
}

// TestOneInviteMakesOneAccountUnderRace pins AC-4 against a real SQLite file:
// the guarded update inside the transaction is what decides the winner, not the
// lookup that precedes it, so four racers on one code leave one account.
// covers: AC-4
func TestOneInviteMakesOneAccountUnderRace(t *testing.T) {
	s, clock := newStore(t)
	inv := mintInvite(t, s, "hash", clock.Now().Add(time.Hour))

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Four different addresses, so nothing but the invite can be what
			// refuses three of them.
			_, errs[i] = s.SpendInviteAndCreateAccount(t.Context(), inv.ID,
				ids.New(ids.Account, clock.Now()), account(string(rune('a'+i))+"@example.com"))
		}()
	}
	wg.Wait()

	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, store.ErrInviteInvalid):
		default:
			t.Errorf("racer %d failed with something other than a refusal: %v", i, err)
		}
	}
	if won != 1 {
		t.Errorf("%d racers created an account, want exactly 1", won)
	}
	accounts, err := s.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("got %d accounts, want 1", len(accounts))
	}
}

// TestATakenAddressLeavesTheInviteLive pins AC-10: the transaction rolls back
// whole, so the person's link still works on an address that is free.
// covers: AC-10
func TestATakenAddressLeavesTheInviteLive(t *testing.T) {
	s, clock := newStore(t)
	register(t, s, "taken@example.com")
	inv := mintInvite(t, s, "hash", clock.Now().Add(time.Hour))

	_, err := s.SpendInviteAndCreateAccount(t.Context(), inv.ID,
		ids.New(ids.Account, clock.Now()), account("taken@example.com"))
	if !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("registering a taken address: got %v, want ErrEmailTaken", err)
	}

	// The invite is untouched, which is the whole point: it still resolves live.
	if _, err := s.LiveInvite(t.Context(), "hash"); err != nil {
		t.Fatalf("the invite did not survive a taken address: %v", err)
	}
	if _, err := s.SpendInviteAndCreateAccount(t.Context(), inv.ID,
		ids.New(ids.Account, clock.Now()), account("free@example.com")); err != nil {
		t.Fatalf("spending the surviving invite: %v", err)
	}
}

// TestEveryDeadInviteReadsTheSame pins AC-2 and AC-5 at the layer that decides
// it: unknown, spent, revoked and expired are one error, and expiry needs no
// admin action and no sweep.
// covers: AC-2, AC-5
func TestEveryDeadInviteReadsTheSame(t *testing.T) {
	s, clock := newStore(t)

	spent := mintInvite(t, s, "spent", clock.Now().Add(time.Hour))
	if _, err := s.SpendInviteAndCreateAccount(t.Context(), spent.ID,
		ids.New(ids.Account, clock.Now()), account("spender@example.com")); err != nil {
		t.Fatalf("spending: %v", err)
	}
	revoked := mintInvite(t, s, "revoked", clock.Now().Add(time.Hour))
	if err := s.RevokeInvite(t.Context(), revoked.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	mintInvite(t, s, "expired", clock.Now().Add(time.Hour))
	// Nothing writes to the expired row. Time simply passes.
	clock.T = clock.T.Add(2 * time.Hour)

	for _, hash := range []string{"unknown", "spent", "revoked", "expired"} {
		if _, err := s.LiveInvite(t.Context(), hash); !errors.Is(err, store.ErrInviteInvalid) {
			t.Errorf("a %s code answered %v, want ErrInviteInvalid", hash, err)
		}
	}
}

// TestRevokeCarriesTheLivePredicate pins AC-7: revoking an invite that already
// ended changes nothing and is not found, so the two guards cannot write over
// each other's outcome.
// covers: AC-7
func TestRevokeCarriesTheLivePredicate(t *testing.T) {
	s, clock := newStore(t)

	spent := mintInvite(t, s, "spent", clock.Now().Add(time.Hour))
	if _, err := s.SpendInviteAndCreateAccount(t.Context(), spent.ID,
		ids.New(ids.Account, clock.Now()), account("spender@example.com")); err != nil {
		t.Fatalf("spending: %v", err)
	}
	if err := s.RevokeInvite(t.Context(), spent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoking a spent invite: got %v, want ErrNotFound", err)
	}

	expired := mintInvite(t, s, "expired", clock.Now().Add(time.Hour))
	clock.T = clock.T.Add(2 * time.Hour)
	if err := s.RevokeInvite(t.Context(), expired.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoking an expired invite: got %v, want ErrNotFound", err)
	}

	// A spent row stays spent rather than becoming revoked, so the list shows
	// what actually happened to it.
	rows, err := s.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	for _, r := range rows {
		if r.ID == spent.ID && r.RevokedAt != nil {
			t.Error("a refused revoke wrote revoked_at onto a spent invite")
		}
	}
}

// TestTheListNamesTheIssuerAndTheSpender pins AC-8: who invited whom is a fact
// on a row, resolved in the one query rather than by a read per invite.
// covers: AC-8
func TestTheListNamesTheIssuerAndTheSpender(t *testing.T) {
	s, clock := newStore(t)
	admin := register(t, s, "admin@example.com")

	if _, err := s.CreateInvite(t.Context(), store.NewInvite{
		CodeHash: "hash", Note: "for Sam", CreatedBy: admin.ID,
		ExpiresAt: ids.Stamp(clock.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("minting: %v", err)
	}
	inv, err := s.LiveInvite(t.Context(), "hash")
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if _, err := s.SpendInviteAndCreateAccount(t.Context(), inv.ID,
		ids.New(ids.Account, clock.Now()), account("sam@example.com")); err != nil {
		t.Fatalf("spending: %v", err)
	}

	rows, err := s.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Note == nil || *row.Note != "for Sam" {
		t.Errorf("note is %v, want \"for Sam\"", row.Note)
	}
	if row.IssuerName == nil || *row.IssuerName != "someone" {
		t.Errorf("issuer is %v, want the admin's display name", row.IssuerName)
	}
	if row.SpenderEmail == nil || *row.SpenderEmail != "sam@example.com" {
		t.Errorf("spender is %v, want sam@example.com", row.SpenderEmail)
	}
}

// TestTheBootstrapReadsAnswerTheEmptyDatabase pins the two conditions AC-13
// gates on, at the layer that answers them.
// covers: AC-13
func TestTheBootstrapReadsAnswerTheEmptyDatabase(t *testing.T) {
	s, clock := newStore(t)

	if yes, err := s.AnyAccountHasEmail(t.Context()); err != nil || yes {
		t.Errorf("an empty database reports a registered account: %v %v", yes, err)
	}
	if yes, err := s.AnyLiveBootstrapInvite(t.Context()); err != nil || yes {
		t.Errorf("an empty database reports a live bootstrap invite: %v %v", yes, err)
	}

	mintInvite(t, s, "hash", clock.Now().Add(time.Hour))
	if yes, err := s.AnyLiveBootstrapInvite(t.Context()); err != nil || !yes {
		t.Errorf("a live platform minted invite was not seen: %v %v", yes, err)
	}

	// An admin's invite is not a bootstrap one, so an outstanding one of those
	// does not stop the platform letting itself in.
	s2, clock2 := newStore(t)
	admin := register(t, s2, "admin@example.com")
	if _, err := s2.CreateInvite(t.Context(), store.NewInvite{
		CodeHash: "hash", CreatedBy: admin.ID, ExpiresAt: ids.Stamp(clock2.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("minting: %v", err)
	}
	if yes, err := s2.AnyLiveBootstrapInvite(t.Context()); err != nil || yes {
		t.Errorf("an admin's invite counted as a bootstrap one: %v %v", yes, err)
	}
	if yes, err := s2.AnyAccountHasEmail(t.Context()); err != nil || !yes {
		t.Errorf("a registered account was not seen: %v %v", yes, err)
	}
}

// TestTheSpendAuditsItself pins the half of AC-15 the edge cannot write. A
// handler is not allowed to learn whether an account was created, because that
// is exactly what the equal answer to a taken address hides, so the row naming
// the invite and the account it made is written where both are known and only
// when the transaction commits.
// covers: AC-15
func TestTheSpendAuditsItself(t *testing.T) {
	s, clock := newStore(t)
	inv := mintInvite(t, s, "hash", clock.Now().Add(time.Hour))

	accountID := ids.New(ids.Account, clock.Now())
	if _, err := s.SpendInviteAndCreateAccount(t.Context(), inv.ID, accountID,
		account("sam@example.com")); err != nil {
		t.Fatalf("spending: %v", err)
	}

	var gotAccount, action, targetType, targetID, outcome string
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT account_id, action, target_type, target_id, outcome FROM audit_log`,
	).Scan(&gotAccount, &action, &targetType, &targetID, &outcome); err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if gotAccount != accountID {
		t.Errorf("the row names account %q, want the account the invite created, %q", gotAccount, accountID)
	}
	if targetType != "invite" || targetID != inv.ID {
		t.Errorf("the row targets %s %q, want invite %q", targetType, targetID, inv.ID)
	}
	if action != "register" || outcome != "allowed" {
		t.Errorf("the row is %q/%q, want register/allowed", action, outcome)
	}

	// A spend that rolled back names nothing, because the row is written inside
	// the transaction it describes.
	dead := mintInvite(t, s, "dead", clock.Now().Add(time.Hour))
	if err := s.RevokeInvite(t.Context(), dead.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, err := s.SpendInviteAndCreateAccount(t.Context(), dead.ID,
		ids.New(ids.Account, clock.Now()), account("nobody@example.com")); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("spending a revoked invite: got %v, want ErrInviteInvalid", err)
	}
	var rows int
	if err := s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_log`).Scan(&rows); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("got %d audit rows, want only the one the committed spend wrote", rows)
	}
}

// TestATakenAddressWritesNoAuditRow pins the other rollback the audit row sits
// behind. The insert that loses is the account one, after the invite is already
// stamped spent, so this is the case where a row could survive a transaction
// that made no account, which would tell a reader an address is taken.
// covers: AC-10, AC-15
func TestATakenAddressWritesNoAuditRow(t *testing.T) {
	s, clock := newStore(t)
	register(t, s, "taken@example.com")
	inv := mintInvite(t, s, "hash", clock.Now().Add(time.Hour))

	if _, err := s.SpendInviteAndCreateAccount(t.Context(), inv.ID,
		ids.New(ids.Account, clock.Now()), account("taken@example.com")); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("registering a taken address: got %v, want ErrEmailTaken", err)
	}

	if got := auditRowsFor(t, s, inv.ID); got != 0 {
		t.Errorf("got %d audit rows naming the invite, want none: a refused registration claimed a spend", got)
	}
}

// TestOneInviteAuditsOnce pins the audit row against AC-4's race. Three losing
// racers roll back, so the one account that exists is described by exactly one
// row rather than by four.
// covers: AC-4, AC-15
func TestOneInviteAuditsOnce(t *testing.T) {
	s, clock := newStore(t)
	inv := mintInvite(t, s, "hash", clock.Now().Add(time.Hour))

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.SpendInviteAndCreateAccount(t.Context(), inv.ID,
				ids.New(ids.Account, clock.Now()), account(string(rune('a'+i))+"@example.com"))
		}()
	}
	wg.Wait()

	if got := auditRowsFor(t, s, inv.ID); got != 1 {
		t.Errorf("got %d audit rows for one invite, want 1", got)
	}
}

// auditRowsFor counts the audit rows naming one invite.
func auditRowsFor(t *testing.T, s *store.Store, inviteID string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_log WHERE target_type = 'invite' AND target_id = ?`, inviteID,
	).Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	return n
}
