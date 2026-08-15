package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// The branches of the pre authentication guard that the happy path never walks,
// spec 0019. Each one is reachable from a browser and none of them is covered by
// pretoken_test.go, which tests the mechanism working rather than the ways a
// cookie arrives broken.

// TestAMalformedNonceCookieIsTreatedAsAbsent is AC-4, and the empty case is the
// one that matters most.
//
// preCSRFToken returns "" for an empty nonce, so a guard that accepted whatever
// the cookie held would compare "" against a posted field of "" and let the
// post through. Anyone can send an empty cookie, so that is a complete bypass of
// the mechanism rather than an edge case. The length check in preCSRFNonce is
// what stops it, and nothing else does.
//
// The other shapes are what a truncated, padded or hand edited cookie looks
// like. All four answer the same way: refused as missing, and a fresh cookie
// handed back so the retry works.
func TestAMalformedNonceCookieIsTreatedAsAbsent(t *testing.T) {
	valid := strings.Repeat("ab", preCSRFNonceBytes)

	for _, tc := range []struct {
		name  string
		value string
		// field is what the browser posts alongside. For the empty nonce it is
		// empty too, which is the pair a bypass would need.
		field string
	}{
		{"empty", "", ""},
		{"one hex short", valid[:len(valid)-1], ""},
		{"one hex long", valid + "a", ""},
		{"right length, not hex", strings.Repeat("z", len(valid)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.signIn(t, "someone@example.test")

			rec := h.postRaw(t, "/login", url.Values{
				"email": {"someone@example.test"}, "password": {testPassword},
				csrfField: {tc.field},
			}, nil, &http.Cookie{Name: preCSRFCookieSecure, Value: tc.value})

			if rec.Code != http.StatusForbidden {
				t.Fatalf("a post carrying a %s cookie: got %d, want 403", tc.name, rec.Code)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == auth.SessionCookie && c.Value != "" {
					t.Fatal("a malformed nonce cookie signed the person in, which is a bypass of the guard")
				}
			}
			row, ok := h.audit.last(auth.ActionPageCSRF)
			if !ok {
				t.Fatal("the refusal wrote no audit row")
			}
			if row.Reason != reasonPreTokenMissing {
				t.Errorf("audit reason = %q, want %q: an unusable cookie is absent, not a mismatch",
					row.Reason, reasonPreTokenMissing)
			}
			// And the person is not stuck: the refusal replaces the broken cookie,
			// so the next attempt off this page goes through.
			fresh, ok := preCSRFCookie(rec)
			if !ok {
				t.Fatal("the refusal set no fresh cookie, so every later attempt fails the same way")
			}
			if _, usable := h.srv.preCSRFNonce(withCookie(fresh)); !usable {
				t.Error("the cookie the refusal handed back is itself unusable")
			}
		})
	}
}

// withCookie wraps one cookie in the request preCSRFNonce reads it from.
func withCookie(c *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.AddCookie(c)
	return r
}

// TestTheResetLinksTokenSurvivesARefusal is AC-6 for the form it matters most
// on. A refused sign in costs the person a retype of their address; a refused
// reset would cost them the link itself, because the token rides in a hidden
// field rather than the address bar and a form re rendered without it has no way
// onward but the inbox.
func TestTheResetLinksTokenSurvivesARefusal(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t, "someone@example.test")

	// A real reset link, read out of the mailbox the way a person reads one.
	if rec := h.post(t, "/forgot", url.Values{"email": {"someone@example.test"}}, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("asking for a reset: got %d, want 200", rec.Code)
	}
	token := linkToken(t, h.mail.latest(t))

	rec := h.postRaw(t, "/reset", url.Values{
		"token": {token}, "password": {"another long enough password"},
	}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a reset post with no cookie: got %d, want 403", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `action="/reset"`) {
		t.Fatal("the refusal came back as something other than the reset form")
	}
	if !strings.Contains(body, `name="token" value="`+token+`"`) {
		t.Error("the re rendered reset form dropped the link's token, so the person has to start from the inbox again")
	}
	if strings.Contains(body, "another long enough password") {
		t.Error("the re rendered form carries the new password back, which is never safe to keep")
	}

	// And the retry off that refusal completes the reset, which is what AC-6 is
	// for: the refusal is a detour, not a dead end.
	nonce, fresh := h.preAuthToken(t, "/reset")
	again := h.postRaw(t, "/reset", url.Values{
		"token": {token}, "password": {"another long enough password"}, csrfField: {fresh},
	}, nil, nonce)
	if again.Code != http.StatusSeeOther && again.Code != http.StatusOK {
		t.Errorf("the retry after a refusal: got %d, want the reset to go through: %s", again.Code, again.Body)
	}
}

// TestARefusedPostDoesNotSpendARateLimitAttempt pins the ordering checkPreCSRF's
// doc comment claims: the guard runs before s.spend, so a person whose cookie
// went missing does not also burn through their allowance discovering it.
//
// Swapping the two reads as a tightening and is really a denial of service on
// the person: a stale tab posting a few times would lock the address out of the
// console for as long as the bucket takes to refill.
//
// The bucket holds ten and the harness clock does not advance, so nothing is
// refilled between these calls: more refusals than the bucket holds, then a real
// sign in that has to be answered.
func TestARefusedPostDoesNotSpendARateLimitAttempt(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t, "someone@example.test")

	for i := range 12 {
		rec := h.postRaw(t, "/login", url.Values{
			"email": {"someone@example.test"}, "password": {testPassword},
		}, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("refusal %d: got %d, want 403", i, rec.Code)
		}
	}

	nonce, token := h.preAuthToken(t, "/login")
	rec := h.postRaw(t, "/login", url.Values{
		"email": {"someone@example.test"}, "password": {testPassword}, csrfField: {token},
	}, nil, nonce)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("the refused posts spent the rate limit, so a missing cookie locks the person out of signing in")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("signing in after the refusals: got %d, want 303: %s", rec.Code, rec.Body)
	}
}

// TestTheOriginCheckAnswersACrossSitePostBeforeTheNonceCheck pins the other
// ordering in checkPreCSRF: the origin check runs first, and both guards stay.
//
// The two cases below are not the same test. A cross site post carrying a valid
// pair proves the pair does not excuse the origin, but it says nothing about the
// order, because a passing nonce check writes no row and leaves no trace either
// way. The pairless post is the one that can tell: with the origin check first
// it is answered `origin_cross_site` on the flat refusal page, and with the
// order swapped it is answered `csrf_pretoken_missing` and handed a fresh nonce
// cookie, which is a cookie minted for a request from somebody else's page.
func TestTheOriginCheckAnswersACrossSitePostBeforeTheNonceCheck(t *testing.T) {
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site"}

	t.Run("carrying a valid pair", func(t *testing.T) {
		h := newHarness(t, nil)
		h.signIn(t, "someone@example.test")

		nonce, token := h.preAuthToken(t, "/login")
		rec := h.postRaw(t, "/login", url.Values{
			"email": {"someone@example.test"}, "password": {testPassword}, csrfField: {token},
		}, crossSite, nonce)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("a cross site post carrying a valid pair: got %d, want 403", rec.Code)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == auth.SessionCookie && c.Value != "" {
				t.Fatal("a cross site post signed the person in, so the nonce pair excused the origin")
			}
		}
		if !h.audit.hasReason(auth.ActionPageCSRF, "origin_cross_site") {
			t.Error("the refusal did not name the origin check, so the log cannot say which guard fired")
		}
	})

	t.Run("carrying no pair", func(t *testing.T) {
		h := newHarness(t, nil)

		rec := h.postRaw(t, "/login", url.Values{
			"email": {"someone@example.test"}, "password": {testPassword},
		}, crossSite)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("a pairless cross site post: got %d, want 403", rec.Code)
		}
		if !h.audit.hasReason(auth.ActionPageCSRF, "origin_cross_site") {
			t.Error("the origin check did not answer first: the log names the nonce guard instead")
		}
		if h.audit.hasReason(auth.ActionPageCSRF, reasonPreTokenMissing) {
			t.Error("the nonce check ran on a cross site post, so the origin check is no longer first")
		}
		if _, ok := preCSRFCookie(rec); ok {
			t.Error("a cross site post was handed a fresh nonce cookie, which only a same origin render should get")
		}
	})
}
