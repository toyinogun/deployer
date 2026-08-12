package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// The row types spec 0007 adds, named here so callers never import the generated
// package.
type (
	// Session is one browser sign in, stored as a hash exactly as a token is.
	Session = sqlcgen.Session
	// EmailToken is one single use link, either a verification or a reset.
	EmailToken = sqlcgen.EmailToken
)

// The two purposes an email link can carry. They are the CHECK constraint's
// values, so a typo fails the insert rather than creating a link nothing matches.
const (
	// PurposeVerifyEmail is the link that stamps email_verified_at.
	PurposeVerifyEmail = "verify_email"
	// PurposePasswordReset is the link that sets a new password hash.
	PurposePasswordReset = "password_reset"
)

// NewIdentityAccount describes a person registering. The raw password never
// reaches the store: the caller hashes it and passes the encoded result.
type NewIdentityAccount struct {
	Email        string
	PasswordHash string
	DisplayName  string
}

// CreateIdentityAccount registers a person. The first admin rule is computed
// inside the same transaction as the insert, so two concurrent first
// registrations cannot both come out admin (AC-4).
//
// A second registration of the same address loses on the partial unique index
// and comes back as ErrEmailTaken. That is the only way this is detected: a read
// before the write would leave a race the database has already closed.
func (s *Store) CreateIdentityAccount(ctx context.Context, n NewIdentityAccount) (Account, error) {
	now := s.now()
	id := ids.New(ids.Account, s.clock.Now())

	var acc Account
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		existing, err := q.CountEmailAccounts(ctx)
		if err != nil {
			return fmt.Errorf("store: counting registered accounts: %w", err)
		}
		var isAdmin int64
		if existing == 0 {
			isAdmin = 1
		}
		acc, err = q.CreateIdentityAccount(ctx, sqlcgen.CreateIdentityAccountParams{
			ID:           id,
			Email:        ptr(n.Email),
			PasswordHash: ptr(n.PasswordHash),
			DisplayName:  ptr(n.DisplayName),
			IsAdmin:      isAdmin,
			Now:          now,
		})
		if isUniqueViolation(err) {
			return ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("store: creating an account: %w", err)
		}
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	return acc, nil
}

// GetAccountByEmail reads one account by its address.
func (s *Store) GetAccountByEmail(ctx context.Context, email string) (Account, error) {
	acc, err := s.q.GetAccountByEmail(ctx, ptr(email))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		// The address is not in the message: an error string is one more place a
		// caller's email could end up in a log.
		return Account{}, fmt.Errorf("store: reading an account by address: %w", err)
	}
	return acc, nil
}

// ListAccounts reads every account, newest first. No pagination: this is the
// admin view of a homelab platform (AC-19).
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	accs, err := s.q.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing accounts: %w", err)
	}
	return accs, nil
}

// MarkEmailVerified stamps the address verified. Verifying an already verified
// account is ErrNotFound, which is what makes a link single use even if two
// requests carry it at once.
func (s *Store) MarkEmailVerified(ctx context.Context, id string) error {
	n, err := s.q.MarkEmailVerified(ctx, sqlcgen.MarkEmailVerifiedParams{Now: ptr(s.now()), ID: id})
	if err != nil {
		return fmt.Errorf("store: verifying account %s: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPassword writes a new password hash and, in the same transaction, revokes
// every live session and every live link the account holds (AC-10, AC-29).
func (s *Store) SetPassword(ctx context.Context, id, passwordHash string) error {
	now := s.now()
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.SetPasswordHash(ctx, sqlcgen.SetPasswordHashParams{
			PasswordHash: ptr(passwordHash), Now: now, ID: id,
		})
		if err != nil {
			return fmt.Errorf("store: setting the password on %s: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		if _, err := q.RevokeAccountSessions(ctx, sqlcgen.RevokeAccountSessionsParams{
			Now: ptr(now), AccountID: id,
		}); err != nil {
			return fmt.Errorf("store: revoking sessions on %s: %w", id, err)
		}
		if _, err := q.RevokeAccountEmailTokens(ctx, sqlcgen.RevokeAccountEmailTokensParams{
			Now: ptr(now), AccountID: id,
		}); err != nil {
			return fmt.Errorf("store: revoking links on %s: %w", id, err)
		}
		return nil
	})
}

// SetAccountDisabled locks an account out or lets it back in. Disabling revokes
// every live session and link in the same transaction, so the change takes effect
// on the very next request rather than whenever a session happens to expire.
func (s *Store) SetAccountDisabled(ctx context.Context, id string, disabled bool) error {
	now := s.now()
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		var stamp *string
		if disabled {
			stamp = ptr(now)
		}
		n, err := q.SetAccountDisabled(ctx, sqlcgen.SetAccountDisabledParams{
			DisabledAt: stamp, Now: now, ID: id,
		})
		if err != nil {
			return fmt.Errorf("store: setting the disabled state on %s: %w", id, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		if !disabled {
			return nil
		}
		if _, err := q.RevokeAccountSessions(ctx, sqlcgen.RevokeAccountSessionsParams{
			Now: ptr(now), AccountID: id,
		}); err != nil {
			return fmt.Errorf("store: revoking sessions on %s: %w", id, err)
		}
		if _, err := q.RevokeAccountEmailTokens(ctx, sqlcgen.RevokeAccountEmailTokensParams{
			Now: ptr(now), AccountID: id,
		}); err != nil {
			return fmt.Errorf("store: revoking links on %s: %w", id, err)
		}
		return nil
	})
}

// CreateSession records a sign in. The raw cookie value never reaches here: the
// caller hashes it and hands back the plaintext exactly once, in the Set-Cookie.
func (s *Store) CreateSession(ctx context.Context, accountID, tokenHash, expiresAt string) (Session, error) {
	sess, err := s.q.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID:        ids.New(ids.Session, s.clock.Now()),
		AccountID: accountID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
		Now:       s.now(),
	})
	if err != nil {
		return Session{}, fmt.Errorf("store: creating a session for %s: %w", accountID, err)
	}
	return sess, nil
}

// ResolveSession turns a session hash into the account it belongs to. Unknown,
// revoked, expired, and belonging to a disabled account are all the same
// ErrSessionInvalid, so a caller learns nothing from which one it hit.
func (s *Store) ResolveSession(ctx context.Context, tokenHash string) (Account, Session, error) {
	row, err := s.q.ResolveSession(ctx, sqlcgen.ResolveSessionParams{
		TokenHash: tokenHash,
		Now:       s.now(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, Session{}, ErrSessionInvalid
	}
	if err != nil {
		return Account{}, Session{}, fmt.Errorf("store: resolving a session: %w", err)
	}
	return row.Account, row.Session, nil
}

// TouchSession records a use and pushes the rolling expiry forward (AC-9).
func (s *Store) TouchSession(ctx context.Context, id, expiresAt string) error {
	now := s.now()
	err := s.q.TouchSession(ctx, sqlcgen.TouchSessionParams{
		Now: ptr(now), ExpiresAt: expiresAt, ID: id,
	})
	if err != nil {
		return fmt.Errorf("store: touching session %s: %w", id, err)
	}
	return nil
}

// RevokeSession kills one session. Revoking an already revoked one is not found.
func (s *Store) RevokeSession(ctx context.Context, id string) error {
	n, err := s.q.RevokeSession(ctx, sqlcgen.RevokeSessionParams{Now: ptr(s.now()), ID: id})
	if err != nil {
		return fmt.Errorf("store: revoking session %s: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateEmailToken mints a single use link, superseding whatever live link the
// account already holds for that purpose. At most one is live at a time, which is
// what makes a resend replace rather than add (AC-6).
func (s *Store) CreateEmailToken(ctx context.Context, accountID, purpose, tokenHash, expiresAt string) (EmailToken, error) {
	now := s.now()
	var tok EmailToken
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.ConsumeLiveEmailTokens(ctx, sqlcgen.ConsumeLiveEmailTokensParams{
			Now: ptr(now), AccountID: accountID, Purpose: purpose,
		}); err != nil {
			return fmt.Errorf("store: superseding the live %s link: %w", purpose, err)
		}
		var err error
		tok, err = q.CreateEmailToken(ctx, sqlcgen.CreateEmailTokenParams{
			ID:        ids.New(ids.EmailToken, s.clock.Now()),
			AccountID: accountID,
			Purpose:   purpose,
			TokenHash: tokenHash,
			ExpiresAt: expiresAt,
			Now:       now,
		})
		if err != nil {
			return fmt.Errorf("store: creating a %s link: %w", purpose, err)
		}
		return nil
	})
	if err != nil {
		return EmailToken{}, err
	}
	return tok, nil
}

// ConsumeEmailToken spends a link, matched on its hash and its purpose together.
// Unknown, already consumed, expired, and minted for the other purpose are all
// the same ErrLinkInvalid, in the same words (AC-5).
//
// The lookup and the consumption share a transaction, so a link presented twice
// at once is spent once.
func (s *Store) ConsumeEmailToken(ctx context.Context, tokenHash, purpose string) (EmailToken, error) {
	now := s.now()
	var tok EmailToken
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		tok, err = q.GetLiveEmailToken(ctx, sqlcgen.GetLiveEmailTokenParams{
			TokenHash: tokenHash, Purpose: purpose, Now: now,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLinkInvalid
		}
		if err != nil {
			return fmt.Errorf("store: reading a link: %w", err)
		}
		n, err := q.ConsumeEmailToken(ctx, sqlcgen.ConsumeEmailTokenParams{Now: ptr(now), ID: tok.ID})
		if err != nil {
			return fmt.Errorf("store: consuming link %s: %w", tok.ID, err)
		}
		if n == 0 {
			return ErrLinkInvalid
		}
		return nil
	})
	if err != nil {
		return EmailToken{}, err
	}
	return tok, nil
}

// ListLiveAPITokens reads one account's live tokens, newest first. It never
// returns a hash, because it never reads one back out to a caller: the row type
// carries it and the layer above projects it away.
func (s *Store) ListLiveAPITokens(ctx context.Context, accountID string) ([]APIToken, error) {
	toks, err := s.q.ListLiveAPITokens(ctx, sqlcgen.ListLiveAPITokensParams{
		AccountID: accountID, Now: ptr(s.now()),
	})
	if err != nil {
		return nil, fmt.Errorf("store: listing tokens for %s: %w", accountID, err)
	}
	return toks, nil
}

// GetAPIToken reads one token row, so a caller can check who owns it before
// acting on it.
func (s *Store) GetAPIToken(ctx context.Context, id string) (APIToken, error) {
	tok, err := s.q.GetAPIToken(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrNotFound
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("store: reading token %s: %w", id, err)
	}
	return tok, nil
}
