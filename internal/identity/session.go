package identity

import (
	"context"
	"errors"
)

// SignedIn is what a successful sign in hands back: the raw session id, which
// exists only long enough to become a Set-Cookie, and the account behind it.
type SignedIn struct {
	// Raw is the cookie value. It is never stored, logged, or returned again.
	Raw     string
	Account Account
}

// Login checks a password and opens a session.
//
// A wrong password, an unknown address, and a disabled account are the same
// credentials_invalid, and all three cost a full password hash, so none of them
// is measurably faster than the others. The one deliberate exception is a
// registered but unverified account, which is told so, because a person who never
// received the mail has to be sent to resend rather than to password reset
// (AC-8).
func (s *Service) Login(ctx context.Context, rawEmail, password string) (SignedIn, error) {
	email := NormalizeEmail(rawEmail)

	// The lockout lives here rather than in a handler because both surfaces call
	// this one method, and a lockout only one of them applies is not a lockout
	// (spec 0021, AC-5). It was in the JSON handler alone until 2026-08-16, which
	// left the browser, the surface the public edge exposes, bounded only by the
	// per address bucket while the JSON surface it was measured against is
	// reachable on the tailnet only.
	//
	// Checked before any work, so a locked out address costs neither a database
	// read nor a password hash.
	if _, locked := s.limits.LockedOut(email); locked {
		return SignedIn{}, Fail(CodeRateLimited, "too many failed sign ins, wait a moment")
	}

	account, err := s.store.AccountByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		// No account, but still spend the hash: an unknown address must not be
		// the fast path (AC-8).
		s.hasher.Verify(password, "")
		s.limits.Failed(email)
		return SignedIn{}, credentialsInvalid()
	case err != nil:
		// An internal fault is not the caller's failed attempt, so it must not
		// feed the backoff.
		return SignedIn{}, err
	}

	// The bootstrap account holds no password hash, so Verify spends the work and
	// answers false. It is refused exactly as a wrong password is (AC-11).
	if !s.hasher.Verify(password, account.PasswordHash) || account.Disabled {
		s.limits.Failed(email)
		return SignedIn{}, credentialsInvalid()
	}
	if account.Email == "" {
		s.limits.Failed(email)
		return SignedIn{}, credentialsInvalid()
	}
	if !account.Verified {
		// A real account presenting the right password, so it is not a failed
		// attempt and must not feed the backoff.
		return SignedIn{}, Fail(CodeEmailUnverified,
			"confirm your email address before signing in")
	}

	raw, err := NewSecret()
	if err != nil {
		return SignedIn{}, err
	}
	if _, err := s.store.CreateSession(ctx, account.ID, HashSecret(raw), s.clock.Now().Add(SessionLifetime)); err != nil {
		return SignedIn{}, err
	}
	// A completed sign in clears the address's failures, so an ordinary typo
	// before a correct password leaves no penalty behind.
	s.limits.Succeeded(email)
	return SignedIn{Raw: raw, Account: account}, nil
}

// Logout revokes one session. Revoking one that is already gone is not an error:
// the caller asked to be signed out and they are.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	if err := s.store.RevokeSession(ctx, sessionID); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// Forgot mails a password reset link. It always succeeds from the caller's side,
// whether or not the address exists, and takes comparable work either way
// (AC-28).
func (s *Service) Forgot(ctx context.Context, rawEmail string) error {
	if s.mailer == nil {
		return Fail(CodeMailUnavailable, "this platform has no mail sender configured")
	}
	email := NormalizeEmail(rawEmail)

	account, err := s.store.AccountByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case account.Disabled || account.Email == "":
		return nil
	}

	link, err := s.issueLink(ctx, account.ID, PurposeReset)
	if err != nil {
		return err
	}
	s.send(ctx, email, resetSubject, resetBody(link))
	return nil
}

// Reset spends a reset link and sets a new password, which revokes every live
// session and every live link the account holds in the same transaction (AC-29).
//
// A verification link presented here is link_invalid, the same answer an unknown
// one gets, because the lookup matches on the purpose as well as the hash.
func (s *Service) Reset(ctx context.Context, rawToken, password string) error {
	if err := CheckPassword(password); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return err
	}

	accountID, err := s.store.ConsumeLink(ctx, HashSecret(rawToken), PurposeReset)
	if errors.Is(err, ErrLinkInvalid) {
		return Fail(CodeLinkInvalid, "that link is not usable")
	}
	if err != nil {
		return err
	}
	return s.store.SetPassword(ctx, accountID, hash)
}

// credentialsInvalid is the single answer every sign in failure but one shares.
func credentialsInvalid() error {
	return Fail(CodeCredentialsInvalid, "that email address and password do not match an account")
}
