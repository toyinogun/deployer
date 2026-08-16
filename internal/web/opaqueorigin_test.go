package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The register page answers Referrer-Policy: no-referrer, because the invite
// code rides in its query string (spec 0015, AC-14). A browser honours that on
// the form post as well as on the links: Chrome serialises the post's Origin as
// the literal `null` rather than naming the page it came from. Every test in
// this package and every curl in a verify run sends a well formed Origin, so the
// whole suite passed against a form no browser could submit. Driven in Chromium
// against the cluster on 2026-08-16, the post carried `origin: null` and the
// console answered 403.
const opaqueOrigin = "null"

// TestARegistrationFromTheNoReferrerPageIsAccepted is the bug. An opaque origin
// carries no information, exactly like an absent Origin header, and the origin
// check already treats an absent one as unjudgeable and falls through. What
// guards this post is the pre authentication pair, which holds against an opaque
// origin on its own: the nonce cookie is SameSite=Lax, so a genuinely cross site
// post carries no cookie at all and is refused for the missing half.
func TestARegistrationFromTheNoReferrerPageIsAccepted(t *testing.T) {
	// covers: spec 0015 AC-1, spec 0025 AC-1
	t.Parallel()
	h := newHarness(t, nil)
	cookie, token := h.preAuthToken(t, "/register")

	form := h.registration(t, "invited@example.test")
	form.Set(csrfField, token)
	rec := h.postRaw(t, "/register", form,
		map[string]string{"Origin": opaqueOrigin, "Sec-Fetch-Site": "same-origin"}, cookie)

	if rec.Code != http.StatusOK {
		t.Fatalf("registering from the no-referrer page: got %d, want 200. Body: %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Check your email") {
		t.Errorf("the post was accepted but did not land on the check your email page: %s", rec.Body)
	}
}

// TestTheRegisterPageStillAnswersNoReferrer pins the header the fix must not
// drop. Sending the invite code out in a referrer header is what spec 0015 AC-14
// forbids, so making the post work by removing the policy would trade one bug
// for a leak.
func TestTheRegisterPageStillAnswersNoReferrer(t *testing.T) {
	// covers: spec 0015 AC-14
	t.Parallel()
	h := newHarness(t, nil)
	rec := h.get(t, "/register?invite=whatever", nil)
	if got := rec.Result().Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("the register page answers Referrer-Policy %q, want no-referrer", got)
	}
}

// TestAnOpaqueOriginWithoutTheCookieIsStillRefused is why accepting the opaque
// origin above is safe. A cross site post can produce `null` deliberately, from
// a sandboxed iframe, but it cannot produce the nonce cookie: the cookie is
// SameSite=Lax and carries the `__Host-` prefix, so a post from somebody else's
// page arrives without it and is refused for the half it is missing.
func TestAnOpaqueOriginWithoutTheCookieIsStillRefused(t *testing.T) {
	// covers: spec 0021 AC-7
	t.Parallel()
	h := newHarness(t, nil)
	_, token := h.preAuthToken(t, "/register")

	form := h.registration(t, "sandboxed@example.test")
	form.Set(csrfField, token)
	rec := h.postRaw(t, "/register", form,
		map[string]string{"Origin": opaqueOrigin, "Sec-Fetch-Site": "cross-site"}, nil)

	if rec.Code == http.StatusOK {
		t.Fatalf("a cross site post with an opaque origin and no cookie was accepted: %s", rec.Body)
	}
}

// TestASignedInPostStillRefusesAnOpaqueOrigin keeps the fix to the surface that
// needs it. No signed in page answers no-referrer, so an opaque origin there is
// not something a browser produces, and the session path keeps refusing it.
func TestASignedInPostStillRefusesAnOpaqueOrigin(t *testing.T) {
	// covers: spec 0021 AC-21
	t.Parallel()
	h := newHarness(t, nil)
	cookie := h.signIn(t, "person@example.test")

	rec := h.postRaw(t, "/logout", url.Values{csrfField: {h.csrfFor(t, cookie)}},
		map[string]string{"Origin": opaqueOrigin}, cookie)

	if rec.Code != http.StatusForbidden {
		t.Errorf("a signed in post with an opaque origin: got %d, want 403", rec.Code)
	}
}
