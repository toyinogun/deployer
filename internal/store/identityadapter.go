package store

import (
	"context"
	"errors"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
)

// IdentityStore adapts the store to the narrow interface internal/identity
// declares. The mapping is what lets that package hold the rules without
// depending on this one, its generated row types, or its driver.
type IdentityStore struct{ s *Store }

// ForIdentity returns the identity facing view of the store.
func ForIdentity(s *Store) IdentityStore { return IdentityStore{s: s} }

// Compile time proof that the adapter is what internal/identity asked for.
var _ identity.Store = IdentityStore{}

// AccountByEmail reads one account by its address.
func (a IdentityStore) AccountByEmail(ctx context.Context, email string) (identity.Account, error) {
	acc, err := a.s.GetAccountByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		return identity.Account{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Account{}, err
	}
	return toIdentityAccount(acc), nil
}

// AccountByID reads one account.
func (a IdentityStore) AccountByID(ctx context.Context, id string) (identity.Account, error) {
	acc, err := a.s.GetAccount(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return identity.Account{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Account{}, err
	}
	return toIdentityAccount(acc), nil
}

// ListAccounts reads every account, newest first.
func (a IdentityStore) ListAccounts(ctx context.Context) ([]identity.Account, error) {
	rows, err := a.s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]identity.Account, 0, len(rows))
	for _, row := range rows {
		out = append(out, toIdentityAccount(row))
	}
	return out, nil
}

// MarkVerified stamps the address confirmed.
func (a IdentityStore) MarkVerified(ctx context.Context, id string) error {
	if err := a.s.MarkEmailVerified(ctx, id); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// SetPassword writes a new hash and revokes every live session and link with it.
func (a IdentityStore) SetPassword(ctx context.Context, id, passwordHash string) error {
	if err := a.s.SetPassword(ctx, id, passwordHash); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// SetDisabled locks an account out or lets it back in.
func (a IdentityStore) SetDisabled(ctx context.Context, id string, disabled bool) error {
	if err := a.s.SetAccountDisabled(ctx, id, disabled); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// CreateSession records a sign in, returning the row's id.
func (a IdentityStore) CreateSession(ctx context.Context, accountID, tokenHash string, expiresAt time.Time) (string, error) {
	sess, err := a.s.CreateSession(ctx, accountID, tokenHash, ids.Stamp(expiresAt))
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// RevokeSession kills one session.
func (a IdentityStore) RevokeSession(ctx context.Context, id string) error {
	if err := a.s.RevokeSession(ctx, id); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// CreateLink mints a single use link, superseding the account's live one for
// that purpose.
func (a IdentityStore) CreateLink(ctx context.Context, accountID, purpose, tokenHash string, expiresAt time.Time) error {
	_, err := a.s.CreateEmailToken(ctx, accountID, purpose, tokenHash, ids.Stamp(expiresAt))
	return err
}

// ConsumeLink spends a link, matched on its hash and its purpose together.
func (a IdentityStore) ConsumeLink(ctx context.Context, tokenHash, purpose string) (string, error) {
	tok, err := a.s.ConsumeEmailToken(ctx, tokenHash, purpose)
	if errors.Is(err, ErrLinkInvalid) {
		return "", identity.ErrLinkInvalid
	}
	if err != nil {
		return "", err
	}
	return tok.AccountID, nil
}

// CreateInvite mints one invite and returns its id.
func (a IdentityStore) CreateInvite(ctx context.Context, n identity.NewInvite) (string, error) {
	inv, err := a.s.CreateInvite(ctx, NewInvite{
		CodeHash:  n.CodeHash,
		Note:      n.Note,
		CreatedBy: n.CreatedBy,
		ExpiresAt: ids.Stamp(n.ExpiresAt),
	})
	if err != nil {
		return "", err
	}
	return inv.ID, nil
}

// ListInvites reads every invite, newest first. The hash on the row never
// crosses this boundary: the shape the identity package declared has no field
// for it, so one cannot leak by being forgotten.
func (a IdentityStore) ListInvites(ctx context.Context) ([]identity.InviteRow, error) {
	rows, err := a.s.ListInvites(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]identity.InviteRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, identity.InviteRow{
			ID:           r.ID,
			Note:         deref(r.Note),
			IssuerName:   deref(r.IssuerName),
			SpenderEmail: deref(r.SpenderEmail),
			ExpiresAt:    r.ExpiresAt,
			ConsumedAt:   deref(r.ConsumedAt),
			RevokedAt:    deref(r.RevokedAt),
			CreatedAt:    r.CreatedAt,
		})
	}
	return out, nil
}

// RevokeInvite pulls a live invite back. One already spent or expired is not
// found, because the guard carries the same live predicate the spend does.
func (a IdentityStore) RevokeInvite(ctx context.Context, id string) error {
	if err := a.s.RevokeInvite(ctx, id); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// LiveInvite resolves a code hash to the invite it names, and only while that
// invite is live.
func (a IdentityStore) LiveInvite(ctx context.Context, codeHash string) (string, error) {
	inv, err := a.s.LiveInvite(ctx, codeHash)
	if errors.Is(err, ErrInviteInvalid) {
		return "", identity.ErrInviteInvalid
	}
	if err != nil {
		return "", err
	}
	return inv.ID, nil
}

// SpendInviteAndCreateAccount stamps the invite spent and writes the account it
// created, in one transaction. The account id is drawn here, before the
// transaction opens, so the guarded update and the insert can name each other.
func (a IdentityStore) SpendInviteAndCreateAccount(ctx context.Context, inviteID string,
	n identity.NewAccount,
) (identity.Account, error) {
	accountID := ids.New(ids.Account, a.s.clock.Now())
	acc, err := a.s.SpendInviteAndCreateAccount(ctx, inviteID, accountID, NewIdentityAccount{
		Email:        n.Email,
		PasswordHash: n.PasswordHash,
		DisplayName:  n.DisplayName,
	})
	switch {
	case errors.Is(err, ErrInviteInvalid):
		return identity.Account{}, identity.ErrInviteInvalid
	case errors.Is(err, ErrEmailTaken):
		return identity.Account{}, identity.ErrEmailTaken
	case err != nil:
		return identity.Account{}, err
	}
	return toIdentityAccount(acc), nil
}

// AnyAccountHasEmail reports whether a person has ever registered.
func (a IdentityStore) AnyAccountHasEmail(ctx context.Context) (bool, error) {
	return a.s.AnyAccountHasEmail(ctx)
}

// AnyLiveBootstrapInvite reports whether the platform already minted itself a
// way in that nobody has spent.
func (a IdentityStore) AnyLiveBootstrapInvite(ctx context.Context) (bool, error) {
	return a.s.AnyLiveBootstrapInvite(ctx)
}

// MintToken stores an API token as its hash plus a non secret prefix. A name the
// account already holds live loses on the partial unique index, which is the only
// way that is detected.
func (a IdentityStore) MintToken(ctx context.Context, accountID, name, tokenHash, prefix string, expiresAt time.Time) (identity.TokenView, error) {
	n := NewToken{AccountID: accountID, Name: name, TokenHash: tokenHash, Prefix: prefix}
	if !expiresAt.IsZero() {
		n.ExpiresAt = ids.Stamp(expiresAt)
	}
	tok, err := a.s.CreateAPIToken(ctx, n)
	if isUniqueViolation(err) {
		return identity.TokenView{}, identity.ErrTokenNameTaken
	}
	if err != nil {
		return identity.TokenView{}, err
	}
	return toTokenView(tok), nil
}

// ListTokens reads one account's live tokens, newest first.
func (a IdentityStore) ListTokens(ctx context.Context, accountID string) ([]identity.TokenView, error) {
	rows, err := a.s.ListLiveAPITokens(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]identity.TokenView, 0, len(rows))
	for _, row := range rows {
		out = append(out, toTokenView(row))
	}
	return out, nil
}

// TokenByID reads one token row so a caller can be checked against its owner.
func (a IdentityStore) TokenByID(ctx context.Context, id string) (identity.TokenView, error) {
	tok, err := a.s.GetAPIToken(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return identity.TokenView{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.TokenView{}, err
	}
	return toTokenView(tok), nil
}

// RevokeToken kills one token.
func (a IdentityStore) RevokeToken(ctx context.Context, id string) error {
	if err := a.s.RevokeAPIToken(ctx, id); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// toIdentityAccount projects a row onto what the identity package reads. The
// nullable columns become the plain values that package's rules are written
// against: a null email is an empty string, which is exactly what exempts the
// bootstrap account from the verified gate.
func toIdentityAccount(acc Account) identity.Account {
	return identity.Account{
		ID:           acc.ID,
		Email:        deref(acc.Email),
		DisplayName:  deref(acc.DisplayName),
		PasswordHash: deref(acc.PasswordHash),
		Verified:     acc.EmailVerifiedAt != nil,
		Disabled:     acc.DisabledAt != nil,
		IsAdmin:      acc.IsAdmin == 1,
		CreatedAt:    acc.CreatedAt,
	}
}

// toTokenView projects a token row onto what a caller may see: never the hash.
func toTokenView(tok APIToken) identity.TokenView {
	return identity.TokenView{
		ID:         tok.ID,
		AccountID:  tok.AccountID,
		Name:       tok.Name,
		Prefix:     tok.TokenPrefix,
		CreatedAt:  tok.CreatedAt,
		LastUsedAt: deref(tok.LastUsedAt),
		ExpiresAt:  deref(tok.ExpiresAt),
	}
}

// mapNotFound turns this package's absence sentinel into the identity package's.
func mapNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return identity.ErrNotFound
	}
	return err
}
