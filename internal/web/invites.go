package web

import (
	"errors"
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// invitesPageData is the invite list plus the mint form. It is the admin's whole
// view of who may still join.
type invitesPageData struct {
	Invites []identity.InviteView
	Message string
	// Minted is the link a mint just produced, shown on this one render and
	// never again: it is not stored, not logged, and not in any later page
	// (AC-6, AC-14).
	Minted string
	// MintedNote is what the admin typed about that invite, so the link on the
	// page is identifiable when several were minted in a row.
	MintedNote string
}

func (s *Server) adminInvitesPage(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	s.renderInvites(w, r, admin, sess, http.StatusOK, invitesPageData{})
}

// adminInviteMint issues one invite and shows its link exactly once.
//
// It renders rather than redirecting, because a redirect could only carry the
// link in a query string, which puts a live credential in browser history and
// every log between here and the browser.
func (s *Server) adminInviteMint(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, admin, sess) {
		return
	}
	issued, err := s.svc.IssueInvite(r.Context(), admin.ID, r.PostFormValue("note"))
	if err != nil {
		code, refusal := identity.CodeOf(err)
		if !refusal {
			s.internalError(w, r, err, "minting an invite from a page failed")
			return
		}
		auth.Record(r.Context(), s.auditor, auth.Audit{
			AccountID: admin.ID, Action: auth.ActionAdmin,
			TargetType: "invite", Reason: "issue: " + string(code),
		})
		var e *identity.Error
		errors.As(err, &e)
		s.renderInvites(w, r, admin, sess, statusFor(code), invitesPageData{Message: e.Message})
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "invite", TargetID: issued.ID, Allowed: true, Reason: "issue",
	})
	s.renderInvites(w, r, admin, sess, http.StatusOK, invitesPageData{
		Minted: issued.Link, MintedNote: issued.Note,
	})
}

// adminInviteRevoke pulls a live invite back. One that is already spent or
// expired is refused not_found and changes nothing, and the refusal is audited
// exactly as adminFailure audits one on the accounts page (AC-7, AC-15).
func (s *Server) adminInviteRevoke(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.adminSession(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, admin, sess) {
		return
	}
	target := r.PathValue("id")
	if err := s.svc.RevokeInvite(r.Context(), target); err != nil {
		code, refusal := identity.CodeOf(err)
		if !refusal {
			s.internalError(w, r, err, "revoking an invite from a page failed")
			return
		}
		auth.Record(r.Context(), s.auditor, auth.Audit{
			AccountID: admin.ID, Action: auth.ActionAdmin,
			TargetType: "invite", TargetID: target, Reason: "revoke: " + string(code),
		})
		s.renderInvites(w, r, admin, sess, statusFor(code), invitesPageData{
			Message: "That invite is not live, so there was nothing to revoke.",
		})
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "invite", TargetID: target, Allowed: true, Reason: "revoke",
	})
	http.Redirect(w, r, "/admin/invites", http.StatusSeeOther)
}

// renderInvites reads the list and renders the page, carrying whatever message
// or freshly minted link the caller passed in.
func (s *Server) renderInvites(w http.ResponseWriter, r *http.Request, admin auth.Account, sess auth.Session,
	status int, data invitesPageData,
) {
	invites, err := s.svc.ListInvites(r.Context())
	if err != nil {
		s.internalError(w, r, err, "listing invites for a page failed")
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "invites", Allowed: true, Reason: "list",
	})
	data.Invites = invites
	s.render(w, r, admin, sess, status, "invites", "invites", data)
}
