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

	// The configuration actions, added by spec 0010 (AC-12). A write records one
	// row per key it changed; a read records only a refusal.

	// ActionConfigSet is one key written by set_config or by deploy_app's
	// optional config map.
	ActionConfigSet = "config_set"
	// ActionConfigUnset is one key removed by unset_config.
	ActionConfigUnset = "config_unset"
	// ActionConfigGet is one get_config call, recorded only when it is refused.
	ActionConfigGet = "config_get"

	// The release actions, added by spec 0011 (AC-20).

	// ActionRollback is one rollback_app call. Both outcomes are recorded, not
	// only refusals: a rollback replaced what was running, which is worth a row
	// even when it was allowed.
	ActionRollback = "rollback"
	// ActionReleases is one list_releases call, recorded only when it is
	// refused. Like ActionStatus, a successful read is not an access decision.
	ActionReleases = "releases"

	// The app lifecycle actions, added by spec 0012 (AC-11, AC-29).

	// ActionAppList is one list_apps call, recorded only when it is refused.
	// Like ActionStatus, a successful read is not an access decision.
	ActionAppList = "app_list"
	// ActionAppDelete is one delete_app call. Both outcomes are recorded: a
	// delete removed an app and everything it was serving, which is worth a row
	// even when it was allowed.
	ActionAppDelete = "app_delete"

	// The page actions, added by spec 0013 (AC-12, AC-15, AC-20).

	// ActionPageCSRF is one page POST refused because its synchroniser token was
	// missing or wrong, or because its origin was not this platform's. Only the
	// refusal is recorded: a form that verified is not an access decision.
	ActionPageCSRF = "page_csrf"
	// ActionConfigReveal is one browser reveal of a configuration value that is
	// not flagged secret. Both outcomes are recorded, because a reveal read a
	// value back out of the platform, which is worth a row even when allowed.
	ActionConfigReveal = "config_reveal"
	// ActionAppView is one page read of an app the caller does not own, recorded
	// only when refused. A read of your own app is not an access decision.
	ActionAppView = "app_view"
)

// TargetAppConfig is the target type a configuration change is recorded against.
// The table carries one target pair, so the app id and the key travel joined by a
// slash in TargetID and both survive (spec 0010, AC-12).
const TargetAppConfig = "app_config"

// Audit is one authorization outcome. AccountID is empty when the presented
// token resolved to nothing, which is exactly the denial worth recording.
type Audit struct {
	AccountID  string
	Action     string
	TargetType string
	TargetID   string
	Allowed    bool
	Reason     string
	// ClientAddress is the visitor the action was attributed to, from
	// ClientAddress on the request that caused it. Empty on a row the platform
	// wrote itself, which is written as null: a suspension sweep, a reconcile
	// drive and a scheduled backup run have no visitor to record (spec 0021,
	// AC-17). It is a new field rather than a new parameter so every current call
	// site still compiles, and each one that has a request sets it where it
	// builds this struct.
	ClientAddress string
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

// Lockout is the growing penalty a run of bad credentials from one address
// earns. internal/identity's limiter satisfies it, keyed here on the visitor's
// network address rather than on an email: a bearer token names no account until
// it resolves, so there is nothing else to key on (spec 0022, AC-16).
type Lockout interface {
	// LockedOut reports whether key is inside its penalty window.
	LockedOut(key string) (time.Duration, bool)
	// Failed records one bad credential, extending the window.
	Failed(key string)
	// Succeeded clears the run, so one good token undoes the backoff.
	Succeeded(key string)
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
	// The bad token penalty, added by spec 0022. Nil until WithLockout is
	// called, which leaves a build with no public deploy path unbounded exactly
	// as it was.
	lockout Lockout
}

// NewAuthenticator returns an authenticator over the given store.
func NewAuthenticator(s Store, t TokenToucher) *Authenticator {
	return &Authenticator{store: s, toucher: t}
}

// WithLockout adds the penalty a run of bad bearer tokens from one address
// earns. It goes here rather than into either handler, so both routes on the
// deploy path inherit exactly one rule and a third surface cannot be written
// without it (spec 0022, AC-16).
func (a *Authenticator) WithLockout(l Lockout) *Authenticator {
	a.lockout = l
	return a
}

// Authenticate resolves a raw bearer token to its account, ErrTokenInvalid,
// ErrAccountSuspended, or ErrTooManyAttempts.
//
// Unknown, revoked, expired, and held by an account that never confirmed its
// address are all the same error on purpose: a caller learns only that the token
// does not work. A good token on a suspended account is the one case that is
// told apart, so the surface above can answer account_suspended (spec 0018,
// AC-12). The token's last use is recorded on success, and a failure to record
// it is logged rather than allowed to refuse a caller who presented a good
// token.
//
// clientAddr is the visitor's derived address, from ClientAddress. A run of bad
// tokens from one address earns a growing penalty here, which is why the rule is
// not in either handler: two routes call this and a rule only one of them applies
// is not a rule. An empty address is never penalised, for the same reason the
// token bucket lets one through: a caller the platform cannot tell apart from
// another is not a key worth holding (spec 0022, AC-16).
func (a *Authenticator) Authenticate(ctx context.Context, raw, clientAddr string) (Account, error) {
	penalised := a.lockout != nil && clientAddr != ""
	if penalised {
		if _, locked := a.lockout.LockedOut(clientAddr); locked {
			return Account{}, ErrTooManyAttempts
		}
	}
	// fail records one bad credential against the address before handing the
	// refusal back, so every path out of this function that is a refusal counts,
	// and none of them can be counted twice by a caller.
	fail := func(err error) (Account, error) {
		if penalised {
			a.lockout.Failed(clientAddr)
		}
		return Account{}, err
	}
	if raw == "" {
		return fail(ErrTokenInvalid)
	}
	account, token, err := a.store.ResolveToken(ctx, HashToken(raw))
	if err != nil {
		if errors.Is(err, ErrTokenInvalid) {
			return fail(ErrTokenInvalid)
		}
		// A fault inside the platform is not a wrong credential, so it earns no
		// penalty: an unreachable database must not lock every caller out.
		return Account{}, fmt.Errorf("auth: resolving the presented token: %w", err)
	}
	// The suspension gate, on the bearer route. The store no longer filters a
	// suspended account out of the resolve, so this is the only place the bearer
	// route decides it, and it decides it as its own outcome rather than folding
	// it into the invalid credential answer (spec 0018, AC-12).
	//
	// The account is returned alongside the error, which nothing else here does:
	// a surface refusing a suspended caller still has to audit which account it
	// refused, and the caller already proved it holds that account's credential.
	// Every caller branches on the error first, so the account is only ever read
	// by one that meant to.
	//
	// It is not a bad credential either, so it earns no penalty: the caller
	// holds a working token and is being refused for a reason it cannot guess
	// its way past.
	if account.Disabled {
		return account, ErrAccountSuspended
	}
	// The verified gate, on the bearer route. A token held by an account that
	// never confirmed its address is refused with the same answer an invalid token
	// gets, on every surface, MCP and upload alike (AC-16). The gate must be
	// readable in one place rather than split across two layers.
	if !account.usable() {
		return fail(ErrTokenInvalid)
	}
	if penalised {
		a.lockout.Succeeded(clientAddr)
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
