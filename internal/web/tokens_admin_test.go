package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// TestMintingShowsTheRawValueExactlyOnce is AC-22. The panel is the last place
// the value exists outside a clipboard, so the two things to prove are that it
// is there once and that no later request brings it back. covers: AC-22
func TestMintingShowsTheRawValueExactlyOnce(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "mint@example.test")

	rec := h.post(t, "/tokens", url.Values{
		"name": {"laptop agent"}, "csrf": {h.csrfFor(t, cookie)},
	}, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("minting: got %d, want 200: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	raw := rawTokenIn(t, body)
	if !strings.Contains(body, "not be shown again") {
		t.Error("the one time panel does not warn that the value will not come back")
	}
	// The value never travels in a URL, which is where it would end up in a
	// browser's history and in every proxy log on the way.
	if strings.Contains(rec.Header().Get("Location"), raw) {
		t.Error("the raw token was put in a redirect URL")
	}

	// Reload the list: the value must be gone, and only the prefix left.
	again := h.get(t, "/tokens", cookie).Body.String()
	if strings.Contains(again, raw) {
		t.Error("the raw token is rendered again on a later request")
	}
	if !strings.Contains(again, "laptop agent") {
		t.Error("the minted token is missing from the list")
	}
}

// TestMintingRefusesAnExpiryItCannotHonour covers both ways expires_days can be
// wrong: text the handler rejects itself, and a number the service refuses. Both
// answer 422 and leave the person on the page with the reason, and neither mints.
// covers: AC-22
func TestMintingRefusesAnExpiryItCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		name string
		days string
	}{
		{"not a number", "soon"},
		{"beyond the ceiling", "366"},
		{"negative", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			cookie := h.signIn(t, "expiry@example.test")

			rec := h.post(t, "/tokens", url.Values{
				"name": {"unminted marker"}, "expires_days": {tc.days}, "csrf": {h.csrfFor(t, cookie)},
			}, cookie, nil)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("minting with expires_days=%q: got %d, want 422", tc.days, rec.Code)
			}
			// The assertion is that nothing was minted, not only that the
			// response was 422.
			if again := h.get(t, "/tokens", cookie).Body.String(); strings.Contains(again, "unminted marker") {
				t.Error("a token was minted despite the refusal")
			}
		})
	}
}

// TestTheTokenListLeaksNeitherValueNorHash is AC-21's closing clause. A prefix
// is not a credential; a hash is what an offline attack starts from.
// covers: AC-21
func TestTheTokenListLeaksNeitherValueNorHash(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "list-tokens@example.test")
	account := h.accountID(t, cookie)

	minted, err := h.srv.svc.MintToken(t.Context(),
		identity.Account{ID: account, Email: "list-tokens@example.test", Verified: true}, "agent", 30)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	body := h.get(t, "/tokens", cookie).Body.String()
	if strings.Contains(body, minted.Raw) {
		t.Error("the token list rendered the raw value")
	}
	if strings.Contains(body, auth.HashToken(minted.Raw)) {
		t.Error("the token list rendered the token hash")
	}
	if !strings.Contains(body, minted.Token.Prefix) {
		t.Error("the token list does not show the prefix it promises")
	}
}

// TestRevokingAnotherAccountsTokenIsNotFound is AC-23: the same answer an id
// that does not exist gets, so the page cannot be used to learn which ids are
// real. covers: AC-23
func TestRevokingAnotherAccountsTokenIsNotFound(t *testing.T) {
	h := newHarness(t, nil)
	owner := h.signIn(t, "owner-token@example.test")
	other := h.signIn(t, "other-token@example.test")

	minted, err := h.srv.svc.MintToken(t.Context(),
		identity.Account{ID: h.accountID(t, owner), Email: "owner-token@example.test", Verified: true},
		"theirs", 0)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	foreign := h.post(t, "/tokens/"+minted.Token.ID+"/revoke",
		url.Values{"csrf": {h.csrfFor(t, other)}}, other, nil)
	unknown := h.post(t, "/tokens/tok_does_not_exist/revoke",
		url.Values{"csrf": {h.csrfFor(t, other)}}, other, nil)

	if foreign.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("foreign %d, unknown %d, want 404 and 404", foreign.Code, unknown.Code)
	}
	// And the owner's token is untouched, which is the part a status code alone
	// does not prove.
	tokens, err := h.srv.svc.ListTokens(t.Context(), h.accountID(t, owner))
	if err != nil {
		t.Fatalf("listing tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Errorf("the owner has %d live tokens, want 1", len(tokens))
	}
}

// TestRevokingYourOwnTokenStopsItAuthenticating is AC-23's happy path: the
// revoke has to reach the credential, not just the row on the page.
// covers: AC-23
func TestRevokingYourOwnTokenStopsItAuthenticating(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "revoke@example.test")
	account := h.accountID(t, cookie)

	minted, err := h.srv.svc.MintToken(t.Context(),
		identity.Account{ID: account, Email: "revoke@example.test", Verified: true}, "mine", 0)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}
	rec := h.post(t, "/tokens/"+minted.Token.ID+"/revoke",
		url.Values{"csrf": {h.csrfFor(t, cookie)}}, cookie, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoking: got %d, want 303", rec.Code)
	}
	if _, err := h.srv.auth.Authenticate(t.Context(), minted.Raw, ""); err == nil {
		t.Error("the revoked token still authenticates")
	}
	row, ok := h.audit.last(auth.ActionTokenRevoke)
	if !ok || !row.Allowed || row.TargetID != minted.Token.ID {
		t.Errorf("revoking wrote %+v, want an allowed row naming the token", row)
	}
}

// TestAdminPageRefusesAnOrdinaryAccountAndRedirectsAVisitor is AC-24. Being
// signed out is not a refusal, it is not having answered yet, so the two get
// different answers on purpose. covers: AC-24
func TestAdminPageRefusesAnOrdinaryAccountAndRedirectsAVisitor(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test") // the first account registered is the administrator
	ordinary := h.signIn(t, "second@example.test")

	if got := h.get(t, "/admin/accounts", admin); got.Code != http.StatusOK {
		t.Fatalf("the admin's own accounts page: got %d, want 200", got.Code)
	}

	refused := h.get(t, "/admin/accounts", ordinary)
	if refused.Code != http.StatusForbidden {
		t.Errorf("an ordinary account: got %d, want 403", refused.Code)
	}
	row, ok := h.audit.last(auth.ActionAdmin)
	if !ok || row.Reason != string(identity.CodeAdminRequired) {
		t.Errorf("an ordinary account wrote %+v, want an admin_required row", row)
	}

	visitor := h.get(t, "/admin/accounts", nil)
	if visitor.Code != http.StatusSeeOther {
		t.Errorf("a signed out visitor: got %d, want 303", visitor.Code)
	}
	if got := visitor.Header().Get("Location"); !strings.HasPrefix(got, "/login") {
		t.Errorf("a signed out visitor goes to %q, want /login", got)
	}
}

// TestAdminPageListsEveryAccount is AC-24's listing half.
// covers: AC-24
func TestAdminPageListsEveryAccount(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	h.signIn(t, "second@example.test")

	body := h.get(t, "/admin/accounts", admin).Body.String()
	for _, want := range []string{"first@example.test", "second@example.test"} {
		if !strings.Contains(body, want) {
			t.Errorf("the accounts page does not list %q", want)
		}
	}
}

// TestDisableNeedsTheTypedAddressToMatch is AC-25. The typed address is a
// confirmation, not an authorization: it is what stops a misclick on a dense
// table from signing somebody else out of everything. covers: AC-25
func TestDisableNeedsTheTypedAddressToMatch(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	victim := h.signIn(t, "second@example.test")
	target := h.accountID(t, victim)

	mismatched := h.post(t, "/admin/accounts/"+target+"/disable", url.Values{
		"confirm_email": {"someoneelse@example.test"}, "csrf": {h.csrfFor(t, admin)},
	}, admin, nil)
	if mismatched.Code != http.StatusUnprocessableEntity {
		t.Errorf("a mismatched confirmation: got %d, want 422", mismatched.Code)
	}
	if !h.audit.hasReason(auth.ActionAdmin, "suspend: confirmation_mismatch") {
		t.Errorf("a mismatched confirmation wrote no confirmation_mismatch row: %+v", h.audit.all())
	}
	// Nothing changed: the account still signs in.
	if got := h.get(t, "/apps", victim); got.Code != http.StatusOK {
		t.Errorf("a mismatched confirmation still disabled the account: /apps got %d", got.Code)
	}
}

// TestDisableRevokesEverythingAndEnableLetsThemBackIn is AC-25's acted out path,
// including that revocation is one way: the sessions held before stay dead.
// covers: AC-25
func TestDisableRevokesEverythingAndEnableLetsThemBackIn(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	victim := h.signIn(t, "second@example.test")
	target := h.accountID(t, victim)

	disable := h.post(t, "/admin/accounts/"+target+"/disable", url.Values{
		// Case is not what is being confirmed here, the address is.
		"confirm_email": {"SECOND@example.test"}, "csrf": {h.csrfFor(t, admin)},
	}, admin, nil)
	if disable.Code != http.StatusSeeOther {
		t.Fatalf("disabling: got %d, want 303: %s", disable.Code, disable.Body)
	}
	row, ok := h.audit.last(auth.ActionAdmin)
	if !ok || !row.Allowed || row.TargetID != target || row.Reason != "suspend" {
		t.Errorf("disabling wrote %+v, want an allowed disable row naming the target", row)
	}
	if got := h.get(t, "/apps", victim); got.Code != http.StatusSeeOther {
		t.Errorf("a disabled account's session still renders /apps: got %d", got.Code)
	}

	enable := h.post(t, "/admin/accounts/"+target+"/enable",
		url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil)
	if enable.Code != http.StatusSeeOther {
		t.Fatalf("enabling: got %d, want 303", enable.Code)
	}
	// Enabling does not resurrect the old session, and it should not: revocation
	// is one way.
	if got := h.get(t, "/apps", victim); got.Code != http.StatusSeeOther {
		t.Error("enabling brought a revoked session back to life")
	}
	if h.login(t, "second@example.test") == nil {
		t.Error("an enabled account cannot sign in again")
	}
}

// TestAdminRevokesAnotherAccountsToken is AC-25's third write, and the pair that
// has to match: a token id belonging to a different account than the path names
// is not found, never a silent revocation of the wrong row. covers: AC-25
func TestAdminRevokesAnotherAccountsToken(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	other := h.signIn(t, "second@example.test")
	adminID, otherID := h.accountID(t, admin), h.accountID(t, other)

	minted, err := h.srv.svc.MintToken(t.Context(),
		identity.Account{ID: otherID, Email: "second@example.test", Verified: true}, "theirs", 0)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	// The right token under the wrong account is not found, and revokes nothing.
	wrongPair := h.post(t, "/admin/accounts/"+adminID+"/tokens/"+minted.Token.ID+"/revoke",
		url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil)
	if wrongPair.Code != http.StatusNotFound {
		t.Errorf("a mismatched account and token: got %d, want 404", wrongPair.Code)
	}
	if _, err := h.srv.auth.Authenticate(t.Context(), minted.Raw, ""); err != nil {
		t.Error("a mismatched pair revoked the token anyway")
	}

	right := h.post(t, "/admin/accounts/"+otherID+"/tokens/"+minted.Token.ID+"/revoke",
		url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil)
	if right.Code != http.StatusSeeOther {
		t.Fatalf("revoking: got %d, want 303: %s", right.Code, right.Body)
	}
	if _, err := h.srv.auth.Authenticate(t.Context(), minted.Raw, ""); err == nil {
		t.Error("the revoked token still authenticates")
	}
	row, ok := h.audit.last(auth.ActionAdmin)
	if !ok || !row.Allowed || row.TargetID != minted.Token.ID {
		t.Errorf("an admin revoke wrote %+v, want an allowed row naming the token", row)
	}
}

// TestAdminActionsAreCSRFGuardedToo keeps the three writes that touch somebody
// else's account behind the same guard every other POST is behind.
// covers: AC-12, AC-25
func TestAdminActionsAreCSRFGuardedToo(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	victim := h.signIn(t, "second@example.test")
	target := h.accountID(t, victim)

	for _, path := range []string{
		"/admin/accounts/" + target + "/disable",
		"/admin/accounts/" + target + "/enable",
		"/admin/accounts/" + target + "/tokens/tok_1/revoke",
	} {
		rec := h.post(t, path, url.Values{"confirm_email": {"second@example.test"}}, admin, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with no csrf value: got %d, want 403", path, rec.Code)
		}
	}
	if got := h.get(t, "/apps", victim); got.Code != http.StatusOK {
		t.Error("an unguarded admin post disabled the account anyway")
	}
}

// rawTokenIn pulls the minted value out of the one time panel.
func rawTokenIn(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "dpl_")
	if !ok {
		t.Fatal("the one time panel shows no token value")
	}
	value, _, _ := strings.Cut(rest, "<")
	return "dpl_" + strings.TrimSpace(value)
}
