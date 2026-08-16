package httpapi

import (
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// adminListInvites is the admin view of who may still join. It never carries a
// raw code: the shape has no field for one, so it cannot leak by being
// forgotten (AC-8, AC-14).
func (i *Identity) adminListInvites(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, ok := i.adminSession(w, r)
	if !ok {
		return
	}
	invites, err := i.svc.ListInvites(ctx)
	if err != nil {
		i.fail(ctx, w, err)
		return
	}
	bodies := make([]map[string]any, 0, len(invites))
	for _, inv := range invites {
		bodies = append(bodies, map[string]any{
			"id":        inv.ID,
			"note":      inv.Note,
			"email":     inv.Email,
			"issued_by": inv.IssuedBy,
			"spent_by":  inv.SpentBy,
			"expires":   inv.ExpiresAt,
			"created":   inv.CreatedAt,
			"state":     string(inv.State),
		})
	}
	auth.Record(ctx, i.auditor, auth.Audit{
		ClientAddress: i.clientAddress(r),
		AccountID:     admin.ID, Action: auth.ActionAdmin,
		TargetType: "invites", Allowed: true, Reason: "list",
	})
	writeJSON(ctx, w, http.StatusOK, map[string]any{"invites": bodies})
}

// adminMintInvite issues one invite. The link is in this response and nowhere
// else, ever again: it is not stored, not logged, and not audited (AC-6, AC-14).
func (i *Identity) adminMintInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, ok := i.adminSession(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
		// Email is optional and binds the invite to that address, mailing the
		// link to it. It takes the same validation, the same refusals and the
		// same inline send the page does, so neither surface can mint an invite
		// the other cannot (spec 0025, AC-15).
		Email string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	issued, err := i.svc.IssueInvite(ctx, admin.ID, body.Note, body.Email)
	if err != nil {
		code, _ := identity.CodeOf(err)
		auth.Record(ctx, i.auditor, auth.Audit{
			ClientAddress: i.clientAddress(r),
			AccountID:     admin.ID, Action: auth.ActionAdmin,
			TargetType: "invite", Reason: "issue: " + string(code),
		})
		i.fail(ctx, w, err)
		return
	}
	auth.Record(ctx, i.auditor, auth.Audit{
		ClientAddress: i.clientAddress(r),
		AccountID:     admin.ID, Action: auth.ActionAdmin,
		TargetType: "invite", TargetID: issued.ID, Allowed: true, Reason: "issue",
	})
	writeJSON(ctx, w, http.StatusCreated, map[string]any{
		"id":      issued.ID,
		"note":    issued.Note,
		"expires": issued.ExpiresAt,
		"email":   issued.Email,
		"sent":    issued.Sent,
		// The one and only time this value leaves the platform, other than the
		// message a bound mint sends to its own address (spec 0025, AC-12).
		"link": issued.Link,
	})
}

// adminRevokeInvite pulls a live invite back. One that is already spent or
// expired is 404 and changes nothing, because the store's guard carries the same
// live predicate the spend does (AC-7).
func (i *Identity) adminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, ok := i.adminSession(w, r)
	if !ok {
		return
	}
	target := r.PathValue("id")
	if err := i.svc.RevokeInvite(ctx, target); err != nil {
		code, _ := identity.CodeOf(err)
		auth.Record(ctx, i.auditor, auth.Audit{
			ClientAddress: i.clientAddress(r),
			AccountID:     admin.ID, Action: auth.ActionAdmin,
			TargetType: "invite", TargetID: target, Reason: "revoke: " + string(code),
		})
		i.fail(ctx, w, err)
		return
	}
	auth.Record(ctx, i.auditor, auth.Audit{
		ClientAddress: i.clientAddress(r),
		AccountID:     admin.ID, Action: auth.ActionAdmin,
		TargetType: "invite", TargetID: target, Allowed: true, Reason: "revoke",
	})
	w.WriteHeader(http.StatusNoContent)
}
