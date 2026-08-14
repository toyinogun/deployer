package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// tokensPageData is the token list, and, on the one response that mints, the
// raw value shown exactly once.
type tokensPageData struct {
	Tokens []identity.TokenView
	// Minted is the raw token, held in this response body and nowhere else. It
	// is never put in a URL, never re rendered on a later request, and never
	// logged, so closing the panel really is the last time it exists outside the
	// person's clipboard (AC-22).
	Minted string
	// MintedName names which token the panel belongs to, so a person minting a
	// second one can tell them apart.
	MintedName string
	Message    string
}

func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	s.renderTokens(w, r, account, sess, http.StatusOK, tokensPageData{})
}

// tokenMint mints a token and renders the one time panel. The raw value reaches
// the response and stops there: the redirect a form post would normally end with
// is deliberately not used, because a redirect would have to carry the value in
// a URL or hold it somewhere between two requests.
func (s *Server) tokenMint(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, account, sess) {
		return
	}
	days := 0
	if raw := r.PostFormValue("expires_days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.renderTokens(w, r, account, sess, http.StatusUnprocessableEntity, tokensPageData{
				Message: "The number of days must be a whole number.",
			})
			return
		}
		days = n
	}
	name := r.PostFormValue("name")

	minted, err := s.svc.MintToken(r.Context(), toIdentityAccount(account), name, days)
	if err != nil {
		code, refusal := identity.CodeOf(err)
		if !refusal {
			s.internalError(w, r, err, "minting a token from a page failed")
			return
		}
		var e *identity.Error
		errors.As(err, &e)
		s.renderTokens(w, r, account, sess, statusFor(code), tokensPageData{Message: e.Message})
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionTokenMint, Allowed: true,
		TargetType: "api_token", TargetID: minted.Token.ID,
	})
	s.renderTokens(w, r, account, sess, http.StatusOK, tokensPageData{
		Minted: minted.Raw, MintedName: minted.Token.Name,
	})
}

// tokenRevoke revokes a token the caller owns. One belonging to someone else
// answers not found, the same as one that does not exist.
func (s *Server) tokenRevoke(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, account, sess) {
		return
	}
	id := r.PathValue("id")
	if err := s.svc.RevokeToken(r.Context(), account.ID, id); err != nil {
		code, refusal := identity.CodeOf(err)
		if !refusal {
			s.internalError(w, r, err, "revoking a token from a page failed")
			return
		}
		if code == identity.CodeNotFound {
			s.renderRefused(w, r, account, sess, http.StatusNotFound, "No such token",
				"There is no token by that id on this account.")
			return
		}
		s.renderTokens(w, r, account, sess, statusFor(code), tokensPageData{Message: "That token could not be revoked."})
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionTokenRevoke, Allowed: true,
		TargetType: "api_token", TargetID: id,
	})
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

// renderTokens reads the live list and renders it around whatever the caller
// wants shown above it.
func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, account auth.Account, sess auth.Session,
	status int, data tokensPageData,
) {
	tokens, err := s.svc.ListTokens(r.Context(), account.ID)
	if err != nil {
		s.internalError(w, r, err, "listing tokens for a page failed")
		return
	}
	data.Tokens = tokens
	s.render(w, r, account, sess, status, "tokens", "tokens", data)
}

// toIdentityAccount carries the resolved account across the one call that takes
// the identity layer's own shape. Only the three fields the mint actually reads
// are carried: the display name is not one of them, and auth.Account holds the
// platform's internal account name rather than the person's anyway.
func toIdentityAccount(a auth.Account) identity.Account {
	return identity.Account{ID: a.ID, Email: a.Email, Verified: a.Verified}
}
