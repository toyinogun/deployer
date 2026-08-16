package web

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
)

// TestTheSessionCookieIsHostScopedWhenSecure is AC-19. The `__Host-` prefix is
// what stops an app on a sibling subdomain shadowing the console's session, which
// would be session fixation with a deploy as the delivery mechanism.
func TestTheSessionCookieIsHostScopedWhenSecure(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	h := newHarness(t, nil) // the harness runs on https, so the prefix is live
	cookie := h.signIn(t, "person@example.test")

	if cookie.Name != auth.SessionCookieSecure {
		t.Fatalf("the session cookie is named %q, want %q", cookie.Name, auth.SessionCookieSecure)
	}
	rec := h.postRaw(t, "/logout", url.Values{csrfField: {h.csrfFor(t, cookie)}}, nil, cookie)
	for _, c := range rec.Result().Cookies() {
		if !auth.IsSessionCookie(c.Name) {
			continue
		}
		if !c.Secure {
			t.Error("the session cookie is not Secure, which a __Host- prefixed cookie must be")
		}
		if c.Domain != "" {
			t.Errorf("the session cookie carries Domain=%q, which the __Host- prefix forbids and which "+
				"would hand it to every app on the wildcard", c.Domain)
		}
		if c.Path != "/" {
			t.Errorf("the session cookie carries Path=%q, want /", c.Path)
		}
	}
}

// TestTheSessionCookieDropsThePrefixOverPlainHTTP is AC-19. A browser refuses a
// Secure cookie outright over plain HTTP, so keeping the prefix would make signing
// in locally impossible. That deployment loses the sibling subdomain guarantee,
// which is stated rather than hidden.
func TestTheSessionCookieDropsThePrefixOverPlainHTTP(t *testing.T) {
	// covers: AC-19
	t.Parallel()
	if got := auth.SessionCookieName(false); got != auth.SessionCookiePlain {
		t.Errorf("over plain HTTP the cookie is named %q, want %q", got, auth.SessionCookiePlain)
	}
	if got := auth.SessionCookieName(true); got != auth.SessionCookieSecure {
		t.Errorf("over HTTPS the cookie is named %q, want %q", got, auth.SessionCookieSecure)
	}
	// Each scheme resolves its own name and not the other one. This used to
	// assert that both names resolved either way, which is what let a pre rename
	// cookie keep authenticating and let a sibling app plant a parent scoped one.
	// The cases live in internal/auth/session_test.go, beside the read itself.
	for _, secure := range []bool{true, false} {
		req, err := http.NewRequest(http.MethodGet, "http://example.test/", nil)
		if err != nil {
			t.Fatalf("building a request: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName(secure), Value: "the-session"})
		if got := auth.SessionID(req, secure); got != "the-session" {
			t.Errorf("with secure=%t the cookie resolved to %q, want the session id", secure, got)
		}
	}
}

// TestBothOriginsAreAcceptedAndAThirdIsNot is AC-21. The platform answers on two
// names now, so the accepted set is two rather than one, and it is still a set
// rather than anything goes.
func TestBothOriginsAreAcceptedAndAThirdIsNot(t *testing.T) {
	// covers: AC-21
	t.Parallel()
	h := newHarness(t, nil)

	for i, tc := range []struct {
		origin string
		want   int
	}{
		{testConsoleURL, http.StatusSeeOther},
		{"https://" + testConsoleHost, http.StatusSeeOther},
		{"https://evil.example.test", http.StatusForbidden},
		{"https://" + testConsoleHost + ".evil.test", http.StatusForbidden},
		{"http://" + testConsoleHost, http.StatusForbidden},
	} {
		// One account per case, because an accepted logout ends the session and
		// a shared one would make the order of the cases matter.
		signedIn := h.signIn(t, fmt.Sprintf("person%d@example.test", i))
		rec := h.postRaw(t, "/logout", url.Values{csrfField: {h.csrfFor(t, signedIn)}},
			map[string]string{"Origin": tc.origin}, signedIn)
		if rec.Code != tc.want {
			t.Errorf("a post from %s: got %d, want %d", tc.origin, rec.Code, tc.want)
		}
	}
}

// TestThePreTokenStaysHostScoped is AC-21a. Widening the accepted origin set does
// not let a nonce minted on one host satisfy a post to the other: the pre
// authentication cookie carries the `__Host-` prefix, so each hostname mints its
// own and a post still has to carry the cookie its own host set.
func TestThePreTokenStaysHostScoped(t *testing.T) {
	// covers: AC-21a
	t.Parallel()
	h := newHarness(t, nil)

	// A nonce and its token, minted on one host.
	nonce, token := h.preAuthToken(t, "/login")

	// The same token posted back without that nonce is refused, which is the
	// property host scoping gives: the other host's browser never sends this
	// cookie, so it can never present this pair.
	rec := h.postRaw(t, "/login", url.Values{
		"email": {"person@example.test"}, "password": {testPassword}, csrfField: {token},
	}, nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a post carrying the token but not its nonce: got %d, want 403", rec.Code)
	}

	// The pair together still works, so the refusal above is about the missing
	// cookie rather than about the token being wrong.
	rec = h.postRaw(t, "/login", url.Values{
		"email": {"nobody@example.test"}, "password": {testPassword}, csrfField: {token},
	}, nil, nonce)
	if rec.Code == http.StatusForbidden {
		t.Error("the matching pair was refused, so this test is not measuring host scoping")
	}
	if h.srv.preCSRFCookieName() != preCSRFCookieSecure {
		t.Errorf("the pre token cookie is named %q, want the __Host- prefixed one",
			h.srv.preCSRFCookieName())
	}
}
