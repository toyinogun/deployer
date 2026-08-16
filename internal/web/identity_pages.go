package web

import (
	"errors"
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// formPage is what a form renders with: what the person typed, kept so a
// refusal does not empty the fields, and the one message shown above them.
type formPage struct {
	Email   string
	Name    string
	Next    string
	Token   string
	Message string
	// Invite is the registration code, carried from the query string into a
	// hidden field and nowhere else. It is never checked on the way through: the
	// page must not become a second oracle telling a holder which kind of bad
	// code they have (spec 0015, AC-18).
	Invite string
	// MailDown is whether there is no sender configured, which the forms that
	// exist only to send mail say plainly rather than failing on submit.
	MailDown bool
	// CSRF is the pre authentication token this form posts back, the HMAC of the
	// nonce in the browser's cookie. Never the nonce itself (spec 0019, AC-9).
	CSRF string
}

// withPreCSRF returns a copy carrying the token, and the message when one is
// given. An empty message leaves whatever sentence the page already holds, so
// filling the token in on a re rendered failure does not wipe its reason.
func (f formPage) withPreCSRF(token, message string) any {
	f.CSRF = token
	if message != "" {
		f.Message = message
	}
	return f
}

// unverifiedPageData is the dedicated page a registered but unverified sign in
// lands on: the address, so the person can see which one to check, and the
// resend limit written out, so hitting it is not a mystery (AC-8).
// It hosts the resend form, so it carries a token of its own: /resend has no GET
// route to set the cookie from, and this is the page its post comes off.
type unverifiedPageData struct {
	Email string
	// CSRF and Message are the same pair formPage carries; this page is not a
	// formPage only because its body differs.
	CSRF    string
	Message string
}

// withPreCSRF returns a copy carrying the token, and the message when one is
// given. See formPage.withPreCSRF.
func (u unverifiedPageData) withPreCSRF(token, message string) any {
	u.CSRF = token
	if message != "" {
		u.Message = message
	}
	return u
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderPreAuth(w, r, http.StatusOK, "login", formPage{Next: r.URL.Query().Get("next")})
}

// loginSubmit signs in. Every rule here belongs to the identity service: the
// generic message on bad credentials and the lockout both come from the same
// svc.Login the JSON surface calls, so the browser cannot be a softer way in
// (AC-5). That was a description of intent rather than of the code until
// 2026-08-16: the lockout lived in the JSON handler alone, and svc.Login touched
// the limiter nowhere, so this surface counted no failures and checked no
// penalty. It moved into svc.Login rather than being copied here, because two
// copies is how the surfaces drift apart again.
func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkPreCSRF(w, r, "login", formPage{
		Email: r.PostFormValue("email"), Next: r.PostFormValue("next"),
	}) || !s.spend(w, r) {
		return
	}
	email, password := r.PostFormValue("email"), r.PostFormValue("password")
	next := r.PostFormValue("next")

	in, err := s.svc.Login(r.Context(), email, password)
	if err != nil {
		code, refusal := identity.CodeOf(err)
		if refusal && code == identity.CodeEmailUnverified {
			s.renderPreAuth(w, r, http.StatusForbidden, "unverified", unverifiedPageData{Email: email})
			return
		}
		s.formFailure(w, r, "login", formPage{Email: email, Next: next}, err)
		return
	}
	// Exactly one CSRF mechanism is live at a time: the session token takes over
	// here, so the pre authentication cookie goes in the same response (AC-7).
	s.clearPreCSRFCookie(w)
	s.setSessionCookie(w, in.Raw)
	http.Redirect(w, r, afterSignIn(next, in.Account), http.StatusSeeOther)
}

// afterSignIn is where a successful sign in lands. Someone who has verified their
// address and has never been handed their agent configuration goes to /connect
// once; everybody else goes where they always did (spec 0023, AC-3).
//
// A next outranks it, and that is the whole reason this is not a check inside
// /apps. A next only exists because the session gate put it there, meaning the
// person was already trying to reach a particular page, and sending them
// elsewhere would drop that with nothing to recover it from. They are not
// stamped by that sign in either, so the next plain one still lands on /connect.
//
// The bootstrap account never satisfies this: it holds no address, so it is
// never verified, and it is refused by Login before reaching here anyway (AC-5).
func afterSignIn(next string, account identity.Account) string {
	if next != "" {
		return safeNext(next)
	}
	if account.Verified && !account.Connected {
		return "/connect"
	}
	// next is "" by here, so this is where an ordinary sign in lands.
	return "/apps"
}

// registerPage renders the form and copies the invite code from the query into
// a hidden field. It never touches the invites table: an unknown, expired,
// revoked, spent and valid code all render the identical page, and every
// distinction is made on the post (AC-18).
func (s *Server) registerPage(w http.ResponseWriter, r *http.Request) {
	noReferrer(w)
	s.renderPreAuth(w, r, http.StatusOK, "register", formPage{
		Invite: r.URL.Query().Get("invite"), MailDown: !s.opts.HasMailer,
	})
}

// registerSubmit registers. A duplicate address renders the identical check your
// email page a new one does, because the service answers both the same way and
// the page must not be the surface that tells an attacker which addresses
// exist (AC-6). A bad invite is refused in the words the service holds, which is
// the same sentence and status the JSON surface answers with (spec 0015, AC-2).
func (s *Server) registerSubmit(w http.ResponseWriter, r *http.Request) {
	noReferrer(w)
	if !s.checkPreCSRF(w, r, "register", formPage{
		Email: r.PostFormValue("email"), Name: r.PostFormValue("display_name"),
		Invite: r.PostFormValue("invite"), MailDown: !s.opts.HasMailer,
	}) || !s.spend(w, r) {
		return
	}
	email := r.PostFormValue("email")
	name := r.PostFormValue("display_name")
	invite := r.PostFormValue("invite")

	if err := s.svc.Register(r.Context(), invite, email, r.PostFormValue("password"), name); err != nil {
		s.formFailure(w, r, "register",
			formPage{Email: email, Name: name, Invite: invite, MailDown: !s.opts.HasMailer}, err)
		return
	}
	s.checkYourMail(w, r, email)
}

// noReferrer stops the browser carrying this page's address onward. The invite
// code rides in the query string, and without this it would travel in the
// Referer header of anything the page links to (AC-14).
func noReferrer(w http.ResponseWriter) {
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// verifyPage consumes the link. All four ways a link can be no good, consumed,
// expired, unknown, or for the other purpose, render the one shared page in the
// same words, because telling them apart tells a holder of a stolen link which
// kind they hold (AC-7).
func (s *Server) verifyPage(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Verify(r.Context(), r.URL.Query().Get("token")); err != nil {
		if _, refusal := identity.CodeOf(err); !refusal {
			s.internalError(w, r, err, "verifying an address failed")
			return
		}
		s.renderPublic(w, r, http.StatusBadRequest, "message", messagePage{
			Title:       "That link no longer works",
			Message:     "Verification links work once and expire after 24 hours. Ask for a fresh one and check the newest message.",
			Action:      "/unverified",
			ActionLabel: "Send a new link",
		})
		return
	}
	s.renderPublic(w, r, http.StatusOK, "message", messagePage{
		Title:       "Address confirmed",
		Message:     "Your address is confirmed. Sign in to see your apps.",
		Action:      "/login",
		ActionLabel: "Sign in",
	})
}

func (s *Server) unverifiedPage(w http.ResponseWriter, r *http.Request) {
	s.renderPreAuth(w, r, http.StatusOK, "unverified", unverifiedPageData{Email: r.URL.Query().Get("email")})
}

// resendSubmit issues a fresh verification link. The confirmation is the same
// whether or not the address is one the platform knows.
func (s *Server) resendSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkPreCSRF(w, r, "unverified", unverifiedPageData{
		Email: r.PostFormValue("email"),
	}) || !s.spend(w, r) {
		return
	}
	email := r.PostFormValue("email")
	if err := s.svc.Resend(r.Context(), email); err != nil {
		if code, refusal := identity.CodeOf(err); refusal && code != identity.CodeNotFound {
			s.renderPreAuth(w, r, http.StatusOK, "unverified", unverifiedPageData{Email: email})
			return
		}
		if _, refusal := identity.CodeOf(err); !refusal {
			s.internalError(w, r, err, "resending a verification link failed")
			return
		}
	}
	s.checkYourMail(w, r, email)
}

func (s *Server) forgotPage(w http.ResponseWriter, r *http.Request) {
	s.renderPreAuth(w, r, http.StatusOK, "forgot", formPage{MailDown: !s.opts.HasMailer})
}

// forgotSubmit starts a password reset. The confirmation is identical whether or
// not the address exists, for the same reason registration's is (AC-9).
func (s *Server) forgotSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkPreCSRF(w, r, "forgot", formPage{
		Email: r.PostFormValue("email"), MailDown: !s.opts.HasMailer,
	}) || !s.spend(w, r) {
		return
	}
	email := r.PostFormValue("email")
	if err := s.svc.Forgot(r.Context(), email); err != nil {
		if _, refusal := identity.CodeOf(err); !refusal {
			s.internalError(w, r, err, "starting a password reset failed")
			return
		}
		if code, _ := identity.CodeOf(err); code == identity.CodeMailUnavailable || code == identity.CodeRateLimited {
			s.formFailure(w, r, "forgot", formPage{Email: email, MailDown: !s.opts.HasMailer}, err)
			return
		}
	}
	s.renderPublic(w, r, http.StatusOK, "message", messagePage{
		Title:   "Check your email",
		Message: "If that address has an account, a reset link is on its way. The link works once and expires in 24 hours.",
	})
}

func (s *Server) resetPage(w http.ResponseWriter, r *http.Request) {
	s.renderPreAuth(w, r, http.StatusOK, "reset", formPage{Token: r.URL.Query().Get("token")})
}

// resetSubmit sets the new password and lands the person on sign in. It does not
// sign them in: the service revokes every session the account holds, and signing
// straight back in would hide that from the person whose account it was.
func (s *Server) resetSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkPreCSRF(w, r, "reset", formPage{
		Token: r.PostFormValue("token"),
	}) || !s.spend(w, r) {
		return
	}
	token := r.PostFormValue("token")
	if err := s.svc.Reset(r.Context(), token, r.PostFormValue("password")); err != nil {
		code, refusal := identity.CodeOf(err)
		if refusal && code == identity.CodeLinkInvalid {
			s.renderPublic(w, r, http.StatusBadRequest, "message", messagePage{
				Title:       "That link no longer works",
				Message:     "Reset links work once and expire after 24 hours. Ask for a fresh one and use the newest message.",
				Action:      "/forgot",
				ActionLabel: "Start again",
			})
			return
		}
		s.formFailure(w, r, "reset", formPage{Token: token}, err)
		return
	}
	s.clearSessionCookie(w)
	s.renderPublic(w, r, http.StatusOK, "message", messagePage{
		Title:       "Password changed",
		Message:     "Your password is set and every other session has been signed out. Sign in with the new one.",
		Action:      "/login",
		ActionLabel: "Sign in",
	})
}

// logout revokes the session behind the cookie, clears it, and lands on sign in.
// The cookie is cleared even when the revoke failed: leaving a browser holding a
// cookie the person asked to drop is the worse of the two outcomes.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, account, sess) {
		return
	}
	if err := s.svc.Logout(r.Context(), sess.ID); err != nil && !errors.Is(err, identity.ErrSessionInvalid) {
		s.internalError(w, r, err, "signing out failed")
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		ClientAddress: s.clientAddress(r),
		AccountID:     account.ID, Action: auth.ActionLogout, Allowed: true,
	})
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// checkYourMail is the one confirmation registration and resend share.
func (s *Server) checkYourMail(w http.ResponseWriter, r *http.Request, email string) {
	s.renderPublic(w, r, http.StatusOK, "message", messagePage{
		Title:   "Check your email",
		Message: "If that address is new here, a confirmation link is on its way to " + email + ". The link works once and expires in 24 hours.",
	})
}

// formFailure re renders a form carrying the refusal's own sentence. A fault is
// not a refusal and never renders as one: it is logged and answered as an
// internal error, so an internal failure is never dressed up as something the
// person typed wrong.
func (s *Server) formFailure(w http.ResponseWriter, r *http.Request, page string, form formPage, err error) {
	code, refusal := identity.CodeOf(err)
	if !refusal {
		s.internalError(w, r, err, "an identity page request failed")
		return
	}
	var e *identity.Error
	errors.As(err, &e)
	form.Message = e.Message
	s.renderPreAuth(w, r, statusFor(code), page, form)
}

// statusFor maps a refusal code onto its status, the same pairing the JSON
// surface fixes, so one code cannot mean two things on two surfaces.
func statusFor(c identity.Code) int {
	switch c {
	case identity.CodeEmailInvalid, identity.CodePasswordTooShort, identity.CodeInvalidExpiry,
		identity.CodeNoteTooLong:
		return http.StatusUnprocessableEntity
	case identity.CodeCredentialsInvalid:
		return http.StatusUnauthorized
	case identity.CodeEmailUnverified, identity.CodeAdminRequired, identity.CodeInviteInvalid:
		return http.StatusForbidden
	case identity.CodeLinkInvalid:
		return http.StatusBadRequest
	case identity.CodeTokenNameTaken, identity.CodeAddressRegistered:
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
