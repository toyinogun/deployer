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

// The two names the session id travels under. Which one is live is decided by
// whether the platform is served over HTTPS, exactly as the pre authentication
// CSRF cookie already decides it (spec 0021, AC-19).
const (
	// SessionCookieSecure is the name in production. The `__Host-` prefix is
	// part of the protection, not decoration: a browser refuses such a cookie
	// from any page that sets a Domain attribute, so an app deployed by a
	// stranger on a sibling hostname under the platform's wildcard can neither
	// read this cookie nor shadow it with one of its own. Without it, setting a
	// parent scoped cookie of the same name is session fixation with a deploy as
	// the delivery mechanism.
	SessionCookieSecure = "__Host-deployer_session"
	// SessionCookiePlain is the name over plain HTTP, where a browser refuses a
	// Secure cookie outright and keeping the prefix would make signing in locally
	// impossible. A plain HTTP deployment therefore loses the sibling subdomain
	// guarantee above. The cluster is served over HTTPS, so this name is never
	// reached in production.
	SessionCookiePlain = "deployer_session"
)

// SessionCookieName is the name the session id travels under, given whether the
// platform is served over HTTPS. Both surfaces read and write through it, so a
// cookie written one way and cleared another is not a session that outlives its
// sign out.
func SessionCookieName(secure bool) string {
	if secure {
		return SessionCookieSecure
	}
	return SessionCookiePlain
}

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

// IsSessionCookie reports whether name is either name the session id travels
// under. It exists for the callers that inspect a Set-Cookie without holding the
// platform's scheme, which is every test that checks a sign in handed one back.
func IsSessionCookie(name string) bool {
	return name == SessionCookieSecure || name == SessionCookiePlain
}

// SessionID pulls the raw session id out of a request's cookies. It returns an
// empty string when there is none, which AuthenticateSession treats as invalid.
//
// Both names are tried, secure first, because the caller here does not hold the
// platform's scheme and reading the wrong one would sign everybody out on every
// request rather than once. Only one of the two is ever written, so there is no
// ambiguity to resolve: whichever arrives is the one this platform set.
func SessionID(r *http.Request) string {
	for _, name := range []string{SessionCookieSecure, SessionCookiePlain} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
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
