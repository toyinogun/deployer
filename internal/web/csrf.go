package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"

	"github.com/toyinogun/deployer/internal/auth"
)

// csrfField is the hidden form field the synchroniser token travels in.
const csrfField = "csrf"

// csrfToken derives the token for one session: the HMAC SHA256 of the session
// id under the platform's key.
//
// Derived and never stored, so it is valid exactly as long as its session is and
// is revoked by the same act that revokes the session. Deriving it from the
// session id rather than from the raw cookie value keeps the cookie out of the
// page body, which the leak boundary forbids (AC-12, AC-31).
func (s *Server) csrfToken(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.opts.CSRFKey)
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

// checkCSRF guards one authenticated POST: the origin check first, then the
// synchroniser token compared in constant time. A refusal changes nothing,
// answers 403, and writes an audit row.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request, account auth.Account, sess auth.Session) bool {
	if !s.checkOrigin(w, r, account) {
		return false
	}
	want := s.csrfToken(sess.ID)
	got := r.PostFormValue(csrfField)
	if want == "" || !hmac.Equal([]byte(want), []byte(got)) {
		s.refuseCSRF(w, r, account, "csrf_invalid")
		return false
	}
	return true
}

// checkOrigin refuses a POST that a browser says came from somewhere else.
//
// Both headers are optional and both are trusted only when present. A request
// carrying neither passes here, and that is correct rather than a hole: every
// browser able to perform a cross site form post sends Sec-Fetch-Site, and a
// scripted client that omits both carries no session cookie either, so it is an
// unauthenticated request the session gate refuses anyway (AC-13).
//
// This is the whole guard on the pre authentication forms, which have no session
// to bind a synchroniser token to. That is stated knowingly in the spec's
// security model rather than papered over with a pre session cookie.
//
// The platform answers on more than one name, so a post is accepted from the
// console's configured origin or from the name it was itself addressed to, and a
// post carrying any other origin is still refused (spec 0021, AC-21; see
// acceptedOrigin for why this is a comparison rather than a list). Accepting
// same origin does not weaken the pre authentication CSRF pair: that cookie
// carries the `__Host-` prefix, so it is host scoped and each hostname mints its
// own nonce. A token minted on one host cannot satisfy a post to another
// (AC-21a).
func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request, account auth.Account) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		s.refuseCSRF(w, r, account, "origin_cross_site")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !s.acceptedOrigin(u, r) {
			s.refuseCSRF(w, r, account, "origin_mismatch")
			return false
		}
	}
	return true
}

// acceptedOrigin reports whether a POST may claim to come from this address.
//
// Two ways to be accepted. The configured set is the console's own address,
// which is the one name whose origin the platform knows before a request
// arrives. Everything else is accepted by being **same origin**: the post claims
// to come from the very name it was addressed to, which is a comparison rather
// than a list and so needs no configuration.
//
// That second clause is what the pages are actually reached by. They register on
// the bare pattern, so every name that is not the console or the deploy host
// serves them, the tailnet name and the LAN included, and spec 0021 (AC-26) makes
// the tailnet name the only way to reach the admin surface, since every admin
// page is 404 on the console. Spec 0022 removed DEPLOYER_PUBLIC_URL, which had
// been putting the tailnet origin in the set, and the accepted set silently
// became one entry: a sign in on the tailnet name answered 403 and the whole
// admin surface was unreachable. A list cannot hold names nobody configured, so
// this compares instead of listing.
//
// The scheme is compared as well as the host, so a post from http:// on the same
// name is still refused. It follows the console's own scheme, because a platform
// serving the console over TLS serves every one of its names that way.
//
// This does not weaken the pre authentication CSRF pair: the nonce cookie
// carries the `__Host-` prefix, so each hostname mints its own and a token
// minted on one host cannot satisfy a post to another (spec 0021, AC-21a).
func (s *Server) acceptedOrigin(u *url.URL, r *http.Request) bool {
	if slices.Contains(s.origins, u.Scheme+"://"+u.Host) {
		return true
	}
	return u.Host != "" && u.Host == r.Host && u.Scheme == s.originScheme()
}

// originScheme is the scheme every name this platform answers on is served over,
// taken from the console's own address rather than from the request, because a
// request that arrived through a proxy carries no TLS state of its own.
func (s *Server) originScheme() string {
	if s.secure {
		return "https"
	}
	return "http"
}

// refuseCSRF records the refusal and renders the 403 page. The reason names
// which of the two checks failed, because a form that stopped working and an
// attempt from elsewhere look identical without it.
func (s *Server) refuseCSRF(w http.ResponseWriter, r *http.Request, account auth.Account, reason string) {
	auth.Record(r.Context(), s.auditor, auth.Audit{
		ClientAddress: s.clientAddress(r),
		AccountID:     account.ID, Action: auth.ActionPageCSRF, Reason: reason,
	})
	s.renderPublic(w, r, http.StatusForbidden, "message", messagePage{
		Title:   "That form could not be accepted",
		Message: "Go back, reload the page, and try again.",
	})
}
