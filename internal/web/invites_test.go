package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
)

// TestTheRegisterPageNeverValidatesTheCode is AC-18: the page is not a second
// oracle. Every kind of code, good and bad alike, renders the identical form,
// and every distinction is made on the post.
// covers: AC-18
func TestTheRegisterPageNeverValidatesTheCode(t *testing.T) {
	h := newHarness(t, nil)

	live := h.invite(t)
	spent := h.invite(t)
	if rec := h.post(t, "/register", url.Values{
		"invite": {spent}, "email": {"spender@example.test"},
		"password": {testPassword}, "display_name": {"Someone"},
	}, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("spending an invite: got %d", rec.Code)
	}
	revoked := h.invite(t)
	row, err := h.store.LiveInvite(t.Context(), identity.HashSecret(revoked), "")
	if err != nil {
		t.Fatalf("reading the invite to revoke: %v", err)
	}
	if err := h.store.RevokeInvite(t.Context(), row.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	expired := h.invite(t)
	h.clock.T = h.clock.T.Add(identity.InviteLifetime + time.Hour)

	// Each page differs only by the code in its own hidden field, so comparing
	// the renders means blanking that one value out.
	var pages []string
	for _, code := range []string{live, spent, revoked, expired, "not-a-real-code"} {
		rec := h.get(t, "/register?invite="+url.QueryEscape(code), nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /register with a code: got %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `name="invite" value="`+code+`"`) {
			t.Errorf("the page dropped the code it was handed")
		}
		pages = append(pages, withoutCSRF(strings.ReplaceAll(rec.Body.String(), code, "CODE")))
	}
	for i, page := range pages {
		if page != pages[0] {
			t.Errorf("page %d renders differently from the one with a live code", i)
		}
	}
}

// TestTheRegisterPageSaysItIsInviteOnly is AC-16: a bare visit renders normally
// and explains why nothing happens without a link.
// covers: AC-16
func TestTheRegisterPageSaysItIsInviteOnly(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.get(t, "/register", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /register: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "by invitation") {
		t.Error("the register page does not say registration is invite only")
	}
}

// TestTheRegisterPageForbidsTheReferrer is the browser side half of AC-14: the
// code rides in the query string, so the page must not let it travel onward in
// a Referer header.
// covers: AC-14
func TestTheRegisterPageForbidsTheReferrer(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.get(t, "/register?invite=whatever", nil).Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("GET /register sends Referrer-Policy %q, want no-referrer", got)
	}
}

// TestTheInvitePagesNeedAnAdminSession is AC-9 on the browser surface.
// covers: AC-9
func TestTheInvitePagesNeedAnAdminSession(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t, "admin@example.test") // the first account is the admin
	ordinary := h.signIn(t, "ordinary@example.test")

	if rec := h.get(t, "/admin/invites", ordinary); rec.Code != http.StatusForbidden {
		t.Errorf("GET /admin/invites as an ordinary account: got %d, want 403", rec.Code)
	}
	for _, path := range []string{"/admin/invites", "/admin/invites/inv_nosuch/revoke"} {
		rec := h.post(t, path, url.Values{"csrf": {h.csrfFor(t, ordinary)}}, ordinary, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as an ordinary account: got %d, want 403", path, rec.Code)
		}
	}
}

// TestBothInviteMutationsCarryTheSynchroniserToken is AC-19: a post without the
// token is refused and changes nothing, exactly as the accounts page's two
// mutations are.
// covers: AC-19
func TestBothInviteMutationsCarryTheSynchroniserToken(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	if rec := h.post(t, "/admin/invites", url.Values{"note": {"no token"}}, admin, nil); rec.Code != http.StatusForbidden {
		t.Errorf("minting without the token: got %d, want 403", rec.Code)
	}
	rows, err := h.store.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(rows) != 1 { // the one signIn spent, and nothing else
		t.Errorf("a refused mint wrote %d extra invites", len(rows)-1)
	}

	minted := h.post(t, "/admin/invites",
		url.Values{"note": {"for Sam"}, "csrf": {h.csrfFor(t, admin)}}, admin, nil)
	if minted.Code != http.StatusOK {
		t.Fatalf("minting: got %d, want 200: %s", minted.Code, minted.Body)
	}
	live := liveInviteID(t, h)

	if rec := h.post(t, "/admin/invites/"+live+"/revoke", url.Values{}, admin, nil); rec.Code != http.StatusForbidden {
		t.Errorf("revoking without the token: got %d, want 403", rec.Code)
	}
	if liveInviteID(t, h) != live {
		t.Error("a refused revoke ended the invite anyway")
	}
}

// TestTheMintedLinkIsShownOnceAndThenNeverAgain is AC-6 and AC-14 on the page:
// the link is in the response that minted it and in no later render.
// covers: AC-6, AC-14
func TestTheMintedLinkIsShownOnceAndThenNeverAgain(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	minted := h.post(t, "/admin/invites",
		url.Values{"note": {"for Sam"}, "csrf": {h.csrfFor(t, admin)}}, admin, nil)
	if minted.Code != http.StatusOK {
		t.Fatalf("minting: got %d: %s", minted.Code, minted.Body)
	}
	body := minted.Body.String()
	if !strings.Contains(body, "/register?invite=") {
		t.Fatal("the mint response does not carry the link")
	}
	_, rest, _ := strings.Cut(body, "/register?invite=")
	code, _, _ := strings.Cut(rest, "<")

	again := h.get(t, "/admin/invites", admin)
	if strings.Contains(again.Body.String(), code) {
		t.Error("the invite list shows a raw code on a later render")
	}
	if !strings.Contains(again.Body.String(), "for Sam") {
		t.Error("the invite list does not show the note it was minted with")
	}
}

// TestARevokedInviteStaysOnTheListMarked is AC-7 and AC-8 on the page.
// covers: AC-7, AC-8
func TestARevokedInviteStaysOnTheListMarked(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	if rec := h.post(t, "/admin/invites",
		url.Values{"note": {"for Sam"}, "csrf": {h.csrfFor(t, admin)}}, admin, nil); rec.Code != http.StatusOK {
		t.Fatalf("minting: got %d", rec.Code)
	}
	live := liveInviteID(t, h)

	if rec := h.post(t, "/admin/invites/"+live+"/revoke",
		url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoking: got %d, want 303", rec.Code)
	}
	listed := h.get(t, "/admin/invites", admin).Body.String()
	if !strings.Contains(listed, "revoked") {
		t.Error("a revoked invite is not marked revoked on the list")
	}
	if !strings.Contains(listed, "spent by") {
		t.Error("the invite the admin spent registering is not shown as spent by them")
	}

	// Revoking it again is refused and changes nothing.
	if rec := h.post(t, "/admin/invites/"+live+"/revoke",
		url.Values{"csrf": {h.csrfFor(t, admin)}}, admin, nil); rec.Code != http.StatusNotFound {
		t.Errorf("revoking twice: got %d, want 404", rec.Code)
	}
}

// liveInviteID is the id of the one live invite in the table.
func liveInviteID(t *testing.T, h *harness) string {
	t.Helper()
	rows, err := h.store.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing invites: %v", err)
	}
	for _, r := range rows {
		if r.ConsumedAt == nil && r.RevokedAt == nil {
			return r.ID
		}
	}
	t.Fatal("no live invite in the table")
	return ""
}
