package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"

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
func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request, account auth.Account) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		s.refuseCSRF(w, r, account, "origin_cross_site")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || s.origin == "" || u.Scheme+"://"+u.Host != s.origin {
			s.refuseCSRF(w, r, account, "origin_mismatch")
			return false
		}
	}
	return true
}

// refuseCSRF records the refusal and renders the 403 page. The reason names
// which of the two checks failed, because a form that stopped working and an
// attempt from elsewhere look identical without it.
func (s *Server) refuseCSRF(w http.ResponseWriter, r *http.Request, account auth.Account, reason string) {
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionPageCSRF, Reason: reason,
	})
	s.renderPublic(w, r, http.StatusForbidden, "message", messagePage{
		Title:   "That form could not be accepted",
		Message: "Go back, reload the page, and try again.",
	})
}
