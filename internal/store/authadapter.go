package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// AuthStore adapts the store to the narrow interface internal/auth declares.
// The mapping is deliberate rather than incidental: it is what lets the auth
// package stay free of this one, its generated row types, and its driver.
type AuthStore struct{ s *Store }

// ForAuth returns the auth facing view of the store.
func ForAuth(s *Store) AuthStore { return AuthStore{s: s} }

// Compile time proof that the adapter is what internal/auth asked for.
var _ auth.Store = AuthStore{}

// AccountByName returns the account with that name, or auth.ErrNoAccount.
func (a AuthStore) AccountByName(ctx context.Context, name string) (auth.Account, error) {
	acc, err := a.s.GetAccountByName(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return auth.Account{}, auth.ErrNoAccount
	}
	if err != nil {
		return auth.Account{}, err
	}
	return auth.Account{ID: acc.ID, Name: acc.Name}, nil
}

// CreateAccount registers a caller identity.
func (a AuthStore) CreateAccount(ctx context.Context, name string) (auth.Account, error) {
	acc, err := a.s.CreateAccount(ctx, name)
	if err != nil {
		return auth.Account{}, err
	}
	return auth.Account{ID: acc.ID, Name: acc.Name}, nil
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
	return auth.Account{ID: acc.ID, Name: acc.Name},
		auth.Token{ID: tok.ID, AccountID: tok.AccountID}, nil
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
