package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// maxIdentityBody caps a JSON request body on the identity surface. Every one of
// them is a handful of short fields, so anything larger is not a real caller.
const maxIdentityBody = 8 << 10

// Identity is the HTTP surface a person drives: registration, sessions, their own
// API tokens, and the admin view. Machines never reach it; they hold a bearer
// token and reach /mcp and /v1/uploads, which are unchanged.
type Identity struct {
	svc     *identity.Service
	auth    *auth.Authenticator
	auditor auth.Auditor
	// secure is the cookie's Secure flag, derived from the public address rather
	// than configured separately: a platform served over https must not hand out a
	// cookie a plain http request would carry.
	secure bool
	// hasMailer is whether a sender is configured. The endpoints that exist only
	// to send mail answer mail_unavailable when it is not.
	hasMailer bool
}

// NewIdentity returns the identity surface. publicURL decides the cookie's Secure
// flag; hasMailer decides whether the mail dependent endpoints work at all.
func NewIdentity(svc *identity.Service, a *auth.Authenticator, auditor auth.Auditor, publicURL string, hasMailer bool) *Identity {
	secure := false
	if u, err := url.Parse(publicURL); err == nil && u.Scheme == "https" {
		secure = true
	}
	return &Identity{svc: svc, auth: a, auditor: auditor, secure: secure, hasMailer: hasMailer}
}

// Register adds this package's identity routes to mux.
func (i *Identity) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/register", i.register)
	mux.HandleFunc("GET /v1/auth/verify", i.verify)
	mux.HandleFunc("POST /v1/auth/resend", i.resend)
	mux.HandleFunc("POST /v1/auth/login", i.login)
	mux.HandleFunc("POST /v1/auth/logout", i.logout)
	mux.HandleFunc("POST /v1/auth/forgot", i.forgot)
	mux.HandleFunc("POST /v1/auth/reset", i.reset)
	mux.HandleFunc("GET /v1/auth/me", i.me)

	mux.HandleFunc("POST /v1/tokens", i.mintToken)
	mux.HandleFunc("GET /v1/tokens", i.listTokens)
	mux.HandleFunc("DELETE /v1/tokens/{id}", i.revokeToken)

	mux.HandleFunc("GET /v1/admin/accounts", i.adminListAccounts)
	mux.HandleFunc("POST /v1/admin/accounts/{id}/disable", i.adminDisable)
	mux.HandleFunc("POST /v1/admin/accounts/{id}/enable", i.adminEnable)
	mux.HandleFunc("DELETE /v1/admin/accounts/{id}/tokens/{tokenId}", i.adminRevokeToken)

	mux.HandleFunc("GET /v1/admin/invites", i.adminListInvites)
	mux.HandleFunc("POST /v1/admin/invites", i.adminMintInvite)
	mux.HandleFunc("DELETE /v1/admin/invites/{id}", i.adminRevokeInvite)
}

// session resolves the caller's session cookie, answering 401 and returning false
// when there is not a live one. Every session backed handler starts here, so
// there is exactly one place a person is turned into an account.
func (i *Identity) session(w http.ResponseWriter, r *http.Request) (auth.Account, auth.Session, bool) {
	ctx := r.Context()
	account, sess, err := i.auth.AuthenticateSession(ctx, auth.SessionID(r))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEmailUnverified):
			writeCode(ctx, w, http.StatusForbidden, identity.CodeEmailUnverified,
				"confirm your email address before using this account")
		case errors.Is(err, auth.ErrSessionInvalid):
			writeCode(ctx, w, http.StatusUnauthorized, identity.CodeCredentialsInvalid, "sign in first")
		default:
			slog.ErrorContext(ctx, "resolving a session failed", "error", err)
			writeCode(ctx, w, http.StatusUnauthorized, identity.CodeCredentialsInvalid, "sign in first")
		}
		return auth.Account{}, auth.Session{}, false
	}
	return account, sess, true
}

// adminSession is session plus the admin check. An ordinary account is refused
// admin_required; a bearer token never gets this far, because this reads a cookie
// and a token is not one (AC-20).
func (i *Identity) adminSession(w http.ResponseWriter, r *http.Request) (auth.Account, bool) {
	account, _, ok := i.session(w, r)
	if !ok {
		return auth.Account{}, false
	}
	if !account.IsAdmin {
		auth.Record(r.Context(), i.auditor, auth.Audit{
			AccountID: account.ID, Action: auth.ActionAdmin, Reason: string(identity.CodeAdminRequired),
		})
		writeCode(r.Context(), w, http.StatusForbidden, identity.CodeAdminRequired,
			"this needs an administrator")
		return auth.Account{}, false
	}
	return account, true
}

// spend takes one token from the caller's bucket, answering 429 when it is empty.
// The four unauthenticated endpoints share it (AC-24).
func (i *Identity) spend(w http.ResponseWriter, r *http.Request) bool {
	if i.svc.Limits().Allow(clientAddress(r)) {
		return true
	}
	writeCode(r.Context(), w, http.StatusTooManyRequests, identity.CodeRateLimited,
		"too many requests, wait a moment")
	return false
}

// setSessionCookie hands the raw session id back exactly once, as a cookie the
// browser will not expose to script and will not send cross site.
func (i *Identity) setSessionCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   i.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(identity.SessionLifetime),
	})
}

// clearSessionCookie empties the cookie and expires it immediately (AC-9).
func (i *Identity) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   i.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clientAddress is who a rate limit bucket belongs to: the last hop of
// X-Forwarded-For when the request came through the ingress.
//
// Not the connection address, which is the ingress pod for every caller and would
// collapse every bucket into one. Falling back to the connection address is right
// for a direct call, where there is no proxy to have written the header.
func clientAddress(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		hops := strings.Split(fwd, ",")
		return strings.TrimSpace(hops[len(hops)-1])
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

// decode reads a bounded JSON body into v, answering 422 on anything unreadable.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIdentityBody))
	if err := dec.Decode(v); err != nil {
		writeCode(r.Context(), w, http.StatusUnprocessableEntity, identity.CodeEmailInvalid,
			"that request body is not readable JSON")
		return false
	}
	return true
}

// fail answers a refusal from the identity layer, or a 500 for a fault.
//
// This is the one place an error becomes a status. A fault is logged and answered
// as internal: an internal error is never dressed up as an access decision, and a
// wrapped error string never reaches a caller (AC-27).
func (i *Identity) fail(ctx context.Context, w http.ResponseWriter, err error) {
	code, isRefusal := identity.CodeOf(err)
	if !isRefusal {
		slog.ErrorContext(ctx, "an identity request failed", "error", err)
		writeCode(ctx, w, http.StatusInternalServerError, identity.CodeInternal, "internal error")
		return
	}
	var e *identity.Error
	errors.As(err, &e)
	writeCode(ctx, w, statusFor(code), code, e.Message)
}

// statusFor maps a code onto its HTTP status. The pairing is fixed here rather
// than chosen per handler, so one code cannot mean two things on two endpoints.
func statusFor(c identity.Code) int {
	switch c {
	case identity.CodeEmailInvalid, identity.CodePasswordTooShort, identity.CodeInvalidExpiry:
		return http.StatusUnprocessableEntity
	case identity.CodeCredentialsInvalid:
		return http.StatusUnauthorized
	case identity.CodeEmailUnverified, identity.CodeAdminRequired, identity.CodeInviteInvalid:
		return http.StatusForbidden
	case identity.CodeLinkInvalid:
		return http.StatusBadRequest
	case identity.CodeTokenNameTaken:
		return http.StatusConflict
	case identity.CodeNotFound:
		return http.StatusNotFound
	case identity.CodeRateLimited:
		return http.StatusTooManyRequests
	case identity.CodeMailUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeCode answers with a code from the closed set and a sentence. The shape is
// {"error": {"code": "...", "message": "..."}}, which is the identity surface's
// own: the upload endpoints keep the plainer body they already answer with.
func writeCode(ctx context.Context, w http.ResponseWriter, status int, code identity.Code, message string) {
	writeJSON(ctx, w, status, map[string]any{
		"error": map[string]string{"code": string(code), "message": message},
	})
}
