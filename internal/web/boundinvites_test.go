package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// mintBound mints one invite from the admin page, optionally bound to an
// address, and returns the whole rendered response. Everything below drives the
// real form rather than calling the service, because the address is a form field
// and a test that skips the form would never cross it.
func mintBound(t *testing.T, h *harness, admin *http.Cookie, note, email string) *http.Response {
	t.Helper()
	rec := h.post(t, "/admin/invites", url.Values{
		"note": {note}, "email": {email}, "csrf": {h.csrfFor(t, admin)},
	}, admin, nil)
	return rec.Result()
}

// mintedLink pulls the one time link out of a mint response.
func mintedLink(t *testing.T, body string) string {
	t.Helper()
	const open = `id="minted-invite">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("the mint response carried no link:\n%s", body)
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "<")
	if j < 0 {
		t.Fatal("the link was never closed")
	}
	return strings.TrimSpace(rest[:j])
}

// codeFromLink pulls the raw invite code back out of a register link, the way an
// invited person's browser does.
func codeFromLink(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parsing the invite link: %v", err)
	}
	code := u.Query().Get("invite")
	if code == "" {
		t.Fatalf("the invite link carried no code: %s", link)
	}
	return code
}

// readBody reads a response body once and closes it.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the response body: %v", err)
		}
	}()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	return buf.String()
}

// TestABoundInviteTravelsFromMintToAccount is the thin thread: an admin types an
// address, the platform mails the link to exactly that address, and registering
// from it as that person creates the account and spends the invite.
// covers: AC-1, AC-4, AC-5, AC-9
func TestABoundInviteTravelsFromMintToAccount(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")
	before := h.mail.count()

	// A mixed case address, so the same test proves the normalization: what is
	// stored, what is mailed and what registers are one value (AC-9).
	resp := mintBound(t, h, admin, "for Sam", "Sam@Example.Test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("minting a bound invite: got %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)

	if got := h.mail.count(); got != before+1 {
		t.Fatalf("a bound mint sent %d messages, want exactly one", got-before)
	}
	if to := h.mail.latestTo(t); to != "sam@example.test" {
		t.Errorf("the message went to %q, want the normalized address", to)
	}
	link := mintedLink(t, body)
	if !strings.Contains(h.mail.latest(t), link) {
		t.Error("the mailed message does not carry the link the page shows")
	}
	// The page names the address it went to, and the message names the inviter
	// and the seven day expiry (AC-4, AC-5).
	if !strings.Contains(body, "sam@example.test") {
		t.Error("the mint page never says which address it was sent to")
	}
	for _, want := range []string{"Someone", "7 days"} {
		if !strings.Contains(h.mail.latest(t), want) {
			t.Errorf("the invite message is missing %q:\n%s", want, h.mail.latest(t))
		}
	}

	// The invited person registers from that link, as that address.
	if rec := h.post(t, "/register", url.Values{
		"invite": {codeFromLink(t, link)}, "email": {"sam@example.test"},
		"password": {testPassword}, "display_name": {"Sam"},
	}, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("registering from a bound invite: got %d, want 200", rec.Code)
	}
	if _, err := h.store.GetAccountByEmail(t.Context(), "sam@example.test"); err != nil {
		t.Fatalf("the bound registration created no account: %v", err)
	}
}

// TestABoundInviteRefusesEveryOtherAddress is the binding itself. A live bound
// invite presented by anybody else is refused in exactly the words and the status
// an unknown code gets, creates nothing, and is still usable afterwards by the
// person it was meant for.
// covers: AC-8
func TestABoundInviteRefusesEveryOtherAddress(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	link := mintedLink(t, readBody(t, mintBound(t, h, admin, "", "sam@example.test")))
	code := codeFromLink(t, link)

	wrong := h.post(t, "/register", url.Values{
		"invite": {code}, "email": {"mallory@example.test"},
		"password": {testPassword}, "display_name": {"Mallory"},
	}, nil, nil)

	// The control: a code that never existed at all. If the two responses differ
	// by so much as a word, holding a live invite is observable.
	unknown := h.post(t, "/register", url.Values{
		"invite": {"not-a-real-code"}, "email": {"mallory@example.test"},
		"password": {testPassword}, "display_name": {"Mallory"},
	}, nil, nil)

	if wrong.Code != unknown.Code {
		t.Errorf("a wrong address answered %d and an unknown code answered %d", wrong.Code, unknown.Code)
	}
	// Each page echoes the code it was handed back into the hidden field, so
	// comparing the two means blanking that one value out first.
	blank := func(body, code string) string {
		return withoutCSRF(strings.ReplaceAll(body, code, "CODE"))
	}
	if blank(wrong.Body.String(), code) != blank(unknown.Body.String(), "not-a-real-code") {
		t.Error("a bound invite with the wrong address renders differently from an unknown code")
	}
	if _, err := h.store.GetAccountByEmail(t.Context(), "mallory@example.test"); err == nil {
		t.Fatal("the refused registration created an account")
	}

	// The invite survived, so the person it was for can still use it.
	if rec := h.post(t, "/register", url.Values{
		"invite": {code}, "email": {"sam@example.test"},
		"password": {testPassword}, "display_name": {"Sam"},
	}, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("the invite did not survive a refused address: got %d", rec.Code)
	}
}

// TestAnUnboundInviteStillTakesAnyAddress is the compatibility case, and it is
// the one line in this build worth reading twice: the lookup's predicate has to
// keep a null address matching every candidate, or every invite minted before
// this feature, the platform's own bootstrap one included, stops working.
// covers: AC-1, AC-16
func TestAnUnboundInviteStillTakesAnyAddress(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	// Straight through the store with no address at all, which is the shape the
	// boot time bootstrap invite carries permanently.
	bootstrap := h.invite(t)
	if rec := h.post(t, "/register", url.Values{
		"invite": {bootstrap}, "email": {"anyone@example.test"},
		"password": {testPassword}, "display_name": {"Anyone"},
	}, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("an unbound invite refused an address: got %d", rec.Code)
	}

	// And an unbound mint from the page is unchanged: a link, and no message.
	before := h.mail.count()
	body := readBody(t, mintBound(t, h, admin, "for whoever", ""))
	if mintedLink(t, body) == "" {
		t.Error("an unbound mint showed no link")
	}
	if strings.Contains(body, "Sent to <strong>") {
		t.Error("an unbound mint claimed to have sent something")
	}
	if got := h.mail.count(); got != before {
		t.Errorf("an unbound mint sent %d messages, want none", got-before)
	}
}

// TestASendFailureKeepsTheInvite is AC-6: the invite is committed before the
// send is attempted, so a provider being down costs an invite nothing. The page
// still carries the link, says the send failed, and the log line names neither
// the address nor the code.
// covers: AC-6, AC-12
func TestASendFailureKeepsTheInvite(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	h.mail.refuse = true
	resp := mintBound(t, h, admin, "for Sam", "sam@example.test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a failed send refused the mint: got %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "failed") {
		t.Error("the page does not say the send failed")
	}
	link := mintedLink(t, body)

	if line := logged.String(); strings.Contains(line, "sam@example.test") ||
		strings.Contains(line, codeFromLink(t, link)) {
		t.Errorf("the failure log carries the address or the code: %s", line)
	}

	// The invite is live and bound, so handing the link over by hand works.
	h.mail.refuse = false
	if rec := h.post(t, "/register", url.Values{
		"invite": {codeFromLink(t, link)}, "email": {"sam@example.test"},
		"password": {testPassword}, "display_name": {"Sam"},
	}, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("the invite did not survive a failed send: got %d", rec.Code)
	}
}

// TestARefusedMintWritesNothing is AC-3 and AC-2: an address that already has an
// account has nobody left to invite, and a malformed one is refused before any
// of that. Neither mints a row and neither sends a message.
// covers: AC-2, AC-3
func TestARefusedMintWritesNothing(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	before, err := h.store.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing invites: %v", err)
	}
	sends := h.mail.count()

	if got := mintBound(t, h, admin, "", "admin@example.test").StatusCode; got != http.StatusConflict {
		t.Errorf("minting to a registered address: got %d, want 409", got)
	}
	if got := mintBound(t, h, admin, "", "not an address").StatusCode; got != http.StatusUnprocessableEntity {
		t.Errorf("minting to a malformed address: got %d, want 422", got)
	}

	after, err := h.store.ListInvites(t.Context())
	if err != nil {
		t.Fatalf("listing invites: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused mint wrote %d rows", len(after)-len(before))
	}
	if h.mail.count() != sends {
		t.Error("a refused mint sent a message")
	}
}

// TestTheInviteListShowsTheBoundAddress is AC-14: the address has its own
// column, and an unbound invite leaves that cell empty rather than borrowing the
// note.
// covers: AC-14
func TestTheInviteListShowsTheBoundAddress(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")

	mintBound(t, h, admin, "bound one", "sam@example.test")
	mintBound(t, h, admin, "unbound one", "")

	body := h.get(t, "/admin/invites", admin).Body.String()
	if !strings.Contains(body, "Sent to") {
		t.Error("the list has no address column")
	}
	if !strings.Contains(body, "sam@example.test") {
		t.Error("the list does not show the bound address")
	}
	if !strings.Contains(body, "not sent") {
		t.Error("an unbound invite does not render an empty address cell")
	}
}

// TestTheInviteGateAnswersBeforeThePassword is AC-10 and AC-11: the address is
// part of the invite lookup, which is still the first statement in Register, so
// a mismatched caller is refused ahead of CheckPassword and costs the platform
// no key derivation. A password that would fail on its own never gets to say so.
// covers: AC-10, AC-11
func TestTheInviteGateAnswersBeforeThePassword(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.test")
	link := mintedLink(t, readBody(t, mintBound(t, h, admin, "", "sam@example.test")))

	rec := h.post(t, "/register", url.Values{
		"invite": {codeFromLink(t, link)}, "email": {"mallory@example.test"},
		"password": {"short"}, "display_name": {"Mallory"},
	}, nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a mismatched address with a bad password: got %d, want the 403 the invite gate answers", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "by invitation") {
		t.Errorf("the refusal spoke past the invite gate rather than answering it:\n%s", rec.Body)
	}
}
