package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// auditText reads every audit row back as one string, column by column, so a
// test can ask whether a value leaked into any of them without naming the column
// it would have leaked into. NULLs read as the empty string, which is what a row
// that never carried the value looks like.
func auditText(t *testing.T, h *harness) string {
	t.Helper()
	rows, err := h.store.DB().QueryContext(t.Context(),
		`SELECT id, coalesce(account_id,''), action, coalesce(target_type,''),
		        coalesce(target_id,''), outcome, coalesce(reason,'')
		 FROM audit_log`)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("closing the audit rows: %v", err)
		}
	}()
	var all strings.Builder
	for rows.Next() {
		var id, account, action, targetType, targetID, outcome, reason string
		if err := rows.Scan(&id, &account, &action, &targetType, &targetID, &outcome, &reason); err != nil {
			t.Fatalf("scanning an audit row: %v", err)
		}
		all.WriteString(strings.Join([]string{
			id, account, action, targetType, targetID, outcome, reason,
		}, " ") + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("walking the audit rows: %v", err)
	}
	return all.String()
}

// TestNoAuditRowCarriesTheBoundAddress is AC-13. Binding an invite to a person
// puts their address on the invite row, and the audit log is the one place it
// must not follow, because those rows are the record an operator reads and they
// keep the shape spec 0015 gave them. All three actions a bound invite can
// reach run here, issue, revoke and spend, so a leak in any of them fails this.
// covers: AC-13, AC-12
func TestNoAuditRowCarriesTheBoundAddress(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	// One invite that gets revoked, and one that gets spent.
	revoked := readBody(t, mintBound(t, h, admin, "for Dana", "dana@example.test"))
	spent := readBody(t, mintBound(t, h, admin, "for Sam", "sam@example.test"))

	before, err := h.store.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing invites: %v", err)
	}
	var revokedID string
	for _, inv := range before {
		if inv.Email != nil && *inv.Email == "dana@example.test" {
			revokedID = inv.ID
		}
	}
	if revokedID == "" {
		t.Fatal("the bound mint for dana@example.test is not on the list")
	}
	if rec := h.post(t, "/admin/invites/"+revokedID+"/revoke", url.Values{
		"csrf": {h.csrfFor(t, admin)},
	}, admin, nil); rec.Code != http.StatusOK && rec.Code != http.StatusSeeOther {
		t.Fatalf("revoking a bound invite: got %d", rec.Code)
	}

	code := codeFromLink(t, mintedLink(t, spent))
	if rec := h.post(t, "/register", url.Values{
		"invite": {code}, "email": {"sam@example.test"},
		"password": {testPassword}, "display_name": {"Sam"},
	}, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("spending a bound invite: got %d", rec.Code)
	}

	// Both sinks, because a bound invite's three actions do not all write to the
	// same one: issue and revoke are the edge's rows, and the spend is written
	// inside the store transaction that made the account.
	rows := auditText(t, h)
	for _, a := range h.audit.all() {
		rows += strings.Join([]string{
			a.AccountID, a.Action, a.TargetType, a.TargetID, a.Reason, a.ClientAddress,
		}, " ") + "\n"
	}
	for _, leak := range []string{"dana@example.test", "sam@example.test", code,
		codeFromLink(t, mintedLink(t, revoked))} {
		if strings.Contains(rows, leak) {
			t.Errorf("an audit row carries %q, which only the invite row may hold:\n%s", leak, rows)
		}
	}
	// The link is the invite id, so an operator can still get from a row to the
	// address deliberately. Losing that would make the rows useless rather than
	// private.
	if !strings.Contains(rows, revokedID) {
		t.Errorf("no audit row names the revoked invite, so nothing links a row to its address:\n%s", rows)
	}
}

// TestThereIsNoResendControl is AC-17. The platform holds a hash, never the
// code, so it cannot reproduce a message it already sent, and offering a control
// that looked like it could would be a lie. Recovering a lost invite is a revoke
// plus a fresh mint, and this fails the moment somebody adds the button that
// looks helpful.
// covers: AC-17
func TestThereIsNoResendControl(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	link := mintedLink(t, readBody(t, mintBound(t, h, admin, "for Sam", "sam@example.test")))

	page := h.get(t, "/admin/invites", admin).Body.String()
	for _, control := range []string{"resend", "Resend", "send again", "Send again"} {
		if strings.Contains(page, control) {
			t.Errorf("the invite list offers %q, but the platform cannot reproduce a sent link", control)
		}
	}
	// And the reason it cannot: the link itself is gone from every later render.
	if strings.Contains(page, link) || strings.Contains(page, codeFromLink(t, link)) {
		t.Error("the invite list rendered the link again, so the code outlived its one showing")
	}
}

// TestALiveBoundCodeRendersLikeAnUnknownOne is AC-18. Binding an invite adds a
// second thing the register page could be tempted to tell a visitor, whether the
// code is live and whose it is, so the page has to keep answering nothing at
// all. Every distinction is still made on the post.
// covers: AC-18
func TestALiveBoundCodeRendersLikeAnUnknownOne(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	live := codeFromLink(t, mintedLink(t, readBody(t,
		mintBound(t, h, admin, "for Sam", "sam@example.test"))))

	render := func(code string) string {
		body := h.get(t, "/register?invite="+url.QueryEscape(code)).Body.String()
		// The page echoes whatever code it was handed into the hidden field, so
		// the codes are blanked before the two are compared.
		return withoutCSRF(strings.ReplaceAll(body, code, "CODE"))
	}
	if render(live) != render("not-a-real-code") {
		t.Error("a live bound code renders differently from an unknown one, so the page validates")
	}
	if strings.Contains(render(live), "sam@example.test") {
		t.Error("the register page prefilled the bound address")
	}
}
