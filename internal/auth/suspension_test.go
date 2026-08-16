package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// seededAs returns a store holding one live token for an account shaped by
// modify, which is how each credential shape below is built from the same good
// starting point.
func seededAs(raw string, modify func(*auth.Account)) *fakeStore {
	s := newFakeStore()
	account := auth.Account{ID: "acct_1", Name: "person", Email: "a@example.test", Verified: true}
	modify(&account)
	s.accounts[account.Name] = account
	s.tokens[auth.HashToken(raw)] = auth.Token{ID: "tok_1", AccountID: account.ID}
	return s
}

// TestCredentialShapesStayApart is the whole of AC-12, and the reason it earns
// its own test: dropping the disabled filter out of ResolveToken moved a gate
// that used to be enforced twice, in the query and in Go, down to being enforced
// once in Go, on the most sensitive path in the platform.
//
// Unknown, revoked and expired are all the same empty result from the store, so
// they arrive here as one shape and must stay one answer. An unverified account
// joins them. Only a good token on a suspended account is told apart, and only
// because the caller holding it already proved it holds that account's
// credential, so it learns nothing it did not already know. covers: AC-12
func TestCredentialShapesStayApart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		store   *fakeStore
		present string
		want    error
	}{
		{
			name:    "unknown, revoked or expired",
			store:   seededAs("good-token", func(*auth.Account) {}),
			present: "no-token-matches-this",
			want:    auth.ErrTokenInvalid,
		},
		{
			name:    "unverified",
			store:   seededAs("good-token", func(a *auth.Account) { a.Verified = false }),
			present: "good-token",
			want:    auth.ErrTokenInvalid,
		},
		{
			name:    "suspended",
			store:   seededAs("good-token", func(a *auth.Account) { a.Disabled = true }),
			present: "good-token",
			want:    auth.ErrAccountSuspended,
		},
		{
			name:    "good",
			store:   seededAs("good-token", func(*auth.Account) {}),
			present: "good-token",
			want:    nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account, err := auth.NewAuthenticator(tc.store, nil).Authenticate(ctx, tc.present, "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("authenticating a %s credential returned %v, want %v", tc.name, err, tc.want)
			}
			switch {
			case tc.want == nil:
			case errors.Is(tc.want, auth.ErrAccountSuspended):
				// The account travels with the refusal so the surface above can
				// audit which one it refused, and answer account_suspended rather
				// than a blank credential error.
				if account.ID == "" {
					t.Error("a suspended refusal carried no account, so nothing above it can name the account it refused")
				}
			default:
				if account.ID != "" {
					t.Errorf("an invalid credential resolved to account %s, which it must never do", account.ID)
				}
			}
		})
	}
}
