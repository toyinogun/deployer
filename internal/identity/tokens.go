package identity

import (
	"context"
	"errors"
	"time"
)

// Minted is a freshly created API token. Raw is the only time the value exists
// outside the caller's own keeping: it is returned once and never again.
type Minted struct {
	Raw   string
	Token TokenView
}

// MintToken creates an API token for a verified account. An unverified account is
// refused, because a token is exactly the thing verification gates (AC-15).
//
// Over HTTP the session gate in internal/auth refuses that caller first, with the
// same code, so this branch is the invariant held here rather than the answer a
// caller sees. It stays because a second caller of this service should not have to
// know the gate ran.
func (s *Service) MintToken(ctx context.Context, account Account, name string, days int) (Minted, error) {
	if account.Email != "" && !account.Verified {
		return Minted{}, Fail(CodeEmailUnverified, "confirm your email address before minting a token")
	}
	if name == "" {
		return Minted{}, Fail(CodeTokenNameTaken, "a token needs a name")
	}
	expires, _, err := TokenExpiry(s.clock.Now(), days)
	if err != nil {
		return Minted{}, err
	}

	raw, err := NewAPIToken()
	if err != nil {
		return Minted{}, err
	}
	view, err := s.store.MintToken(ctx, account.ID, name, HashSecret(raw), TokenPrefix(raw), expires)
	if errors.Is(err, ErrTokenNameTaken) {
		return Minted{}, Fail(CodeTokenNameTaken, "you already have a live token by that name")
	}
	if err != nil {
		return Minted{}, err
	}
	return Minted{Raw: raw, Token: view}, nil
}

// MarkConnected records that this account has been handed its agent
// configuration. It is safe to call on every visit: the store's statement is
// conditional, so the stamp lands once and a later call changes nothing
// (spec 0023, AC-4, AC-4a).
func (s *Service) MarkConnected(ctx context.Context, accountID string) error {
	return s.store.MarkConnected(ctx, accountID)
}

// Now is the service clock, for a caller that has to name a date in text it
// composes rather than in a row it writes. The clock is injected, so a test owns
// what "today" means here exactly as it does everywhere else.
func (s *Service) Now() time.Time { return s.clock.Now() }

// ListTokens reads one account's live tokens, newest first. It never reads
// another account's, because the account id is the caller's own resolved identity
// rather than anything they sent (AC-13).
func (s *Service) ListTokens(ctx context.Context, accountID string) ([]TokenView, error) {
	return s.store.ListTokens(ctx, accountID)
}

// RevokeToken kills a token the caller owns. A token belonging to somebody else
// is not_found, the same answer an unknown id gets, so an id cannot be probed for
// existence (AC-14).
func (s *Service) RevokeToken(ctx context.Context, accountID, tokenID string) error {
	tok, err := s.store.TokenByID(ctx, tokenID)
	switch {
	case errors.Is(err, ErrNotFound), err == nil && tok.AccountID != accountID:
		return notFound()
	case err != nil:
		return err
	}
	if err := s.store.RevokeToken(ctx, tokenID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound()
		}
		return err
	}
	return nil
}

// ListAccounts is the admin view: every account, newest first (AC-19).
func (s *Service) ListAccounts(ctx context.Context) ([]Account, error) {
	return s.store.ListAccounts(ctx)
}

// SetDisabled locks an account out or lets it back in. Disabling revokes every
// live session and link in the same transaction as the change (AC-10).
func (s *Service) SetDisabled(ctx context.Context, accountID string, disabled bool) error {
	if err := s.store.SetDisabled(ctx, accountID, disabled); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound()
		}
		return err
	}
	return nil
}

// RevokeTokenOf kills another account's token, which is the one thing an admin
// may do to a credential that is not theirs. The token has to actually belong to
// the named account, so a mismatched pair is not_found rather than a silent
// revocation of somebody else's row (AC-19).
func (s *Service) RevokeTokenOf(ctx context.Context, accountID, tokenID string) error {
	return s.RevokeToken(ctx, accountID, tokenID)
}

// AccountByID reads one account, for the admin surface and for a session that has
// to be re-read.
func (s *Service) AccountByID(ctx context.Context, id string) (Account, error) {
	acc, err := s.store.AccountByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Account{}, notFound()
	}
	return acc, err
}

// notFound is the single answer for a row that does not exist and for one that
// belongs to somebody else.
func notFound() error { return Fail(CodeNotFound, "no such thing") }

// TokenPrefix is the readable head of a token, stored beside the hash so a row
// can be told apart from another without being usable.
func TokenPrefix(raw string) string {
	const prefixLen = 8
	if len(raw) < prefixLen {
		return raw
	}
	return raw[:prefixLen]
}
