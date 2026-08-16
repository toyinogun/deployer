package httpapi

import (
	"errors"
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// checkYourMail is the one answer register, resend and forgot ever give. It is
// identical whether the address exists, does not exist, or already has an
// account, because any difference is a way to ask whether somebody is registered.
const checkYourMail = "check your email for the next step"

// register spends an invite, creates the account it authorised, and mails that
// account a verification link.
//
// The answer is a 202 with this body whether or not the address was free, and it
// costs a full password hash either way (AC-2). A caller with no valid invite is
// refused before any of that, in the same words and with the same status the
// page surface answers with, because both call the one service method that holds
// the check (spec 0015, AC-1, AC-2).
func (i *Identity) register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !i.spend(w, r) {
		return
	}
	var body struct {
		Invite   string `json:"invite"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := i.svc.Register(ctx, body.Invite, body.Email, body.Password, body.Name); err != nil {
		i.fail(ctx, w, err)
		return
	}
	// Audited without an account id on purpose: naming the account here would
	// record whether the address was already taken, which is the one thing this
	// endpoint is built not to reveal.
	auth.Record(ctx, i.auditor, auth.Audit{ClientAddress: i.clientAddress(r), Action: auth.ActionRegister, Allowed: true})
	writeJSON(ctx, w, http.StatusAccepted, map[string]string{"message": checkYourMail})
}

// verify spends a verification link. Every way it can fail is link_invalid, in
// the same words (AC-5).
func (i *Identity) verify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := i.svc.Verify(ctx, r.URL.Query().Get("token")); err != nil {
		i.fail(ctx, w, err)
		return
	}
	writeJSON(ctx, w, http.StatusOK, map[string]bool{"verified": true})
}

// resend issues a fresh verification link, superseding the live one.
func (i *Identity) resend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !i.spend(w, r) {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := i.svc.Resend(ctx, body.Email); err != nil {
		i.fail(ctx, w, err)
		return
	}
	writeJSON(ctx, w, http.StatusAccepted, map[string]string{"message": checkYourMail})
}

// login checks a password and opens a session.
//
// A failure writes an audit row and feeds the per address backoff; a success
// clears it, so one correct password undoes the penalty (AC-8, AC-23).
func (i *Identity) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !i.spend(w, r) {
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	// The lockout, the backoff and the clear on success all live in svc.Login, so
	// this surface and the browser refuse identically (spec 0021, AC-5). They
	// used to live here, which made the browser the softer way in. Do not
	// reintroduce them: a second Failed call here would count one wrong password
	// twice and halve the free attempts on this surface alone.
	in, err := i.svc.Login(ctx, body.Email, body.Password)
	if err != nil {
		code, _ := identity.CodeOf(err)
		auth.Record(ctx, i.auditor, auth.Audit{ClientAddress: i.clientAddress(r), Action: auth.ActionLogin, Reason: string(code)})
		i.fail(ctx, w, err)
		return
	}

	i.setSessionCookie(w, in.Raw)
	auth.Record(ctx, i.auditor, auth.Audit{
		ClientAddress: i.clientAddress(r),
		AccountID:     in.Account.ID, Action: auth.ActionLogin, Allowed: true,
	})
	writeJSON(ctx, w, http.StatusOK, meBody(identityToAuth(in.Account)))
}

// logout revokes the current session and clears the cookie.
func (i *Identity) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, sess, ok := i.session(w, r)
	if !ok {
		return
	}
	if err := i.svc.Logout(ctx, sess.ID); err != nil {
		i.fail(ctx, w, err)
		return
	}
	i.clearSessionCookie(w)
	auth.Record(ctx, i.auditor, auth.Audit{
		ClientAddress: i.clientAddress(r),
		AccountID:     account.ID, Action: auth.ActionLogout, Allowed: true,
	})
	w.WriteHeader(http.StatusNoContent)
}

// forgot mails a password reset link. It always answers 202, whether or not the
// address exists (AC-28).
func (i *Identity) forgot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !i.spend(w, r) {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := i.svc.Forgot(ctx, body.Email); err != nil {
		i.fail(ctx, w, err)
		return
	}
	writeJSON(ctx, w, http.StatusAccepted, map[string]string{"message": checkYourMail})
}

// reset spends a reset link and sets a new password, which revokes every live
// session the account holds (AC-29).
func (i *Identity) reset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := i.svc.Reset(ctx, body.Token, body.Password); err != nil {
		i.fail(ctx, w, err)
		return
	}
	// The session this request may have carried is one of the ones just revoked,
	// so the cookie goes with it rather than being left to fail on the next call.
	i.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// me reports who the caller is.
func (i *Identity) me(w http.ResponseWriter, r *http.Request) {
	account, _, ok := i.session(w, r)
	if !ok {
		return
	}
	acc, err := i.svc.AccountByID(r.Context(), account.ID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			writeCode(r.Context(), w, http.StatusUnauthorized, identity.CodeCredentialsInvalid, "sign in first")
			return
		}
		i.fail(r.Context(), w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, meBody(identityToAuth(acc)))
}

// meBody is the shape both login and me answer with. Nothing secret is in it: no
// hash, no session id, no token.
func meBody(a auth.Account) map[string]any {
	return map[string]any{
		"email":    a.Email,
		"name":     a.Name,
		"is_admin": a.IsAdmin,
		"verified": a.Verified,
	}
}

// identityToAuth is the small bridge between the two views of an account, so the
// response shape is written once.
func identityToAuth(a identity.Account) auth.Account {
	return auth.Account{
		ID:       a.ID,
		Name:     a.DisplayName,
		Email:    a.Email,
		Verified: a.Verified,
		Disabled: a.Disabled,
		IsAdmin:  a.IsAdmin,
	}
}
