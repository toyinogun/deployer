package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// The pre authentication CSRF guard, spec 0019. Every test here goes through the
// mux rather than calling a handler, because the cookie and the hidden field are
// only produced and only checked on the way through.

// preCSRFCookie picks the pre authentication cookie out of a response, or says
// there was none.
func preCSRFCookie(rec interface{ Result() *http.Response }) (*http.Cookie, bool) {
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, preCSRFCookiePlain) {
			return c, true
		}
	}
	return nil, false
}

// TestEveryPreAuthPageSetsTheNonceCookieAndRendersItsToken is AC-1 and AC-3: the
// five pages carrying a pre authentication form each set the cookie and render a
// hex token, and the token is the HMAC of the nonce rather than the nonce.
func TestEveryPreAuthPageSetsTheNonceCookieAndRendersItsToken(t *testing.T) {
	h := newHarness(t, nil)

	for _, page := range []string{"/login", "/register", "/forgot", "/reset", "/unverified"} {
		t.Run(page, func(t *testing.T) {
			rec := h.get(t, page, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: got %d, want 200", page, rec.Code)
			}
			c, ok := preCSRFCookie(rec)
			if !ok {
				t.Fatalf("GET %s set no pre authentication cookie", page)
			}
			match := csrfInPage.FindStringSubmatch(rec.Body.String())
			if match == nil {
				t.Fatalf("GET %s rendered no csrf field", page)
			}
			if want := h.srv.preCSRFToken(c.Value); match[1] != want {
				t.Errorf("the rendered token is not the HMAC of the cookie's nonce")
			}
		})
	}
}

// TestOnlyThePreAuthPagesSetTheNonceCookie is AC-1: no other page sets it. A
// page that set it needlessly would put a cookie on a browser that never posts
// a form, which is the sort of spread that makes a mechanism hard to reason
// about later.
func TestOnlyThePreAuthPagesSetTheNonceCookie(t *testing.T) {
	h := newHarness(t, nil)

	for _, page := range []string{"/", "/verify?token=nope", "/apps", "/nowhere"} {
		if _, ok := preCSRFCookie(h.get(t, page, nil)); ok {
			t.Errorf("GET %s set the pre authentication cookie, which only the five form pages do", page)
		}
	}
}

// TestTheNonceCookieCarriesTheHostPrefixAndItsFlags is AC-2. Every attribute is
// pinned one at a time, including the two that have to be absent: a Domain would
// hand the cookie to every app on the wildcard, and a Max-Age would outlive the
// browser session it belongs to.
func TestTheNonceCookieCarriesTheHostPrefixAndItsFlags(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get(t, "/login", nil)
	c, ok := preCSRFCookie(rec)
	if !ok {
		t.Fatal("GET /login set no pre authentication cookie")
	}
	if c.Name != preCSRFCookieSecure {
		t.Errorf("cookie name = %q, want %q: the prefix is what stops a sibling subdomain writing it",
			c.Name, preCSRFCookieSecure)
	}
	if !c.Secure {
		t.Error("cookie is not Secure, which the __Host- prefix requires")
	}
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly, so a script on the page could read the nonce")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("cookie path = %q, want /", c.Path)
	}
	if c.Domain != "" {
		t.Errorf("cookie carries Domain=%q, want none: the prefix forbids it", c.Domain)
	}
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Errorf("cookie carries MaxAge=%d Expires=%v, want neither so it lives as long as the browser session",
			c.MaxAge, c.Expires)
	}
	if raw := rec.Header().Get("Set-Cookie"); strings.Contains(raw, "Domain") {
		t.Errorf("Set-Cookie header carries a Domain attribute: %q", raw)
	}
}

// TestOverPlainHTTPTheCookieDropsThePrefixAndStillSignsIn is AC-2a. A browser
// refuses a Secure cookie over plain HTTP, so keeping the prefix would make
// signing in locally impossible; the name and the flag both come off s.secure.
//
// The harness is flipped rather than rebuilt because s.secure is the one value
// the acceptance criterion is about. The Origin header stays https, which this
// criterion does not speak to.
func TestOverPlainHTTPTheCookieDropsThePrefixAndStillSignsIn(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.secure = false

	rec := h.get(t, "/login", nil)
	c, ok := preCSRFCookie(rec)
	if !ok {
		t.Fatal("GET /login over plain HTTP set no cookie at all")
	}
	if c.Name != preCSRFCookiePlain {
		t.Errorf("cookie name = %q, want the unprefixed %q over plain HTTP", c.Name, preCSRFCookiePlain)
	}
	if c.Secure {
		t.Error("cookie is Secure over plain HTTP, which a browser refuses outright")
	}

	// And the whole round trip still completes, which is the thing the carve out
	// exists for.
	h.signIn(t, "local@example.test")
}

// TestASignedInResponseDropsTheNonceCookie is AC-7: exactly one CSRF mechanism
// is live at a time, so the sign in that sets the session cookie deletes this
// one in the same response.
func TestASignedInResponseDropsTheNonceCookie(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t, "someone@example.test")

	nonce, token := h.preAuthToken(t, "/login")
	rec := h.postRaw(t, "/login", url.Values{
		"email": {"someone@example.test"}, "password": {testPassword}, csrfField: {token},
	}, nil, nonce)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("signing in: got %d, want 303: %s", rec.Code, rec.Body)
	}

	c, ok := preCSRFCookie(rec)
	if !ok {
		t.Fatal("the sign in response did not touch the pre authentication cookie")
	}
	if c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("the pre authentication cookie survived the sign in: value=%q MaxAge=%d", c.Value, c.MaxAge)
	}
}

// TestAPostWithoutTheCookieIsRefusedAndComesBackAsTheForm is AC-4, AC-5 and
// AC-6. The refusal changes nothing, names its own reason in the audit log, and
// re renders the form with the address kept and the password not.
func TestAPostWithoutTheCookieIsRefusedAndComesBackAsTheForm(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t, "someone@example.test")

	_, token := h.preAuthToken(t, "/login")
	rec := h.postRaw(t, "/login", url.Values{
		"email": {"someone@example.test"}, "password": {testPassword}, csrfField: {token},
	}, nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a post with no cookie: got %d, want 403", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if auth.IsSessionCookie(c.Name) && c.Value != "" {
			t.Error("a refused post signed the person in anyway")
		}
	}
	row, ok := h.audit.last(auth.ActionPageCSRF)
	if !ok {
		t.Fatal("a refused post wrote no audit row")
	}
	if row.Reason != reasonPreTokenMissing {
		t.Errorf("audit reason = %q, want %q", row.Reason, reasonPreTokenMissing)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `action="/login"`) {
		t.Error("the refusal did not come back as the form, so the person has no way onward")
	}
	if !strings.Contains(body, preCSRFExpiredMessage) {
		t.Error("the re rendered form does not say what happened")
	}
	if !strings.Contains(body, `value="someone@example.test"`) {
		t.Error("the re rendered form dropped the address, which is safe to keep")
	}
	if strings.Contains(body, testPassword) {
		t.Error("the re rendered form carries the password back, which is never safe to keep")
	}
	if _, ok := preCSRFCookie(rec); !ok {
		t.Error("the refusal set no fresh cookie, so the retry would fail the same way")
	}
	// And the retry off that refusal works, which is the whole point of AC-6.
	if again := h.post(t, "/login", url.Values{
		"email": {"someone@example.test"}, "password": {testPassword},
	}, nil, nil); again.Code != http.StatusSeeOther {
		t.Errorf("the retry after a refusal: got %d, want 303", again.Code)
	}
}

// TestAPostWithAWrongFieldIsRefusedWithItsOwnReason is AC-5: the two ways this
// guard fails are distinct in the audit log, and both are distinct from the
// signed in path's csrf_invalid, so the log says which mechanism fired.
func TestAPostWithAWrongFieldIsRefusedWithItsOwnReason(t *testing.T) {
	h := newHarness(t, nil)

	nonce, _ := h.preAuthToken(t, "/forgot")
	rec := h.postRaw(t, "/forgot", url.Values{
		"email": {"someone@example.test"},
		// The right shape and the wrong value, which is what a guessed token
		// looks like. A malformed one would be refused by the length check
		// instead and prove less.
		csrfField: {strings.Repeat("a", 64)},
	}, nil, nonce)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a post with a wrong token: got %d, want 403", rec.Code)
	}
	row, ok := h.audit.last(auth.ActionPageCSRF)
	if !ok {
		t.Fatal("a refused post wrote no audit row")
	}
	if row.Reason != reasonPreTokenMismatch {
		t.Errorf("audit reason = %q, want %q", row.Reason, reasonPreTokenMismatch)
	}
	if row.Reason == "csrf_invalid" {
		t.Error("the pre authentication refusal is indistinguishable from the signed in one")
	}
}

// TestEveryGuardedPostRefusesWithoutItsPair is AC-4 across all five paths, not
// just the one the thin thread was built on. /resend is the one that differs:
// it has no GET route, so its cookie comes off /unverified.
func TestEveryGuardedPostRefusesWithoutItsPair(t *testing.T) {
	h := newHarness(t, nil)

	for path := range preAuthPageFor {
		t.Run(path, func(t *testing.T) {
			rec := h.postRaw(t, path, url.Values{"email": {"someone@example.test"}}, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s with no cookie and no token: got %d, want 403", path, rec.Code)
			}
		})
	}
}

// TestResendTakesItsCookieFromTheUnverifiedPage is AC-1: /resend has no GET
// route of its own, so the page hosting its form is the one that has to set the
// cookie. Without this the resend form would be permanently unusable.
func TestResendTakesItsCookieFromTheUnverifiedPage(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get(t, "/unverified?email=someone@example.test", nil)
	nonce, ok := preCSRFCookie(rec)
	if !ok {
		t.Fatal("GET /unverified set no cookie, so its resend form can never be submitted")
	}
	match := csrfInPage.FindStringSubmatch(rec.Body.String())
	if match == nil {
		t.Fatal("the unverified page rendered no csrf field")
	}

	if got := h.postRaw(t, "/resend", url.Values{
		"email": {"someone@example.test"}, csrfField: {match[1]},
	}, nil, nonce); got.Code != http.StatusOK {
		t.Errorf("resending with the pair off /unverified: got %d, want 200: %s", got.Code, got.Body)
	}
}

// TestTwoTabsOnTheSameFormBothSubmit is AC-10. The nonce is reused rather than
// rotated per render, so the tab that rendered first is still good when the
// second one has been through. Rotating is the obvious implementation and it
// breaks this quietly.
func TestTwoTabsOnTheSameFormBothSubmit(t *testing.T) {
	h := newHarness(t, nil)

	first := h.get(t, "/forgot", nil)
	nonce, ok := preCSRFCookie(first)
	if !ok {
		t.Fatal("the first tab got no cookie")
	}
	firstToken := csrfInPage.FindStringSubmatch(first.Body.String())[1]

	// The second tab arrives carrying the cookie the first one was given, the
	// way a browser sends it.
	second := h.get(t, "/forgot", nonce)
	secondToken := csrfInPage.FindStringSubmatch(second.Body.String())[1]
	if secondToken != firstToken {
		t.Error("the second render rotated the nonce, which invalidates the first tab")
	}

	for i, token := range []string{secondToken, firstToken} {
		got := h.postRaw(t, "/forgot", url.Values{
			"email": {"someone@example.test"}, csrfField: {token},
		}, nil, nonce)
		if got.Code != http.StatusOK {
			t.Errorf("tab %d: got %d, want 200: %s", i, got.Code, got.Body)
		}
	}
}

// TestTheNonceNeverReachesAPageOrAnAuditRow is AC-9. The nonce is the secret and
// the HMAC is the proof, so a page that rendered the nonce would hand an
// attacker on another site everything needed to forge the pair.
func TestTheNonceNeverReachesAPageOrAnAuditRow(t *testing.T) {
	h := newHarness(t, nil)

	rec := h.get(t, "/login", nil)
	nonce, ok := preCSRFCookie(rec)
	if !ok {
		t.Fatal("GET /login set no cookie")
	}
	if strings.Contains(rec.Body.String(), nonce.Value) {
		t.Error("the nonce is rendered into the page, so the cookie is no longer a second factor")
	}

	// And a refusal, which is the path that mints a fresh one while rendering.
	refused := h.postRaw(t, "/login", url.Values{"email": {"someone@example.test"}}, nil)
	fresh, ok := preCSRFCookie(refused)
	if !ok {
		t.Fatal("the refusal set no cookie")
	}
	if strings.Contains(refused.Body.String(), fresh.Value) {
		t.Error("a refused post renders its own nonce into the form it comes back as")
	}
	for _, row := range h.audit.all() {
		if row.Reason == nonce.Value || row.TargetID == nonce.Value || row.Reason == fresh.Value {
			t.Error("an audit row carries the nonce")
		}
	}
}
