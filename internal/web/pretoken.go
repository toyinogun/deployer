package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
)

// The pre authentication CSRF mechanism, which is the second of two and works
// differently from the first.
//
// A signed in form binds its token to the session id (see csrf.go). A form shown
// before sign in has no session to bind to, so it binds to a nonce the platform
// puts in a cookie: the cookie carries the nonce, the form carries the HMAC of
// it, and a page on another site can cause neither to be sent with a value it
// knows. Exactly one of the two mechanisms is live per request, and a successful
// sign in deletes this cookie as it sets the session one (AC-7).
//
// The nonce is the secret and the HMAC is the proof. The nonce never reaches a
// page body, a log line, or an audit row; only its HMAC does, which is the same
// leak boundary the session derived token holds (AC-9).
const (
	// preCSRFCookieSecure is the name in production. The `__Host-` prefix is part
	// of the protection, not decoration: a browser refuses to accept such a cookie
	// from a page that sets a Domain attribute, so an app deployed by a stranger on
	// a sibling hostname under the platform's wildcard cannot write this cookie and
	// shadow the console's own.
	preCSRFCookieSecure = "__Host-deployer_csrf"
	// preCSRFCookiePlain is the name over plain HTTP, where a browser refuses a
	// Secure cookie outright and keeping the prefix would make signing in locally
	// impossible. A plain HTTP deployment therefore loses the sibling subdomain
	// guarantee above. The cluster is served over HTTPS, so this name is never
	// reached in production (AC-2a). The session cookie gates its own Secure flag
	// on the same value.
	preCSRFCookiePlain = "deployer_csrf"

	// preCSRFNonceBytes is how much entropy the nonce carries, hex encoded. Both
	// halves of the pair are then hex, so neither needs cookie value escaping.
	preCSRFNonceBytes = 32

	// The two refusal reasons, kept visually distinct from the signed in path's
	// csrf_invalid so an audit row says which mechanism fired (AC-5).
	reasonPreTokenMissing  = "csrf_pretoken_missing"
	reasonPreTokenMismatch = "csrf_pretoken_mismatch"

	// preCSRFExpiredMessage is what a person sees when their form is refused. It
	// is the ordinary case, not an attack: a cookie cleared mid reset, or a form
	// left open past the end of a browser session.
	preCSRFExpiredMessage = "That form expired before it was submitted. Please try again."
)

// preAuthForm is a page holding a form shown before sign in. Every such page
// renders the token it must post back, and a refused post re renders the page
// carrying a fresh one plus the sentence saying what happened.
//
// It returns a copy rather than writing through a pointer, so a refusal cannot
// alter the value its caller still holds.
type preAuthForm interface {
	withPreCSRF(token, message string) any
}

// preCSRFCookieName is the cookie's name, which depends on whether the platform
// is served over HTTPS. See preCSRFCookieSecure for why it changes at all.
func (s *Server) preCSRFCookieName() string {
	if s.secure {
		return preCSRFCookieSecure
	}
	return preCSRFCookiePlain
}

// preCSRFToken derives the token one nonce posts back: the HMAC SHA256 of the
// nonce under the platform's key, the same key and the same hidden field name
// the signed in forms already use.
func (s *Server) preCSRFToken(nonce string) string {
	if nonce == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.opts.CSRFKey)
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// preCSRFFor returns the token for this request, setting a fresh nonce cookie
// when the request carries no usable one and reusing the existing nonce when it
// does.
//
// Reusing rather than rotating is what lets two tabs open on the same form both
// submit: a rotation would invalidate whichever tab rendered first (AC-10).
func (s *Server) preCSRFFor(w http.ResponseWriter, r *http.Request) (string, error) {
	if nonce, ok := s.preCSRFNonce(r); ok {
		return s.preCSRFToken(nonce), nil
	}
	nonce, err := newPreCSRFNonce()
	if err != nil {
		return "", err
	}
	s.setPreCSRFCookie(w, nonce)
	return s.preCSRFToken(nonce), nil
}

// preCSRFNonce reads the nonce this request carries, if it carries a usable one.
// A malformed value is treated as absent rather than as an attack: it is what a
// truncated or hand edited cookie looks like, and the answer to both is a fresh
// one.
func (s *Server) preCSRFNonce(r *http.Request) (string, bool) {
	c, err := r.Cookie(s.preCSRFCookieName())
	if err != nil {
		return "", false
	}
	if len(c.Value) != hex.EncodedLen(preCSRFNonceBytes) {
		return "", false
	}
	if _, err := hex.DecodeString(c.Value); err != nil {
		return "", false
	}
	return c.Value, true
}

// setPreCSRFCookie writes the nonce cookie.
//
// No Max-Age and no Expires, so it lives as long as the browser session, and no
// Domain, which the `__Host-` prefix forbids outright and which would hand the
// cookie to every app on the wildcard if it were there (AC-2).
func (s *Server) setPreCSRFCookie(w http.ResponseWriter, nonce string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.preCSRFCookieName(),
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearPreCSRFCookie drops the nonce cookie. Called as a sign in succeeds, so
// exactly one CSRF mechanism is live at a time (AC-7).
func (s *Server) clearPreCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.preCSRFCookieName(),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// renderPreAuth renders a page holding a pre authentication form, filling in the
// token it must post back and setting the nonce cookie when the browser carries
// none.
//
// Every render of the five such pages goes through here, including the ones a
// failed submit produces: a form re rendered without a token would refuse the
// next attempt, which reads to a person as the platform being broken (AC-1).
func (s *Server) renderPreAuth(w http.ResponseWriter, r *http.Request, status int, page string, form preAuthForm) {
	token, err := s.preCSRFFor(w, r)
	if err != nil {
		s.internalError(w, r, err, "drawing a form token failed")
		return
	}
	s.renderPublic(w, r, status, page, form.withPreCSRF(token, ""))
}

// checkPreCSRF guards one pre authentication POST: the origin check first and
// unchanged, then the nonce cookie against the posted field in constant time. A
// refusal changes nothing, answers 403, writes an audit row, and comes back as
// the form rather than as a dead end.
//
// It runs before the rate limiter is spent, so a refused post does not also cost
// the person one of their attempts.
func (s *Server) checkPreCSRF(w http.ResponseWriter, r *http.Request, page string, form preAuthForm) bool {
	if !s.checkOrigin(w, r, auth.Account{}) {
		return false
	}
	nonce, ok := s.preCSRFNonce(r)
	if !ok {
		s.refusePreCSRF(w, r, reasonPreTokenMissing, page, form)
		return false
	}
	want := s.preCSRFToken(nonce)
	if !hmac.Equal([]byte(want), []byte(r.PostFormValue(csrfField))) {
		s.refusePreCSRF(w, r, reasonPreTokenMismatch, page, form)
		return false
	}
	return true
}

// refusePreCSRF records the refusal and re renders the form.
//
// This is deliberately not refuseCSRF, which renders a standalone message page
// and has no way back to a form. A person who cleared their cookies mid password
// reset would otherwise land on a dead end (AC-6). The fields carried back are
// the ones that are safe to keep: the address, never the password.
//
// The token it renders comes from a fresh cookie when none arrived, and from the
// existing nonce when one did. Rotating on a mismatch would work here and break
// the other tab, which is the case AC-10 exists for.
func (s *Server) refusePreCSRF(w http.ResponseWriter, r *http.Request, reason, page string, form preAuthForm) {
	auth.Record(r.Context(), s.auditor, auth.Audit{
		Action: auth.ActionPageCSRF, Reason: reason,
	})
	token, err := s.preCSRFFor(w, r)
	if err != nil {
		s.internalError(w, r, err, "drawing a form token failed")
		return
	}
	s.renderPublic(w, r, http.StatusForbidden, page, form.withPreCSRF(token, preCSRFExpiredMessage))
}

// newPreCSRFNonce draws a fresh nonce.
func newPreCSRFNonce() (string, error) {
	b := make([]byte, preCSRFNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("web: drawing a form nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
