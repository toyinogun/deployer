package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// ErrSessionInvalid covers unknown, revoked and expired sessions, and sessions
// belonging to a disabled account. Deliberately indistinguishable, exactly as
// ErrTokenInvalid is.
var ErrSessionInvalid = errors.New("auth: session invalid")

// ErrEmailUnverified is a live session whose account no longer holds a confirmed
// address. Separate from ErrSessionInvalid because the caller already proved they
// hold this session, so naming the reason reveals nothing they could not learn by
// signing in, and it is the only answer they can act on (AC-15).
//
// The bearer route has no equivalent: a machine holding a token learns only that
// the token does not work (AC-16).
var ErrEmailUnverified = errors.New("auth: email unverified")

// SessionCookie is the name the session id travels under.
const SessionCookie = "deployer_session"

// Session is one browser sign in, carrying only what this package reads.
type Session struct {
	ID        string
	AccountID string
}

// SessionStore is the session half of persistence. It is separate from Store so
// a platform with no session surface, which is every build before spec 0007,
// needs no implementation of it.
type SessionStore interface {
	// ResolveSession turns a session hash into the account it belongs to, or
	// ErrSessionInvalid. Unknown, revoked, expired and disabled are the same error.
	ResolveSession(ctx context.Context, tokenHash string) (Account, Session, error)
	// TouchSession records a use and pushes the rolling expiry forward by
	// lifetime. The new expiry is computed by the store rather than passed in,
	// because the store owns the clock every timestamp comes from.
	TouchSession(ctx context.Context, id string, lifetime time.Duration) error
}

// WithSessions gives an authenticator its session route. lifetime is how far
// forward each use pushes the expiry.
func (a *Authenticator) WithSessions(s SessionStore, lifetime time.Duration) *Authenticator {
	a.sessions = s
	a.sessionLifetime = lifetime
	return a
}

// AuthenticateSession resolves a raw session id to its account, or
// ErrSessionInvalid, or ErrEmailUnverified.
//
// It is the session half of the one resolution path: the verified gate and the
// disabled check are applied here, in the same function, for the same reason
// they are applied to a bearer token. A new surface cannot forget them because
// there is nowhere else to resolve a caller.
func (a *Authenticator) AuthenticateSession(ctx context.Context, raw string) (Account, Session, error) {
	if raw == "" || a.sessions == nil {
		return Account{}, Session{}, ErrSessionInvalid
	}
	account, session, err := a.sessions.ResolveSession(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			return Account{}, Session{}, ErrSessionInvalid
		}
		return Account{}, Session{}, fmt.Errorf("auth: resolving the presented session: %w", err)
	}
	// A session belongs to a person, and a person who never confirmed their
	// address holds no usable credential anywhere (AC-16). Refused either way; the
	// two part company only in what the person is told (AC-15).
	if account.Disabled {
		return Account{}, Session{}, ErrSessionInvalid
	}
	if !account.usable() {
		return Account{}, Session{}, ErrEmailUnverified
	}
	// The rolling expiry. A failure to push it forward is logged rather than
	// allowed to refuse a caller who presented a good session.
	if err := a.sessions.TouchSession(ctx, session.ID, a.sessionLifetime); err != nil {
		slog.WarnContext(ctx, "recording session use failed", "error", err, "session", session.ID)
	}
	return account, session, nil
}

// SessionID pulls the raw session id out of a request's cookies. It returns an
// empty string when there is none, which AuthenticateSession treats as invalid.
func SessionID(r *http.Request) string {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// usable reports whether an account may authenticate at all.
//
// This is the verified gate, in one place, applied to both routes. An account
// holding an address it never confirmed authenticates nowhere. An account with no
// address is exempt, which is precisely the bootstrap account: it was never a
// person, so there is nothing for it to confirm (AC-16).
func (a Account) usable() bool {
	if a.Disabled {
		return false
	}
	return a.Email == "" || a.Verified
}
