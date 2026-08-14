package httpapi

import (
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// mintToken creates an API token for the calling account. The raw value is in
// this response and nowhere else, ever again (AC-12).
func (i *Identity) mintToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, _, ok := i.session(w, r)
	if !ok {
		return
	}
	var body struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if !decode(w, r, &body) {
		return
	}

	minted, err := i.svc.MintToken(ctx, identity.Account{
		ID: account.ID, Email: account.Email, Verified: account.Verified,
	}, body.Name, body.ExpiresInDays)
	if err != nil {
		code, _ := identity.CodeOf(err)
		auth.Record(ctx, i.auditor, auth.Audit{
			AccountID: account.ID, Action: auth.ActionTokenMint, Reason: string(code),
		})
		i.fail(ctx, w, err)
		return
	}

	auth.Record(ctx, i.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionTokenMint,
		TargetType: "api_token", TargetID: minted.Token.ID, Allowed: true,
	})
	writeJSON(ctx, w, http.StatusCreated, map[string]any{
		"id":     minted.Token.ID,
		"name":   minted.Token.Name,
		"prefix": minted.Token.Prefix,
		// The one and only time this value leaves the platform. It is not logged,
		// not audited, and not stored in the clear anywhere (AC-27).
		"token": minted.Raw,
	})
}

// listTokens reads the calling account's live tokens, newest first. The account
// is the caller's own resolved identity, never anything they sent, so this cannot
// list somebody else's (AC-13).
func (i *Identity) listTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, _, ok := i.session(w, r)
	if !ok {
		return
	}
	tokens, err := i.svc.ListTokens(ctx, account.ID)
	if err != nil {
		i.fail(ctx, w, err)
		return
	}
	writeJSON(ctx, w, http.StatusOK, map[string]any{"tokens": tokenBodies(tokens)})
}

// revokeToken kills a token the caller owns. Somebody else's id answers 404, the
// same answer an unknown id gets (AC-14).
func (i *Identity) revokeToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, _, ok := i.session(w, r)
	if !ok {
		return
	}
	tokenID := r.PathValue("id")
	if err := i.svc.RevokeToken(ctx, account.ID, tokenID); err != nil {
		code, _ := identity.CodeOf(err)
		auth.Record(ctx, i.auditor, auth.Audit{
			AccountID: account.ID, Action: auth.ActionTokenRevoke,
			TargetType: "api_token", TargetID: tokenID, Reason: string(code),
		})
		i.fail(ctx, w, err)
		return
	}
	auth.Record(ctx, i.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionTokenRevoke,
		TargetType: "api_token", TargetID: tokenID, Allowed: true,
	})
	w.WriteHeader(http.StatusNoContent)
}

// adminListAccounts is the whole admin view of who registered (AC-19).
func (i *Identity) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, ok := i.adminSession(w, r)
	if !ok {
		return
	}
	accounts, err := i.svc.ListAccounts(ctx)
	if err != nil {
		i.fail(ctx, w, err)
		return
	}
	bodies := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		bodies = append(bodies, map[string]any{
			"id":       a.ID,
			"email":    a.Email,
			"name":     a.DisplayName,
			"verified": a.Verified,
			"is_admin": a.IsAdmin,
			"disabled": a.Disabled,
			"created":  a.CreatedAt,
		})
	}
	auth.Record(ctx, i.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "accounts", Allowed: true, Reason: "list",
	})
	writeJSON(ctx, w, http.StatusOK, map[string]any{"accounts": bodies})
}

// adminDisable suspends an account: it revokes its sessions and links, and stops
// every app it runs.
func (i *Identity) adminDisable(w http.ResponseWriter, r *http.Request) {
	i.setDisabled(w, r, true, "suspend")
}

// adminEnable restores a suspended account and starts its apps again on the
// image each was already serving. Revocation is one way, so the sessions it held
// before stay dead.
func (i *Identity) adminEnable(w http.ResponseWriter, r *http.Request) {
	i.setDisabled(w, r, false, "restore")
}

// setDisabled is both admin state changes, which differ only in the flag and the
// word recorded. Both go through internal/suspend, the same use case the admin
// page calls, so the two surfaces stop the same apps and write the same audit
// rows (spec 0018, AC-19).
//
// A partial outcome is answered with 200 and the slugs rather than 204, because
// the account did change state and the caller has something to act on
// (spec 0018, AC-6).
func (i *Identity) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool, action string) {
	ctx := r.Context()
	admin, ok := i.adminSession(w, r)
	if !ok {
		return
	}
	target := r.PathValue("id")
	// Suspending yourself revokes the session or token making the call and stops
	// your own apps, with nobody left signed in to undo either. The same refusal
	// the page makes, because both surfaces answer for the same rule
	// (spec 0018, AC-17, AC-19).
	if disabled && target == admin.ID {
		auth.Record(ctx, i.auditor, auth.Audit{
			AccountID: admin.ID, Action: auth.ActionAdmin,
			TargetType: "account", TargetID: target, Reason: "suspend: self",
		})
		writeError(ctx, w, http.StatusUnprocessableEntity, "an admin cannot suspend their own account")
		return
	}
	change := i.suspension.Restore
	if disabled {
		change = i.suspension.Suspend
	}
	result, err := change(ctx, admin.ID, target)
	if err != nil {
		code, _ := identity.CodeOf(err)
		auth.Record(ctx, i.auditor, auth.Audit{
			AccountID: admin.ID, Action: auth.ActionAdmin,
			TargetType: "account", TargetID: target, Reason: action + ": " + string(code),
		})
		i.fail(ctx, w, err)
		return
	}
	if len(result.NotStopped) > 0 {
		writeJSON(ctx, w, http.StatusOK, map[string]any{"apps_not_stopped": result.NotStopped})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminRevokeToken kills another account's token, which is the one thing an admin
// may do to a credential that is not theirs. The pair has to match: a token id
// that belongs to a different account than the path names is 404, not a silent
// revocation of the wrong row.
func (i *Identity) adminRevokeToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin, ok := i.adminSession(w, r)
	if !ok {
		return
	}
	target, tokenID := r.PathValue("id"), r.PathValue("tokenId")
	if err := i.svc.RevokeTokenOf(ctx, target, tokenID); err != nil {
		code, _ := identity.CodeOf(err)
		auth.Record(ctx, i.auditor, auth.Audit{
			AccountID: admin.ID, Action: auth.ActionAdmin,
			TargetType: "api_token", TargetID: tokenID, Reason: "revoke: " + string(code),
		})
		i.fail(ctx, w, err)
		return
	}
	auth.Record(ctx, i.auditor, auth.Audit{
		AccountID: admin.ID, Action: auth.ActionAdmin,
		TargetType: "api_token", TargetID: tokenID, Allowed: true, Reason: "revoke",
	})
	w.WriteHeader(http.StatusNoContent)
}

// tokenBodies projects token rows onto what a caller may see. There is no raw
// value and no hash in the shape at all, so neither can leak by being forgotten.
func tokenBodies(tokens []identity.TokenView) []map[string]any {
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, map[string]any{
			"id":        t.ID,
			"name":      t.Name,
			"prefix":    t.Prefix,
			"created":   t.CreatedAt,
			"last_used": t.LastUsedAt,
			"expires":   t.ExpiresAt,
		})
	}
	return out
}
