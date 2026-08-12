package store_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/store"
)

// liveTokens counts the tokens an account still holds that have not been
// revoked, read straight off the table rather than through the store, so the
// assertion is about the rows rather than about a query's filtering.
func liveTokens(t *testing.T, s *store.Store, accountID string) int {
	t.Helper()
	var n int
	err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM api_tokens WHERE account_id = ? AND revoked_at IS NULL`,
		accountID).Scan(&n)
	if err != nil {
		t.Fatalf("counting live tokens: %v", err)
	}
	return n
}

func countAccounts(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("counting accounts: %v", err)
	}
	return n
}

// Seeding twice with the same token leaves one account and one token row
// (spec 0004, AC-1).
func TestBootstrapIsIdempotent(t *testing.T) {
	s, _ := newStore(t)
	as := store.ForAuth(s)
	const token = "dpl_the_one_token"

	for range 3 {
		if err := auth.Bootstrap(t.Context(), as, token); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}

	if got := countAccounts(t, s); got != 1 {
		t.Errorf("accounts = %d, want 1", got)
	}
	acc, err := s.GetAccountByName(t.Context(), auth.BootstrapAccountName)
	if err != nil {
		t.Fatalf("reading the bootstrap account: %v", err)
	}
	if got := liveTokens(t, s, acc.ID); got != 1 {
		t.Errorf("live tokens = %d, want 1", got)
	}
	// The token resolves, which is the only thing the seeding exists to make true.
	if _, _, err := s.ResolveToken(t.Context(), auth.HashToken(token)); err != nil {
		t.Errorf("the seeded token does not resolve: %v", err)
	}
}

// Rotating the sealed secret leaves exactly one working credential, not two.
func TestBootstrapRotatesTheToken(t *testing.T) {
	s, _ := newStore(t)
	as := store.ForAuth(s)

	if err := auth.Bootstrap(t.Context(), as, "dpl_old"); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if err := auth.Bootstrap(t.Context(), as, "dpl_new"); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	acc, err := s.GetAccountByName(t.Context(), auth.BootstrapAccountName)
	if err != nil {
		t.Fatalf("reading the bootstrap account: %v", err)
	}
	if got := liveTokens(t, s, acc.ID); got != 1 {
		t.Errorf("live tokens = %d, want 1", got)
	}
	if _, _, err := s.ResolveToken(t.Context(), auth.HashToken("dpl_new")); err != nil {
		t.Errorf("the new token does not resolve: %v", err)
	}
	if _, _, err := s.ResolveToken(t.Context(), auth.HashToken("dpl_old")); err == nil {
		t.Error("the old token still resolves after a rotation")
	}
}

// An unset token is a supported local run: nothing is seeded and nothing fails.
func TestBootstrapWithNoTokenSeedsNothing(t *testing.T) {
	s, _ := newStore(t)
	if err := auth.Bootstrap(t.Context(), store.ForAuth(s), ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := countAccounts(t, s); got != 0 {
		t.Errorf("accounts = %d, want 0", got)
	}
}

// The stored value is the hash, and the prefix beside it is not usable on its own.
func TestBootstrapStoresOnlyTheHash(t *testing.T) {
	s, _ := newStore(t)
	const token = "dpl_never_stored_in_the_clear"
	if err := auth.Bootstrap(t.Context(), store.ForAuth(s), token); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var hash, prefix string
	err := s.DB().QueryRow(`SELECT token_hash, token_prefix FROM api_tokens`).Scan(&hash, &prefix)
	if err != nil {
		t.Fatalf("reading the token row: %v", err)
	}
	if hash != auth.HashToken(token) {
		t.Errorf("token_hash = %q, want the SHA-256 of the token", hash)
	}
	if hash == token {
		t.Error("the raw token was stored")
	}
	if prefix != token[:8] {
		t.Errorf("token_prefix = %q, want the first 8 characters", prefix)
	}
	if _, _, err := s.ResolveToken(t.Context(), prefix); err == nil {
		t.Error("the prefix resolves as a token, so it is not merely readable")
	}
}
