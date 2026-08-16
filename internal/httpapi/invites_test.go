package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// TestRegistrationNeedsALiveInvite is AC-1 and AC-2 through the JSON surface: a
// missing code and all four kinds of dead code are one status, one code and one
// sentence, and none of them writes an account.
// covers: AC-1, AC-2
func TestRegistrationNeedsALiveInvite(t *testing.T) {
	h := newIDHarness(t, true)

	spent := h.invite(t)
	if got := h.do(t, "POST", "/v1/auth/register",
		map[string]string{"invite": spent, "email": "spender@example.com", "password": goodPassword},
		nil); got.Code != http.StatusAccepted {
		t.Fatalf("spending an invite: got %d: %s", got.Code, got.Body)
	}

	revoked := h.invite(t)
	revokedRow, err := h.store.LiveInvite(t.Context(), identity.HashSecret(revoked), "")
	if err != nil {
		t.Fatalf("reading the invite to revoke: %v", err)
	}
	if err := h.store.RevokeInvite(t.Context(), revokedRow.ID); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	// Expiry needs no write at all: the code is minted, and the clock moves past
	// the seven days it was good for.
	expired := h.invite(t)
	h.clock.T = h.clock.T.Add(identity.InviteLifetime + time.Hour)

	before, err := h.store.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("listing accounts: %v", err)
	}

	var bodies []string
	for _, tc := range []struct{ name, code string }{
		{"no code at all", ""},
		{"an unknown code", "not-a-real-code"},
		{"a spent code", spent},
		{"a revoked code", revoked},
		{"an expired code", expired},
	} {
		rec := h.do(t, "POST", "/v1/auth/register",
			map[string]string{"invite": tc.code, "email": "nope@example.com", "password": goodPassword}, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403: %s", tc.name, rec.Code, rec.Body)
		}
		if got := codeOf(t, rec); got != "invite_invalid" {
			t.Errorf("%s: got code %q, want invite_invalid", tc.name, got)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i, body := range bodies {
		if body != bodies[0] {
			t.Errorf("refusal %d reads differently from the first:\n%s\n%s", i, bodies[0], body)
		}
	}

	after, err := h.store.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("listing accounts: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused registration wrote %d account rows", len(after)-len(before))
	}
}

// TestTheInviteIsCheckedBeforeAnythingElse is AC-11: the gate is never spoken
// past by a validation message, so a caller holding nothing is told about the
// invite rather than about their password, and never costs a key derivation.
// covers: AC-11
func TestTheInviteIsCheckedBeforeAnythingElse(t *testing.T) {
	h := newIDHarness(t, true)

	rec := h.do(t, "POST", "/v1/auth/register",
		map[string]string{"invite": "", "email": "not-an-address", "password": "short"}, nil)
	if rec.Code != http.StatusForbidden || codeOf(t, rec) != "invite_invalid" {
		t.Errorf("got %d %q, want 403 invite_invalid: %s", rec.Code, codeOf(t, rec), rec.Body)
	}
}

// TestAnInviteSurvivesATakenAddress is AC-10 through the surface: the answer is
// byte for byte a fresh registration's, nothing is created, and the link the
// person was handed still works.
// covers: AC-10
func TestAnInviteSurvivesATakenAddress(t *testing.T) {
	h := newIDHarness(t, true)
	h.registerAndVerify(t, "taken@example.com")

	code := h.invite(t)
	onTaken := h.do(t, "POST", "/v1/auth/register",
		map[string]string{"invite": code, "email": "taken@example.com", "password": goodPassword}, nil)
	if onTaken.Code != http.StatusAccepted {
		t.Fatalf("registering a taken address: got %d, want 202: %s", onTaken.Code, onTaken.Body)
	}

	onFree := h.do(t, "POST", "/v1/auth/register",
		map[string]string{"invite": code, "email": "free@example.com", "password": goodPassword}, nil)
	if onFree.Code != http.StatusAccepted {
		t.Fatalf("the invite did not survive: got %d, want 202: %s", onFree.Code, onFree.Body)
	}
	if onTaken.Body.String() != onFree.Body.String() {
		t.Errorf("the two answers differ:\n%s\n%s", onTaken.Body, onFree.Body)
	}
}

// TestSpendingAnInviteStampsTheAccountItCreated is AC-3: the invite and the
// account name each other, written in one transaction.
// covers: AC-3
func TestSpendingAnInviteStampsTheAccountItCreated(t *testing.T) {
	h := newIDHarness(t, true)
	h.registerAndVerify(t, "spender@example.com")

	rows, err := h.store.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing invites: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d invites, want 1", len(rows))
	}
	if rows[0].ConsumedAt == nil {
		t.Error("the invite was not stamped consumed")
	}
	if rows[0].SpenderEmail == nil || *rows[0].SpenderEmail != "spender@example.com" {
		t.Errorf("the invite names %v as its spender, want spender@example.com", rows[0].SpenderEmail)
	}
}

// TestTheInviteAdminSurfaceNeedsAnAdminSession is AC-9: an ordinary session is
// refused admin_required, and a bearer token never reaches these at all, because
// they read a session and a token is not one.
// covers: AC-9
func TestTheInviteAdminSurfaceNeedsAnAdminSession(t *testing.T) {
	h := newIDHarness(t, true)
	h.registerAndVerify(t, "admin@example.com") // the first account is the admin
	ordinary := h.registerAndVerify(t, "ordinary@example.com")

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/admin/invites"},
		{"POST", "/v1/admin/invites"},
		{"DELETE", "/v1/admin/invites/inv_nosuch"},
	} {
		rec := h.do(t, tc.method, tc.path, nil, ordinary)
		if rec.Code != http.StatusForbidden || codeOf(t, rec) != "admin_required" {
			t.Errorf("%s %s as an ordinary account: got %d %q, want 403 admin_required",
				tc.method, tc.path, rec.Code, codeOf(t, rec))
		}

		// A machine's bearer token is not a session, so it lands on the sign in
		// refusal rather than on the admin one.
		withToken := h.do(t, tc.method, tc.path, nil, nil)
		if withToken.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no session: got %d, want 401", tc.method, tc.path, withToken.Code)
		}
	}
}

// TestAnAdminIssuesListsAndRevokes is AC-6, AC-7 and AC-8: the link is in the
// mint response and nowhere else, the list carries the derived state, and a
// revoke is refused on anything that already ended.
// covers: AC-6, AC-7, AC-8
func TestAnAdminIssuesListsAndRevokes(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")

	minted := h.do(t, "POST", "/v1/admin/invites", map[string]string{"note": "for Sam"}, admin)
	if minted.Code != http.StatusCreated {
		t.Fatalf("minting: got %d, want 201: %s", minted.Code, minted.Body)
	}
	var issued struct{ ID, Note, Link string }
	if err := json.Unmarshal(minted.Body.Bytes(), &issued); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}
	if issued.Link == "" || issued.Note != "for Sam" {
		t.Fatalf("the mint body is %s", minted.Body)
	}

	// The list never carries a raw code, only what an admin needs to decide.
	listed := h.do(t, "GET", "/v1/admin/invites", nil, admin)
	if listed.Code != http.StatusOK {
		t.Fatalf("listing: got %d: %s", listed.Code, listed.Body)
	}
	if body := listed.Body.String(); containsCode(body, issued.Link) {
		t.Error("the invite list carries a raw code")
	}
	live := findInvite(t, listed.Body.Bytes(), issued.ID)
	if live.State != "live" {
		t.Errorf("the fresh invite lists as %q, want live", live.State)
	}
	if live.IssuedBy == "" || live.IssuedBy == "the platform" {
		t.Errorf("the fresh invite is issued by %q, want the admin who minted it", live.IssuedBy)
	}

	if rec := h.do(t, "DELETE", "/v1/admin/invites/"+issued.ID, nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("revoking: got %d, want 204: %s", rec.Code, rec.Body)
	}
	// It stays on the list, marked, rather than disappearing.
	listed = h.do(t, "GET", "/v1/admin/invites", nil, admin)
	if got := findInvite(t, listed.Body.Bytes(), issued.ID); got.State != "revoked" {
		t.Errorf("a revoked invite lists as %q, want revoked", got.State)
	}

	// Revoking it twice is not found, and so is an id that never existed.
	for _, id := range []string{issued.ID, "inv_nosuch"} {
		if rec := h.do(t, "DELETE", "/v1/admin/invites/"+id, nil, admin); rec.Code != http.StatusNotFound {
			t.Errorf("revoking %s again: got %d, want 404", id, rec.Code)
		}
	}
}

// TestAnOverLongNoteIsRefused is the bound AC-6 puts on the one caller supplied
// value this surface takes.
// covers: AC-6
func TestAnOverLongNoteIsRefused(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")

	long := make([]byte, identity.NoteLimit+1)
	for i := range long {
		long[i] = 'a'
	}
	rec := h.do(t, "POST", "/v1/admin/invites", map[string]string{"note": string(long)}, admin)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an over long note: got %d, want 422: %s", rec.Code, rec.Body)
	}
	// The code is the part a caller branches on, so it has to name the field that
	// was actually wrong. The note is not an address.
	if got := codeOf(t, rec); got != "note_too_long" {
		t.Errorf("an over long note answers %q, want note_too_long", got)
	}
}

// TestAnExpiredInviteListsAsExpired pins the derived half of AC-8 at the surface
// that shows it. Expiry is the one state no write produces: the row is untouched
// and only the clock moves, so a list that read a stored column would still say
// live here.
// covers: AC-8
func TestAnExpiredInviteListsAsExpired(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")

	minted := h.do(t, "POST", "/v1/admin/invites", map[string]string{"note": "for Sam"}, admin)
	if minted.Code != http.StatusCreated {
		t.Fatalf("minting: got %d, want 201: %s", minted.Code, minted.Body)
	}
	var issued struct{ ID string }
	if err := json.Unmarshal(minted.Body.Bytes(), &issued); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}

	h.clock.T = h.clock.T.Add(identity.InviteLifetime + time.Hour)

	listed := h.do(t, "GET", "/v1/admin/invites", nil, admin)
	if got := findInvite(t, listed.Body.Bytes(), issued.ID).State; got != "expired" {
		t.Errorf("an invite past its lifetime lists as %q, want expired", got)
	}

	// The invite the admin registered through is spent, and a spent row says so
	// rather than falling to expired now that the clock has passed it too.
	var list struct {
		Invites []inviteBody `json:"invites"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatalf("reading the list: %v", err)
	}
	var spentSeen bool
	for _, row := range list.Invites {
		if row.ID == issued.ID {
			continue
		}
		spentSeen = true
		if row.State != "spent" || row.SpentBy != "admin@example.com" {
			t.Errorf("the spent invite lists as %q spent by %q, want spent by admin@example.com",
				row.State, row.SpentBy)
		}
	}
	if !spentSeen {
		t.Error("no spent invite on the list, so nothing proved the spent state")
	}
}

// TestBootstrapMintsOnceAndOnlyOnEmpty is AC-13: the platform lets itself in on
// an empty database, and a restart against that same database mints nothing
// further.
// covers: AC-13
func TestBootstrapMintsOnceAndOnlyOnEmpty(t *testing.T) {
	h := newIDHarness(t, true)
	svc := identity.NewService(store.ForIdentity(h.store), h.mail, h.clock, identity.Options{
		ConsoleURL: "https://deploy.example.org",
		Hasher:     identity.NewHasherWith(2, 64, 1),
	})

	link, minted, err := svc.BootstrapInvite(t.Context())
	if err != nil || !minted || link == "" {
		t.Fatalf("the first boot minted nothing: %q %v %v", link, minted, err)
	}

	// Three restarts against the same database, each finding the live one it
	// already left behind.
	for i := range 3 {
		if _, minted, err := svc.BootstrapInvite(t.Context()); err != nil || minted {
			t.Errorf("restart %d minted another invite: %v %v", i, minted, err)
		}
	}

	// Once a person holds an account, an empty invite table is no longer a
	// reason to mint one.
	fresh := newIDHarness(t, true)
	fresh.registerAndVerify(t, "someone@example.com")
	freshSvc := identity.NewService(store.ForIdentity(fresh.store), fresh.mail, fresh.clock, identity.Options{
		ConsoleURL: "https://deploy.example.org",
		Hasher:     identity.NewHasherWith(2, 64, 1),
	})
	if _, minted, err := freshSvc.BootstrapInvite(t.Context()); err != nil || minted {
		t.Errorf("a database with an account minted a bootstrap invite: %v %v", minted, err)
	}
}

// TestABootstrapInviteRegistersTheFirstAdmin is the other half of AC-13: the
// link the platform logs actually works, and the account it makes is the admin
// through the existing first admin rule.
// covers: AC-13
func TestABootstrapInviteRegistersTheFirstAdmin(t *testing.T) {
	h := newIDHarness(t, true)
	raw, err := identity.NewSecret()
	if err != nil {
		t.Fatalf("drawing a code: %v", err)
	}
	if _, err := h.store.CreateInvite(t.Context(), store.NewInvite{
		CodeHash:  identity.HashSecret(raw),
		ExpiresAt: ids.Stamp(h.clock.Now().Add(identity.InviteLifetime)),
	}); err != nil {
		t.Fatalf("minting the bootstrap invite: %v", err)
	}

	if rec := h.do(t, "POST", "/v1/auth/register",
		map[string]string{"invite": raw, "email": "first@example.com", "password": goodPassword},
		nil); rec.Code != http.StatusAccepted {
		t.Fatalf("registering through the bootstrap invite: got %d: %s", rec.Code, rec.Body)
	}
	acc, err := h.store.GetAccountByEmail(t.Context(), "first@example.com")
	if err != nil {
		t.Fatalf("reading the account back: %v", err)
	}
	if acc.IsAdmin != 1 {
		t.Error("the account the bootstrap invite created is not an admin")
	}
}

// inviteBody is one invite as the JSON listing carries it. There is no field
// for a raw code, which is the point.
type inviteBody struct {
	ID       string `json:"id"`
	Note     string `json:"note"`
	IssuedBy string `json:"issued_by"`
	SpentBy  string `json:"spent_by"`
	State    string `json:"state"`
}

// findInvite picks one invite out of a listing body by id.
func findInvite(t *testing.T, body []byte, id string) inviteBody {
	t.Helper()
	var list struct {
		Invites []inviteBody `json:"invites"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("reading the invite list: %v", err)
	}
	for _, inv := range list.Invites {
		if inv.ID == id {
			return inv
		}
	}
	t.Fatalf("invite %s is not on the list: %s", id, body)
	return inviteBody{}
}

// containsCode reports whether a body carries the raw code out of a mint link.
func containsCode(body, link string) bool {
	_, code, ok := strings.Cut(link, "invite=")
	if !ok {
		return false
	}
	return strings.Contains(body, code)
}
