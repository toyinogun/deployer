package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// currentSession resolves the caller's session cookie without answering
// anything. It is the read the redirecting gate and the root redirect share.
//
// A page authenticates on the cookie alone: auth.SessionID reads that one
// header, so a request carrying a bearer API token instead resolves to nothing
// and is treated as signed out, which is what keeps the browser from being a
// second door for a machine credential (AC-3).
func (s *Server) currentSession(r *http.Request) (auth.Account, auth.Session, bool) {
	raw := auth.SessionID(r, s.secure)
	if raw == "" {
		return auth.Account{}, auth.Session{}, false
	}
	account, sess, err := s.auth.AuthenticateSession(r.Context(), raw)
	if err != nil {
		if !errors.Is(err, auth.ErrSessionInvalid) && !errors.Is(err, auth.ErrEmailUnverified) {
			slog.ErrorContext(r.Context(), "resolving a page session failed", "error", err)
		}
		return auth.Account{}, auth.Session{}, false
	}
	return account, sess, true
}

// session is the gate every page under /apps, /tokens and /admin starts at.
// Without a live session it redirects to sign in carrying where the caller was
// going, so signing in finishes the navigation rather than dropping it (AC-2).
func (s *Server) session(w http.ResponseWriter, r *http.Request) (auth.Account, auth.Session, bool) {
	account, sess, ok := s.currentSession(r)
	if !ok {
		s.toLogin(w, r)
		return auth.Account{}, auth.Session{}, false
	}
	return account, sess, true
}

// toLogin sends a signed out visitor to the form, remembering the path only.
// The query string is dropped on purpose: a cursor is not worth carrying and a
// token in a link never should be.
func (s *Server) toLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
}

// authorizeSession is the session gate for the authorize endpoint, and the one
// place the query string survives the trip to the sign in form.
//
// toLogin drops it deliberately, because a token in a link must never travel
// through a redirect. Nothing in an authorize URL is a secret: the client id,
// the redirect URI, the state and the PKCE challenge are all the public halves
// of the exchange, and losing them would send the person back to a request that
// no longer says what it was for (spec 0024, AC-9, AC-9a).
func (s *Server) authorizeSession(w http.ResponseWriter, r *http.Request) (auth.Account, auth.Session, bool) {
	account, sess, ok := s.currentSession(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return auth.Account{}, auth.Session{}, false
	}
	return account, sess, true
}

// adminSession is session plus the admin check. An ordinary account gets the
// 403 page and an audit row; a signed out one is redirected, because being
// signed out is not a refusal, it is not having answered yet (AC-24).
func (s *Server) adminSession(w http.ResponseWriter, r *http.Request) (auth.Account, auth.Session, bool) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return auth.Account{}, auth.Session{}, false
	}
	if !account.IsAdmin {
		auth.Record(r.Context(), s.auditor, auth.Audit{
			ClientAddress: s.clientAddress(r),
			AccountID:     account.ID, Action: auth.ActionAdmin, Reason: string(identity.CodeAdminRequired),
		})
		s.renderRefused(w, r, account, sess, http.StatusForbidden, "Not for this account",
			"This page is for platform administrators.")
		return auth.Account{}, auth.Session{}, false
	}
	return account, sess, true
}

// spend takes one token from the caller's bucket. The pre authentication forms
// share the same bucket the JSON surface uses, because they run the same service
// call and a limit that a second surface resets is not a limit (AC-5).
func (s *Server) spend(w http.ResponseWriter, r *http.Request) bool {
	if s.svc.Limits().Allow(s.clientAddress(r)) {
		return true
	}
	s.renderPublic(w, r, http.StatusTooManyRequests, "message", messagePage{
		Title:   "Too many attempts",
		Message: "Wait a moment and try that again.",
	})
	return false
}

// setSessionCookie hands the raw session id back exactly once, with the same
// attributes the JSON surface sets. The two must not drift: a cookie written
// one way and cleared another is a session that outlives its sign out.
func (s *Server) setSessionCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName(s.secure),
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(identity.SessionLifetime),
	})
}

// clearSessionCookie empties the cookie and expires it immediately.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName(s.secure),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clientAddress is who a rate limit bucket belongs to and whose address an audit
// row records. It is auth.ClientAddress with the platform's public hosts filled
// in: the derivation itself lives in one place, so no two surfaces can spend
// from two different buckets (spec 0021, AC-16; spec 0022, AC-14).
func (s *Server) clientAddress(r *http.Request) string {
	return auth.ClientAddress(r, s.opts.TrustedHosts...)
}
