package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// register is the shorthand every test here starts from.
func register(t *testing.T, s *store.Store, email string) store.Account {
	t.Helper()
	acc, err := s.CreateIdentityAccount(t.Context(), store.NewIdentityAccount{
		Email:        email,
		PasswordHash: "argon2id$fake",
		DisplayName:  "someone",
	})
	if err != nil {
		t.Fatalf("registering %s: %v", email, err)
	}
	return acc
}

// TestFirstRegistrationIsAdmin pins AC-4: the first account with an email is the
// admin, later ones are not, and the bootstrap account never counts as the first
// because it holds no email.
func TestFirstRegistrationIsAdmin(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateAccount(t.Context(), "bootstrap"); err != nil {
		t.Fatalf("seeding the bootstrap account: %v", err)
	}

	first := register(t, s, "a@example.com")
	if first.IsAdmin != 1 {
		t.Error("the first registered account is not admin")
	}
	second := register(t, s, "b@example.com")
	if second.IsAdmin != 0 {
		t.Error("the second registered account came out admin")
	}
}

// TestRegistrationWritesTheIdIntoName pins the sourcing decision: accounts.name
// is NOT NULL UNIQUE and was designed as a machine identifier, so registration
// satisfies it with the account's own id and puts the human label in display_name.
func TestRegistrationWritesTheIdIntoName(t *testing.T) {
	s, _ := newStore(t)
	acc := register(t, s, "a@example.com")
	if acc.Name != acc.ID {
		t.Errorf("name is %q, want the account id %q", acc.Name, acc.ID)
	}
	if acc.DisplayName == nil || *acc.DisplayName != "someone" {
		t.Errorf("display name is %v, want %q", acc.DisplayName, "someone")
	}
}

// TestTwoAccountsCannotShareAnAddress proves the partial unique index, not the Go
// code: the second insert loses, and it loses as ErrEmailTaken.
func TestTwoAccountsCannotShareAnAddress(t *testing.T) {
	s, _ := newStore(t)
	register(t, s, "a@example.com")

	_, err := s.CreateIdentityAccount(t.Context(), store.NewIdentityAccount{
		Email: "a@example.com", PasswordHash: "argon2id$fake", DisplayName: "other",
	})
	if !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("registering a taken address: got %v, want ErrEmailTaken", err)
	}
}

// TestManyAccountsMayHaveNoAddress is the other half of the partial index: it is
// partial precisely so every token only account keeps a null.
func TestManyAccountsMayHaveNoAddress(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.CreateAccount(t.Context(), "bootstrap"); err != nil {
		t.Fatalf("creating the first: %v", err)
	}
	if _, err := s.CreateAccount(t.Context(), "robot"); err != nil {
		t.Fatalf("a second account with no address was refused: %v", err)
	}
}

// TestSessionResolvesOnlyWhileLive covers the four cases that must be
// indistinguishable: unknown, revoked, expired, and owned by a disabled account.
func TestSessionResolvesOnlyWhileLive(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")
	expiry := ids.Stamp(clock.Now().Add(30 * 24 * time.Hour))

	sess, err := s.CreateSession(ctx, acc.ID, "hash-live", expiry)
	if err != nil {
		t.Fatalf("creating a session: %v", err)
	}
	if _, _, err := s.ResolveSession(ctx, "hash-live"); err != nil {
		t.Fatalf("resolving a live session: %v", err)
	}

	if _, _, err := s.ResolveSession(ctx, "hash-unknown"); !errors.Is(err, store.ErrSessionInvalid) {
		t.Errorf("unknown session: got %v, want ErrSessionInvalid", err)
	}

	if err := s.RevokeSession(ctx, sess.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if _, _, err := s.ResolveSession(ctx, "hash-live"); !errors.Is(err, store.ErrSessionInvalid) {
		t.Errorf("revoked session: got %v, want ErrSessionInvalid", err)
	}

	// Expiry, on a fresh session, by moving the clock past it.
	if _, err := s.CreateSession(ctx, acc.ID, "hash-second", ids.Stamp(clock.Now().Add(time.Hour))); err != nil {
		t.Fatalf("creating the second session: %v", err)
	}
	clock.Advance(2 * time.Hour)
	if _, _, err := s.ResolveSession(ctx, "hash-second"); !errors.Is(err, store.ErrSessionInvalid) {
		t.Errorf("expired session: got %v, want ErrSessionInvalid", err)
	}
}

// TestDisableRevokesEverySession pins AC-10: the change takes effect on the next
// request, not whenever a session happens to lapse.
func TestDisableRevokesEverySession(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")
	expiry := ids.Stamp(clock.Now().Add(30 * 24 * time.Hour))
	for _, h := range []string{"hash-1", "hash-2"} {
		if _, err := s.CreateSession(ctx, acc.ID, h, expiry); err != nil {
			t.Fatalf("creating session %s: %v", h, err)
		}
	}

	if err := s.SetAccountDisabled(ctx, acc.ID, true); err != nil {
		t.Fatalf("disabling: %v", err)
	}
	for _, h := range []string{"hash-1", "hash-2"} {
		if _, _, err := s.ResolveSession(ctx, h); !errors.Is(err, store.ErrSessionInvalid) {
			t.Errorf("session %s survived the disable: %v", h, err)
		}
	}

	// Enabling does not resurrect them: revocation is one way.
	if err := s.SetAccountDisabled(ctx, acc.ID, false); err != nil {
		t.Fatalf("enabling: %v", err)
	}
	if _, _, err := s.ResolveSession(ctx, "hash-1"); !errors.Is(err, store.ErrSessionInvalid) {
		t.Error("enabling the account brought a revoked session back")
	}
}

// TestPasswordChangeRevokesSessionsAndLinks pins the other half of AC-10 and
// AC-29, both in the same transaction as the change.
func TestPasswordChangeRevokesSessionsAndLinks(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")
	linkExpiry := ids.Stamp(clock.Now().Add(24 * time.Hour))
	if _, err := s.CreateSession(ctx, acc.ID, "hash-1", ids.Stamp(clock.Now().Add(time.Hour))); err != nil {
		t.Fatalf("creating a session: %v", err)
	}
	if _, err := s.CreateEmailToken(ctx, acc.ID, store.PurposeVerifyEmail, "link-1", linkExpiry); err != nil {
		t.Fatalf("creating a link: %v", err)
	}

	if err := s.SetPassword(ctx, acc.ID, "argon2id$new"); err != nil {
		t.Fatalf("setting the password: %v", err)
	}
	if _, _, err := s.ResolveSession(ctx, "hash-1"); !errors.Is(err, store.ErrSessionInvalid) {
		t.Error("a session survived the password change")
	}
	if _, err := s.ConsumeEmailToken(ctx, "link-1", store.PurposeVerifyEmail); !errors.Is(err, store.ErrLinkInvalid) {
		t.Error("a link survived the password change")
	}
}

// TestEmailLinkIsSingleUseAndPurposeBound is AC-5 in one place: it works once,
// and a token minted for one purpose cannot be spent on the other.
func TestEmailLinkIsSingleUseAndPurposeBound(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")
	expiry := ids.Stamp(clock.Now().Add(24 * time.Hour))

	if _, err := s.CreateEmailToken(ctx, acc.ID, store.PurposeVerifyEmail, "link-1", expiry); err != nil {
		t.Fatalf("creating a link: %v", err)
	}

	// Spending it as a reset must fail even though the hash matches.
	if _, err := s.ConsumeEmailToken(ctx, "link-1", store.PurposePasswordReset); !errors.Is(err, store.ErrLinkInvalid) {
		t.Fatalf("a verify link was spendable as a reset: %v", err)
	}
	if _, err := s.ConsumeEmailToken(ctx, "link-1", store.PurposeVerifyEmail); err != nil {
		t.Fatalf("spending the link: %v", err)
	}
	if _, err := s.ConsumeEmailToken(ctx, "link-1", store.PurposeVerifyEmail); !errors.Is(err, store.ErrLinkInvalid) {
		t.Error("the link worked twice")
	}
}

// TestResendSupersedesTheLiveLink pins AC-6 and the partial unique index behind
// it: at most one live link per account per purpose, ever.
func TestResendSupersedesTheLiveLink(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")
	expiry := ids.Stamp(clock.Now().Add(24 * time.Hour))

	if _, err := s.CreateEmailToken(ctx, acc.ID, store.PurposeVerifyEmail, "link-1", expiry); err != nil {
		t.Fatalf("creating the first link: %v", err)
	}
	if _, err := s.CreateEmailToken(ctx, acc.ID, store.PurposeVerifyEmail, "link-2", expiry); err != nil {
		t.Fatalf("creating the second link: %v", err)
	}
	if _, err := s.ConsumeEmailToken(ctx, "link-1", store.PurposeVerifyEmail); !errors.Is(err, store.ErrLinkInvalid) {
		t.Error("the superseded link still worked")
	}
	if _, err := s.ConsumeEmailToken(ctx, "link-2", store.PurposeVerifyEmail); err != nil {
		t.Errorf("the fresh link did not work: %v", err)
	}

	// A reset link lives beside a verify link: the index is per purpose.
	if _, err := s.CreateEmailToken(ctx, acc.ID, store.PurposeVerifyEmail, "link-3", expiry); err != nil {
		t.Fatalf("creating a verify link: %v", err)
	}
	if _, err := s.CreateEmailToken(ctx, acc.ID, store.PurposePasswordReset, "link-4", expiry); err != nil {
		t.Fatalf("a reset link was refused beside a verify link: %v", err)
	}
}

// TestTwoLiveLinksForOnePurposeAreRefusedBySQLite checks the partial unique index
// directly, bypassing the Go layer that stamps the previous row consumed.
func TestTwoLiveLinksForOnePurposeAreRefusedBySQLite(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")
	expiry := ids.Stamp(clock.Now().Add(24 * time.Hour))
	now := ids.Stamp(clock.Now())

	insert := `INSERT INTO email_tokens (id, account_id, purpose, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := s.DB().ExecContext(ctx, insert, "eml_1", acc.ID, store.PurposeVerifyEmail, "raw-1", expiry, now); err != nil {
		t.Fatalf("the first raw insert failed: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, insert, "eml_2", acc.ID, store.PurposeVerifyEmail, "raw-2", expiry, now); err == nil {
		t.Error("the index allowed two live links for one purpose")
	}
}

// TestEmailTokenPurposeIsConstrained checks the CHECK constraint, not the Go code.
func TestEmailTokenPurposeIsConstrained(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO email_tokens (id, account_id, purpose, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"eml_1", acc.ID, "invite", "raw-1", ids.Stamp(clock.Now().Add(time.Hour)), ids.Stamp(clock.Now()))
	if err == nil {
		t.Error("the CHECK constraint allowed an unknown link purpose")
	}
}

// TestTokenListIsPerAccountAndNewestFirst pins AC-13's ordering and its
// isolation: one account's list never carries another's rows.
func TestTokenListIsPerAccountAndNewestFirst(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	mine := register(t, s, "a@example.com")
	theirs := register(t, s, "b@example.com")

	for _, name := range []string{"laptop", "agent"} {
		if _, err := s.CreateAPIToken(ctx, store.NewToken{
			AccountID: mine.ID, Name: name, TokenHash: "hash-" + name, Prefix: "dpl_" + name,
		}); err != nil {
			t.Fatalf("minting %s: %v", name, err)
		}
		clock.Advance(time.Second)
	}
	if _, err := s.CreateAPIToken(ctx, store.NewToken{
		AccountID: theirs.ID, Name: "other", TokenHash: "hash-other", Prefix: "dpl_o",
	}); err != nil {
		t.Fatalf("minting the other account's token: %v", err)
	}

	list, err := s.ListLiveAPITokens(ctx, mine.ID)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d tokens, want 2", len(list))
	}
	if list[0].Name != "agent" {
		t.Errorf("newest first is %q, want %q", list[0].Name, "agent")
	}
}

// TestExpiredAndRevokedTokensLeaveTheList is the rest of "live" in AC-13.
func TestExpiredAndRevokedTokensLeaveTheList(t *testing.T) {
	s, clock := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")

	short, err := s.CreateAPIToken(ctx, store.NewToken{
		AccountID: acc.ID, Name: "short", TokenHash: "hash-short", Prefix: "dpl_s",
		ExpiresAt: ids.Stamp(clock.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	revoked, err := s.CreateAPIToken(ctx, store.NewToken{
		AccountID: acc.ID, Name: "revoked", TokenHash: "hash-revoked", Prefix: "dpl_r",
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if err := s.RevokeAPIToken(ctx, revoked.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	clock.Advance(2 * time.Hour)
	list, err := s.ListLiveAPITokens(ctx, acc.ID)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	for _, tok := range list {
		if tok.ID == short.ID || tok.ID == revoked.ID {
			t.Errorf("token %q is still listed as live", tok.Name)
		}
	}
}

// TestVerifyStampsOnceProves the single use half of AC-5 at the row level: a
// second stamp is not found, so two requests carrying one link cannot both win.
func TestVerifyStampsOnce(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	acc := register(t, s, "a@example.com")

	if err := s.MarkEmailVerified(ctx, acc.ID); err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if err := s.MarkEmailVerified(ctx, acc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("verifying twice: got %v, want ErrNotFound", err)
	}

	got, err := s.GetAccountByEmail(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got.EmailVerifiedAt == nil {
		t.Error("email_verified_at was not stamped")
	}
}
