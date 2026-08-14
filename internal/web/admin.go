package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// adminPageData is the accounts list. An admin sees who registered and may
// disable, enable, and revoke a credential. It carries no visibility over apps:
// the admin flag is about accounts and nothing else (spec 0007, AC-21).
type adminPageData struct {
	Accounts []adminAccountRow
	Message  string
}

// adminAccountRow is one account, with the tokens it holds so the revoke action
// has something to name.
type adminAccountRow struct {
	ID       string
	Email    string
	Name     string
	Verified bool
	IsAdmin  bool
	Disabled bool
	Created  string
	Tokens   []identity.TokenView
	// Self is the signed in admin's own row, which renders no disable control:
	// disabling yourself revokes the session you are reading the page with.
	Self bool
}

func (s *Server) adminAccountsPage(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	s.renderAdmin(w, r, admin, sess, http.StatusOK, "")
}

// adminDisable locks an account out, revoking every session and link it holds.
//
// The typed email is a confirmation, not an authorization: it is compared
// against the account the path names so that a misclick on a dense table cannot
// sign somebody else out of everything (AC-25).
func (s *Server) adminDisable(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, admin, sess) {
		return
	}
	target := r.PathValue("id")

	account, err := s.svc.AccountByID(r.Context(), target)
	if err != nil {
		s.adminFailure(w, r, admin, sess, target, "disable", err)
		return
	}
	typed := strings.TrimSpace(r.PostFormValue("confirm_email"))
	if !strings.EqualFold(typed, account.Email) {
		auth.Record(r.Context(), s.auditor, auth.Audit{
			AccountID: admin.ID, Action: auth.ActionAdmin,
			TargetType: "account", TargetID: target, Reason: "disable: confirmation_mismatch",
		})
		s.renderAdmin(w, r, admin, sess, http.StatusUnprocessableEntity,
			"That address did not match the account you were disabling, so nothing changed.")
		return
	}
	s.adminSetDisabled(w, r, admin, sess, target, true, "disable")
}

// adminEnable lets a disabled account back in. Revocation is one way, so the
// sessions it held before stay dead.
func (s *Server) adminEnable(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, admin, sess) {
		return
	}
	s.adminSetDisabled(w, r, admin, sess, r.PathValue("id"), false, "enable")
}

// adminSetDisabled is both state changes, which differ only in the flag and the
// word recorded. It writes the same audit row its JSON equivalent writes.
func (s *Server) adminSetDisabled(w http.ResponseWriter, r *http.Request, admin auth.Account, sess auth.Session,
	target string, disabled bool, action string,
) {
	if err := s.svc.SetDisabled(r.Context(), target, disabled); err != nil {
		s.adminFailure(w, r, admin, sess, target, action, err)
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "account", TargetID: target, Allowed: true, Reason: action,
	})
	http.Redirect(w, r, "/admin/accounts", http.StatusSeeOther)
}

// adminRevokeToken kills another account's token, which is the one thing an
// admin may do to a credential that is not theirs. The pair has to match: a
// token id belonging to a different account than the path names is not found,
// never a silent revocation of the wrong row.
func (s *Server) adminRevokeToken(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, admin, sess) {
		return
	}
	target, tokenID := r.PathValue("id"), r.PathValue("tokenId")
	if err := s.svc.RevokeTokenOf(r.Context(), target, tokenID); err != nil {
		code, refusal := identity.CodeOf(err)
		if !refusal {
			s.internalError(w, r, err, "revoking an account's token from a page failed")
			return
		}
		auth.Record(r.Context(), s.auditor, auth.Audit{
			AccountID: admin.ID, Action: auth.ActionAdmin,
			TargetType: "api_token", TargetID: tokenID, Reason: "revoke: " + string(code),
		})
		s.renderAdmin(w, r, admin, sess, statusFor(code), "There is no token by that id on that account.")
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "api_token", TargetID: tokenID, Allowed: true, Reason: "revoke",
	})
	http.Redirect(w, r, "/admin/accounts", http.StatusSeeOther)
}

// adminFailure records a refused admin action and re renders the page carrying
// the reason. A fault is not a refusal and is answered as one internal error.
func (s *Server) adminFailure(w http.ResponseWriter, r *http.Request, admin auth.Account, sess auth.Session,
	target, action string, err error,
) {
	code, refusal := identity.CodeOf(err)
	if !refusal && !errors.Is(err, identity.ErrNotFound) {
		s.internalError(w, r, err, "an admin page action failed")
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "account", TargetID: target, Reason: action + ": " + string(code),
	})
	s.renderAdmin(w, r, admin, sess, http.StatusNotFound, "There is no account by that id.")
}

// renderAdmin reads the accounts and their tokens and renders the page.
//
// The tokens are read per account rather than in one join, because there is no
// statement that returns them together and this feature does not get a query of
// its own. The list is every account the platform has, which on an internal
// platform is tens of rows, so the extra reads are cheaper than the invariant
// they would cost (spec 0013, Key invariants).
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, admin auth.Account, sess auth.Session,
	status int, message string,
) {
	accounts, err := s.svc.ListAccounts(r.Context())
	if err != nil {
		s.internalError(w, r, err, "listing accounts for a page failed")
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "accounts", Allowed: true, Reason: "list",
	})

	data := adminPageData{Accounts: make([]adminAccountRow, 0, len(accounts)), Message: message}
	for _, a := range accounts {
		row := adminAccountRow{
			ID: a.ID, Email: a.Email, Name: a.DisplayName,
			Verified: a.Verified, IsAdmin: a.IsAdmin, Disabled: a.Disabled,
			Created: a.CreatedAt, Self: a.ID == admin.ID,
		}
		tokens, err := s.svc.ListTokens(r.Context(), a.ID)
		if err != nil {
			s.internalError(w, r, err, "listing an account's tokens for the admin page failed")
			return
		}
		row.Tokens = tokens
		data.Accounts = append(data.Accounts, row)
	}
	s.render(w, r, admin, sess, status, "admin", "admin", data)
}
