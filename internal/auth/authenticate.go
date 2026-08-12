package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// The audit actions this platform records. They are constants rather than free
// strings because the audit log is only useful if the same event is named the
// same way every time.
const (
	// ActionUpload is one call to the upload endpoint.
	ActionUpload = "upload"
	// ActionDeploy is one deploy_app call.
	ActionDeploy = "deploy"
	// ActionFetchSource is one build fetching the source it will unpack.
	ActionFetchSource = "fetch_source"
	// ActionStatus is one deployment_status call. Only a denied one is recorded:
	// a polling read audited every time fills a table with no signal in it
	// (spec 0005, AC-10).
	ActionStatus = "status"
	// ActionLogs is one get_logs call. Like ActionStatus, only a refusal is
	// recorded: a successful read is not an access decision, and neither is an
	// internal fault (spec 0006, AC-9).
	ActionLogs = "logs"

	// The identity actions, added by spec 0007 (AC-22).

	// ActionRegister is one registration attempt. It never names an account, on
	// purpose: naming one would record whether the address was already taken,
	// which is the one thing that endpoint is built not to reveal.
	ActionRegister = "register"
	// ActionLogin is one sign in. Unlike a read, every failure is recorded: a
	// failed sign in is an access decision and the run of them is the signal.
	ActionLogin = "login"
	// ActionLogout is one deliberate session revocation.
	ActionLogout = "logout"
	// ActionTokenMint is one API token created.
	ActionTokenMint = "token_mint"
	// ActionTokenRevoke is one API token killed by the account that holds it.
	ActionTokenRevoke = "token_revoke"
	// ActionAdmin is any use of the admin surface, allowed or refused. Reason
	// carries which one, so the closed action set does not grow per endpoint.
	ActionAdmin = "admin"
)

// Audit is one authorization outcome. AccountID is empty when the presented
// token resolved to nothing, which is exactly the denial worth recording.
type Audit struct {
	AccountID  string
	Action     string
	TargetType string
	TargetID   string
	Allowed    bool
	Reason     string
}

// Auditor records authorization outcomes. A failure to audit is logged and
// never replaces the outcome the caller was reporting.
type Auditor interface {
	RecordAudit(ctx context.Context, a Audit) error
}

// TokenToucher records that a token was just used. Separate from Store so the
// touch can fail without the authentication failing with it.
type TokenToucher interface {
	TouchToken(ctx context.Context, tokenID string) error
}

// Authenticator turns a presented bearer token into an account. It is the only
// thing in the platform that sees a raw token, and it never keeps or logs one.
type Authenticator struct {
	store   Store
	toucher TokenToucher
	// The session route, added by spec 0007. Nil until WithSessions is called,
	// which is what lets a build with no session surface leave it out.
	sessions        SessionStore
	sessionLifetime time.Duration
}

// NewAuthenticator returns an authenticator over the given store.
func NewAuthenticator(s Store, t TokenToucher) *Authenticator {
	return &Authenticator{store: s, toucher: t}
}

// Authenticate resolves a raw bearer token to its account, or ErrTokenInvalid.
//
// Unknown, revoked, expired, and belonging to a disabled account are all the
// same error on purpose: a caller learns only that the token does not work.
// The token's last use is recorded on success, and a failure to record it is
// logged rather than allowed to refuse a caller who presented a good token.
func (a *Authenticator) Authenticate(ctx context.Context, raw string) (Account, error) {
	if raw == "" {
		return Account{}, ErrTokenInvalid
	}
	account, token, err := a.store.ResolveToken(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, ErrTokenInvalid) {
			return Account{}, ErrTokenInvalid
		}
		return Account{}, fmt.Errorf("auth: resolving the presented token: %w", err)
	}
	// The verified gate, on the bearer route. A token held by an account that
	// never confirmed its address is refused with the same answer an invalid token
	// gets, on every surface, MCP and upload alike (AC-16). The store already
	// filtered disabled accounts out; this holds the rule even so, because the
	// gate must be readable in one place rather than split across two layers.
	if !account.usable() {
		return Account{}, ErrTokenInvalid
	}
	if a.toucher != nil {
		if err := a.toucher.TouchToken(ctx, token.ID); err != nil {
			slog.WarnContext(ctx, "recording token use failed", "error", err, "token", token.ID)
		}
	}
	return account, nil
}

// BearerToken pulls the raw token out of an Authorization header value. It
// returns an empty string for anything that is not a bearer credential, which
// Authenticate treats as invalid, so a malformed header and a wrong token are
// indistinguishable to the caller.
func BearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// Record writes one audit row, logging rather than returning a failure. Auditing
// must never change what the caller is told, so this returns nothing.
func Record(ctx context.Context, auditor Auditor, a Audit) {
	if auditor == nil {
		return
	}
	if err := auditor.RecordAudit(ctx, a); err != nil {
		slog.ErrorContext(ctx, "recording an audit row failed",
			"error", err, "action", a.Action, "allowed", a.Allowed)
	}
}
