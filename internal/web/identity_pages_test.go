package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// register runs one registration through the page and returns the link token it
// mailed.
func register(t *testing.T, h *harness, email string) string {
	t.Helper()
	rec := h.post(t, "/register", url.Values{
		"email": {email}, "password": {testPassword}, "display_name": {"Someone"},
	}, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("registering %s: got %d, want 200", email, rec.Code)
	}
	return linkToken(t, h.mail.latest(t))
}

// TestWrongCredentialsReRenderTheFormWithOneGenericMessage is AC-5. The message
// is generic because telling a wrong password from an unknown address turns the
// sign in form into a way to enumerate accounts. covers: AC-5
func TestWrongCredentialsReRenderTheFormWithOneGenericMessage(t *testing.T) {
	h := newHarness(t, nil)
	register(t, h, "wrong@example.test")

	unknown := h.post(t, "/login", url.Values{
		"email": {"nobody@example.test"}, "password": {testPassword},
	}, nil, nil)
	if unknown.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown address: got %d, want 401", unknown.Code)
	}

	// A registered but unverified account has its own page, so compare against a
	// verified one with the wrong password instead.
	h.get(t, "/verify?token="+register(t, h, "known@example.test"), nil)
	badPassword := h.post(t, "/login", url.Values{
		"email": {"known@example.test"}, "password": {"not the right password"},
	}, nil, nil)
	if badPassword.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password: got %d, want 401", badPassword.Code)
	}

	// The typed address survives the refusal, so a person is not made to type it
	// again, but the password never does.
	if !strings.Contains(badPassword.Body.String(), "known@example.test") {
		t.Error("a refused sign in emptied the address field")
	}
	if strings.Contains(badPassword.Body.String(), "not the right password") {
		t.Error("a refused sign in echoed the password back into the page")
	}
}

// TestRegisteringAnAddressThatExistsLooksIdentical is AC-6: the page must not
// be the surface that tells an attacker which addresses have accounts.
// covers: AC-6
func TestRegisteringAnAddressThatExistsLooksIdentical(t *testing.T) {
	h := newHarness(t, nil)

	first := h.post(t, "/register", url.Values{
		"email": {"dup@example.test"}, "password": {testPassword},
	}, nil, nil)
	second := h.post(t, "/register", url.Values{
		"email": {"dup@example.test"}, "password": {testPassword},
	}, nil, nil)

	if first.Code != second.Code {
		t.Errorf("a new address gets %d and a duplicate gets %d, want the same", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Error("a duplicate registration renders a different page from a new one")
	}
}

// TestEveryBadLinkRendersTheSamePage is AC-7. Telling the four apart tells the
// holder of a stolen link which kind they are holding. covers: AC-7
func TestEveryBadLinkRendersTheSamePage(t *testing.T) {
	h := newHarness(t, nil)

	consumed := register(t, h, "consumed@example.test")
	if rec := h.get(t, "/verify?token="+consumed, nil); rec.Code != http.StatusOK {
		t.Fatalf("the first use of a link: got %d, want 200", rec.Code)
	}

	// A reset link is a real, live token issued for the other purpose.
	h.get(t, "/verify?token="+register(t, h, "purpose@example.test"), nil)
	if rec := h.post(t, "/forgot", url.Values{"email": {"purpose@example.test"}},
		nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("asking for a reset link: got %d, want 200", rec.Code)
	}
	wrongPurpose := linkToken(t, h.mail.latest(t))

	// An expired one: issued, then time moved past the link's day.
	expired := register(t, h, "expired@example.test")
	h.clock.T = h.clock.T.Add(25 * time.Hour)

	responses := map[string]string{}
	for _, tc := range []struct{ name, token string }{
		{"consumed", consumed},
		{"unknown", "a token nobody ever issued"},
		{"the other purpose", wrongPurpose},
		{"expired", expired},
	} {
		rec := h.get(t, "/verify?token="+url.QueryEscape(tc.token), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("a %s link: got %d, want 400", tc.name, rec.Code)
		}
		responses[tc.name] = rec.Body.String()
	}

	first := responses["consumed"]
	for name, body := range responses {
		if body != first {
			t.Errorf("a %s link renders different words from a consumed one", name)
		}
	}
	if !strings.Contains(first, "/unverified") {
		t.Error("the shared link invalid page carries no resend action")
	}
}

// TestAnUnverifiedSignInGetsItsOwnPage is AC-8: the address so the person knows
// which inbox to check, and the resend limit written out so hitting it is not a
// mystery. covers: AC-8
func TestAnUnverifiedSignInGetsItsOwnPage(t *testing.T) {
	h := newHarness(t, nil)
	register(t, h, "unverified@example.test") // registered, link never clicked

	rec := h.post(t, "/login", url.Values{
		"email": {"unverified@example.test"}, "password": {testPassword},
	}, nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an unverified sign in: got %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "unverified@example.test") {
		t.Error("the unverified page does not show the address")
	}
	if !strings.Contains(body, "three") {
		t.Error("the unverified page does not write out the resend limit")
	}
}

// TestResendConfirmsTheSameWayForAnyAddress is AC-8's action, and keeps it from
// becoming an enumeration surface of its own. covers: AC-8, AC-9
func TestResendConfirmsTheSameWayForAnyAddress(t *testing.T) {
	h := newHarness(t, nil)
	register(t, h, "resend@example.test")

	known := h.post(t, "/resend", url.Values{"email": {"resend@example.test"}}, nil, nil)
	unknown := h.post(t, "/resend", url.Values{"email": {"nobody@example.test"}}, nil, nil)

	if known.Code != http.StatusOK || unknown.Code != http.StatusOK {
		t.Fatalf("known %d, unknown %d, want 200 and 200", known.Code, unknown.Code)
	}
	if !strings.Contains(unknown.Body.String(), "Check your email") {
		t.Error("resending to an unknown address does not confirm")
	}
}

// TestForgotConfirmsTheSameWayWhetherOrNotTheAddressExists is AC-9.
// covers: AC-9
func TestForgotConfirmsTheSameWayWhetherOrNotTheAddressExists(t *testing.T) {
	h := newHarness(t, nil)
	h.get(t, "/verify?token="+register(t, h, "forgot@example.test"), nil)

	known := h.post(t, "/forgot", url.Values{"email": {"forgot@example.test"}}, nil, nil)
	unknown := h.post(t, "/forgot", url.Values{"email": {"nobody@example.test"}}, nil, nil)

	if known.Code != unknown.Code {
		t.Errorf("a known address gets %d and an unknown one gets %d, want the same",
			known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Error("the forgot confirmation differs by whether the address exists")
	}
}

// TestResetSetsThePasswordAndSignsEverythingOut is AC-9's other end. It does not
// sign the person straight back in: the service revokes every session the
// account holds, and signing back in would hide that from whoever's account it
// actually was. covers: AC-9
func TestResetSetsThePasswordAndSignsEverythingOut(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "reset@example.test")

	if rec := h.post(t, "/forgot", url.Values{"email": {"reset@example.test"}},
		nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("asking for a reset link: got %d, want 200", rec.Code)
	}
	token := linkToken(t, h.mail.latest(t))

	const newPassword = "an entirely different password"
	rec := h.post(t, "/reset", url.Values{"token": {token}, "password": {newPassword}}, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resetting: got %d, want 200: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), newPassword) {
		t.Error("the reset confirmation echoed the new password back")
	}
	// The session that existed before is dead, and the page does not hand out a
	// new one.
	if after := h.get(t, "/apps", cookie); after.Code != http.StatusSeeOther {
		t.Errorf("the old session survived a password reset: /apps got %d", after.Code)
	}
	if got := h.post(t, "/login", url.Values{
		"email": {"reset@example.test"}, "password": {testPassword},
	}, nil, nil); got.Code != http.StatusUnauthorized {
		t.Errorf("the old password still signs in: got %d, want 401", got.Code)
	}

	// And the reset link is single use, like every other link.
	if again := h.post(t, "/reset", url.Values{
		"token": {token}, "password": {newPassword},
	}, nil, nil); again.Code != http.StatusBadRequest {
		t.Errorf("a reset link worked twice: got %d, want 400", again.Code)
	}
}

// TestMailedLinksPointAtThePages is AC-10: the links a person clicks are page
// URLs, not the JSON endpoints, which stay drivable with curl regardless.
// covers: AC-10
func TestMailedLinksPointAtThePages(t *testing.T) {
	h := newHarness(t, nil)
	register(t, h, "links@example.test")
	verification := h.mail.latest(t)

	if !strings.Contains(verification, testPublicURL+"/verify?token=") {
		t.Errorf("the verification mail does not link at the page: %q", verification)
	}
	if strings.Contains(verification, "/v1/auth/") {
		t.Errorf("the verification mail still links at the JSON surface: %q", verification)
	}

	h.get(t, "/verify?token="+linkToken(t, verification), nil)
	if rec := h.post(t, "/forgot", url.Values{"email": {"links@example.test"}},
		nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("asking for a reset link: got %d, want 200", rec.Code)
	}
	reset := h.mail.latest(t)
	if !strings.Contains(reset, testPublicURL+"/reset?token=") {
		t.Errorf("the reset mail does not link at the page: %q", reset)
	}
}
