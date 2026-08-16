package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// AuthStore adapts the store to the narrow interface internal/auth declares.
// The mapping is deliberate rather than incidental: it is what lets the auth
// package stay free of this one, its generated row types, and its driver.
type AuthStore struct{ s *Store }

// ForAuth returns the auth facing view of the store.
func ForAuth(s *Store) AuthStore { return AuthStore{s: s} }

// Compile time proof that the adapter is what internal/auth asked for, on both
// of its routes.
var (
	_ auth.Store        = AuthStore{}
	_ auth.SessionStore = AuthStore{}
)

// toAuthAccount projects a row onto the caller identity, including the four
// fields spec 0007 added. A null email becomes an empty string, which is exactly
// what exempts the bootstrap account from the verified gate.
func toAuthAccount(acc Account) auth.Account {
	return auth.Account{
		ID:       acc.ID,
		Name:     acc.Name,
		Email:    deref(acc.Email),
		Verified: acc.EmailVerifiedAt != nil,
		Disabled: acc.DisabledAt != nil,
		IsAdmin:  acc.IsAdmin == 1,
		// A stamp, not a flag: the column holds when the configuration was handed
		// over, and every caller above only ever asks whether it happened.
		Connected: acc.ConnectedAt != nil,
	}
}

// ResolveSession turns a session hash into the account it belongs to, mapping the
// store's single indistinguishable failure onto the auth package's own.
func (a AuthStore) ResolveSession(ctx context.Context, tokenHash string) (auth.Account, auth.Session, error) {
	acc, sess, err := a.s.ResolveSession(ctx, tokenHash)
	if errors.Is(err, ErrSessionInvalid) {
		return auth.Account{}, auth.Session{}, auth.ErrSessionInvalid
	}
	if err != nil {
		return auth.Account{}, auth.Session{}, err
	}
	return toAuthAccount(acc), auth.Session{ID: sess.ID, AccountID: sess.AccountID}, nil
}

// TouchSession records a use and pushes the rolling expiry forward by lifetime,
// measured from the store's own clock.
func (a AuthStore) TouchSession(ctx context.Context, id string, lifetime time.Duration) error {
	return a.s.TouchSession(ctx, id, ids.Stamp(a.s.clock.Now().Add(lifetime)))
}

// AccountByName returns the account with that name, or auth.ErrNoAccount.
func (a AuthStore) AccountByName(ctx context.Context, name string) (auth.Account, error) {
	acc, err := a.s.GetAccountByName(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return auth.Account{}, auth.ErrNoAccount
	}
	if err != nil {
		return auth.Account{}, err
	}
	return toAuthAccount(acc), nil
}

// CreateAccount registers a caller identity.
func (a AuthStore) CreateAccount(ctx context.Context, name string) (auth.Account, error) {
	acc, err := a.s.CreateAccount(ctx, name)
	if err != nil {
		return auth.Account{}, err
	}
	return toAuthAccount(acc), nil
}

// ResolveToken turns a token hash into the account it belongs to, mapping the
// store's single indistinguishable failure onto the auth package's own.
func (a AuthStore) ResolveToken(ctx context.Context, tokenHash string) (auth.Account, auth.Token, error) {
	acc, tok, err := a.s.ResolveToken(ctx, tokenHash)
	if errors.Is(err, ErrTokenInvalid) {
		return auth.Account{}, auth.Token{}, auth.ErrTokenInvalid
	}
	if err != nil {
		return auth.Account{}, auth.Token{}, err
	}
	return toAuthAccount(acc), auth.Token{ID: tok.ID, AccountID: tok.AccountID}, nil
}

// RevokeTokensNamed kills every live token an account holds under one name.
func (a AuthStore) RevokeTokensNamed(ctx context.Context, accountID, name string) (int64, error) {
	n, err := a.s.q.RevokeTokensNamed(ctx, sqlcgen.RevokeTokensNamedParams{
		Now:       ptr(a.s.now()),
		AccountID: accountID,
		Name:      name,
	})
	if err != nil {
		return 0, fmt.Errorf("store: revoking tokens named %q on %s: %w", name, accountID, err)
	}
	return n, nil
}

// CreateAPIToken stores a token as its hash plus a non secret prefix.
func (a AuthStore) CreateAPIToken(ctx context.Context, t auth.NewToken) (auth.Token, error) {
	tok, err := a.s.CreateAPIToken(ctx, NewToken{
		AccountID: t.AccountID,
		Name:      t.Name,
		TokenHash: t.TokenHash,
		Prefix:    t.Prefix,
	})
	if err != nil {
		return auth.Token{}, err
	}
	return auth.Token{ID: tok.ID, AccountID: tok.AccountID}, nil
}

// TouchToken records that a token was just used.
func (a AuthStore) TouchToken(ctx context.Context, tokenID string) error {
	return a.s.TouchToken(ctx, tokenID)
}
