package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// TestSafeNextFollowsOnlyALocalPath is the whole of the open redirect guard
// AC-2 promises. Two slashes is the case worth naming: a browser resolves
// //evil.test to another host entirely, so it is not the local path it looks
// like. covers: AC-2
func TestSafeNextFollowsOnlyALocalPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		next string
		want string
	}{
		{"a local path is followed", "/tokens", "/tokens"},
		{"a nested local path is followed", "/apps/demo/logs", "/apps/demo/logs"},
		{"empty falls back to the app list", "", "/apps"},
		{"protocol relative is refused", "//evil.test", "/apps"},
		{"protocol relative with a path is refused", "//evil.test/apps", "/apps"},
		{"an absolute URL is refused", "https://evil.test", "/apps"},
		{"a scheme relative backslash is refused", "\\\\evil.test", "/apps"},
		{"a bare word is refused", "apps", "/apps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := safeNext(tc.next); got != tc.want {
				t.Errorf("safeNext(%q) = %q, want %q", tc.next, got, tc.want)
			}
		})
	}
}

// TestRootRedirectsBySession is AC-2's first sentence: the root is never a page,
// it is whichever page is useful to the visitor. covers: AC-2
func TestRootRedirectsBySession(t *testing.T) {
	h := newHarness(t, nil)

	signedOut := h.get(t, "/", nil)
	if signedOut.Code != http.StatusSeeOther || signedOut.Header().Get("Location") != "/login" {
		t.Errorf("signed out root: got %d to %q, want 303 to /login",
			signedOut.Code, signedOut.Header().Get("Location"))
	}

	cookie := h.signIn(t, "root@example.test")
	signedIn := h.get(t, "/", cookie)
	if signedIn.Code != http.StatusSeeOther || signedIn.Header().Get("Location") != "/apps" {
		t.Errorf("signed in root: got %d to %q, want 303 to /apps",
			signedIn.Code, signedIn.Header().Get("Location"))
	}
}

// TestEveryGatedPageRedirectsCarryingItsPath is AC-2: a signed out visitor is
// not refused, they are sent to sign in and their navigation is finished for
// them afterwards. The query string is deliberately dropped. covers: AC-2
func TestEveryGatedPageRedirectsCarryingItsPath(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{
		"/apps", "/apps/demo", "/apps/demo/releases", "/apps/demo/logs", "/apps/demo/config",
		"/tokens", "/admin/accounts",
	} {
		rec := h.get(t, path+"?cursor=secret-cursor", nil)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("GET %s signed out: got %d, want 303", path, rec.Code)
			continue
		}
		want := "/login?next=" + url.QueryEscape(path)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("GET %s signed out redirects to %q, want %q", path, got, want)
		}
	}
}

// TestSignInFollowsNextOnlyWhenItIsLocal is the acted out half of AC-2: the
// guard has to hold on the real POST, not only in the helper.
// covers: AC-2
func TestSignInFollowsNextOnlyWhenItIsLocal(t *testing.T) {
	for _, tc := range []struct {
		name string
		next string
		want string
	}{
		{"a local path lands where the visitor was going", "/tokens", "/tokens"},
		{"a protocol relative address lands on the app list", "//evil.test", "/apps"},
		{"an absolute address lands on the app list", "https://evil.test", "/apps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			email := "next@example.test"
			if rec := h.post(t, "/register", url.Values{
				"email": {email}, "password": {testPassword},
			}, nil, nil); rec.Code != http.StatusOK {
				t.Fatalf("registering: got %d", rec.Code)
			}
			if rec := h.get(t, "/verify?token="+url.QueryEscape(linkToken(t, h.mail.latest(t))),
				nil); rec.Code != http.StatusOK {
				t.Fatalf("verifying: got %d", rec.Code)
			}

			rec := h.post(t, "/login", url.Values{
				"email": {email}, "password": {testPassword}, "next": {tc.next},
			}, nil, nil)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("signing in: got %d, want 303", rec.Code)
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Errorf("signing in with next=%q landed on %q, want %q", tc.next, got, tc.want)
			}
		})
	}
}

// TestABearerTokenIsSignedOutOnEveryPage is AC-3. A page authenticates on the
// cookie alone, so an API token is not a second door into the browser surface,
// and a route that quietly accepted one would pass every other test here.
// covers: AC-3
func TestABearerTokenIsSignedOutOnEveryPage(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "bearer@example.test")
	account := h.accountID(t, cookie)

	minted, err := h.srv.svc.MintToken(t.Context(),
		identity.Account{ID: account, Email: "bearer@example.test", Verified: true}, "agent", 0)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	for _, path := range []string{"/apps", "/tokens", "/admin/accounts", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+minted.Raw)
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Errorf("GET %s with a bearer token: got %d, want 303 to sign in", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "/login") {
			t.Errorf("GET %s with a bearer token redirects to %q, want /login", path, got)
		}
	}
}

// TestLogoutRevokesTheSessionAndClearsTheCookie is AC-11: the cookie is not
// merely dropped, the session behind it stops authenticating. covers: AC-11
func TestLogoutRevokesTheSessionAndClearsTheCookie(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "out@example.test")

	rec := h.post(t, "/logout", url.Values{"csrf": {h.csrfFor(t, cookie)}}, cookie, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("signing out: got %d to %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("signing out did not clear the session cookie")
	}
	// The old cookie value must now be dead, not merely unset in this response.
	if after := h.get(t, "/apps", cookie); after.Code != http.StatusSeeOther {
		t.Errorf("the revoked session still renders /apps: got %d, want 303", after.Code)
	}
}

// TestAnAuthenticatedPostNeedsItsSynchroniserToken is AC-12. Each case must
// change nothing: the assertion is that no token was minted, not only that the
// response was 403. covers: AC-12
func TestAnAuthenticatedPostNeedsItsSynchroniserToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		csrf func(good string) string
	}{
		{"missing", func(string) string { return "" }},
		{"empty", func(string) string { return "" }},
		{"altered by one character", func(good string) string {
			// Pick a replacement that differs from whatever is already there,
			// otherwise the token comes back unaltered and the case is a no op.
			if good[0] == 'a' {
				return "b" + good[1:]
			}
			return "a" + good[1:]
		}},
		{"a plausible looking other value", func(string) string {
			return strings.Repeat("ab", 32)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			cookie := h.signIn(t, "csrf@example.test")
			good := h.csrfFor(t, cookie)

			rec := h.post(t, "/tokens", url.Values{
				"name": {"should not exist"}, "csrf": {tc.csrf(good)},
			}, cookie, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("a %s csrf value: got %d, want 403", tc.name, rec.Code)
			}
			row, ok := h.audit.last(auth.ActionPageCSRF)
			if !ok || row.Reason != "csrf_invalid" {
				t.Errorf("a %s csrf value wrote %+v, want a csrf_invalid row", tc.name, row)
			}
			// Nothing changed is the half a status code cannot prove.
			tokens, err := h.srv.svc.ListTokens(t.Context(), h.accountID(t, cookie))
			if err != nil {
				t.Fatalf("listing tokens: %v", err)
			}
			if len(tokens) != 0 {
				t.Errorf("a %s csrf value still minted %d token(s)", tc.name, len(tokens))
			}
		})
	}
}

// TestTheRightSynchroniserTokenIsAccepted is the other side of AC-12: the guard
// has to let the real form through, or every refusal above proves nothing.
// covers: AC-12
func TestTheRightSynchroniserTokenIsAccepted(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "good@example.test")

	rec := h.post(t, "/tokens", url.Values{
		"name": {"laptop agent"}, "csrf": {h.csrfFor(t, cookie)},
	}, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("minting with a good csrf value: got %d, want 200: %s", rec.Code, rec.Body)
	}
	tokens, err := h.srv.svc.ListTokens(t.Context(), h.accountID(t, cookie))
	if err != nil {
		t.Fatalf("listing tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens after minting one, want 1", len(tokens))
	}
}

// TestASynchroniserTokenDiesWithItsSession is why the token is derived rather
// than stored: it is revoked by the same act that revokes the session, with no
// second table to clean up. covers: AC-12
func TestASynchroniserTokenDiesWithItsSession(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "derived@example.test")
	stale := h.csrfFor(t, cookie)

	if rec := h.post(t, "/logout", url.Values{"csrf": {stale}}, cookie, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("signing out: got %d, want 303", rec.Code)
	}
	second := h.login(t, "derived@example.test")
	if fresh := h.csrfFor(t, second); fresh == stale {
		t.Error("a new session derived the same csrf token as the revoked one")
	}
}

// TestACrossSitePostIsRefusedBeforeTheHandlerRuns is AC-13, on both the guarded
// authenticated path and the pre authentication form where this check is the
// whole guard. covers: AC-13
func TestACrossSitePostIsRefusedBeforeTheHandlerRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		reason  string
	}{
		{"Sec-Fetch-Site says cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, "origin_cross_site"},
		{"Sec-Fetch-Site says same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, "origin_cross_site"},
		{"a foreign Origin", map[string]string{"Origin": "https://evil.test"}, "origin_mismatch"},
		{"the right host on the wrong scheme", map[string]string{"Origin": "http://deploy.example.test"}, "origin_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			cookie := h.signIn(t, "origin@example.test")

			rec := h.post(t, "/tokens", url.Values{
				"name": {"should not exist"}, "csrf": {h.csrfFor(t, cookie)},
			}, cookie, tc.headers)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: got %d, want 403", tc.name, rec.Code)
			}
			row, ok := h.audit.last(auth.ActionPageCSRF)
			if !ok || row.Reason != tc.reason {
				t.Errorf("%s wrote %+v, want reason %q", tc.name, row, tc.reason)
			}
			tokens, err := h.srv.svc.ListTokens(t.Context(), h.accountID(t, cookie))
			if err != nil {
				t.Fatalf("listing tokens: %v", err)
			}
			if len(tokens) != 0 {
				t.Errorf("%s still minted %d token(s)", tc.name, len(tokens))
			}

			// The same headers on a pre authentication form, where this check is
			// the only guard there is.
			pre := h.post(t, "/forgot", url.Values{"email": {"someone@example.test"}}, nil, tc.headers)
			if pre.Code != http.StatusForbidden {
				t.Errorf("%s on /forgot: got %d, want 403", tc.name, pre.Code)
			}
		})
	}
}

// TestAPostCarryingNeitherHeaderIsAllowed pins the stated hole in AC-13 rather
// than leaving it to be re argued: a request with neither header is an
// unauthenticated request, which the session gate refuses anyway.
// covers: AC-13
func TestAPostCarryingNeitherHeaderIsAllowed(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "headerless@example.test")

	rec := h.post(t, "/tokens", url.Values{
		"name": {"headerless"}, "csrf": {h.csrfFor(t, cookie)},
	}, cookie, map[string]string{"Sec-Fetch-Site": "", "Origin": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("a post with neither header: got %d, want 200", rec.Code)
	}
}

// TestClientAddressPrefersTheLastForwardedHop is what a rate limit bucket
// belongs to. The last hop rather than the first is the one the ingress wrote,
// so a caller cannot claim someone else's bucket by sending a header.
func TestClientAddressPrefersTheLastForwardedHop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		forwarded  string
		remoteAddr string
		want       string
	}{
		{"no header falls back to the connection", "", "192.0.2.7:5555", "192.0.2.7"},
		{"a connection with no port is used whole", "", "192.0.2.7", "192.0.2.7"},
		{"one hop", "198.51.100.4", "10.0.0.1:1", "198.51.100.4"},
		{"the last hop wins", "203.0.113.9, 198.51.100.4", "10.0.0.1:1", "198.51.100.4"},
		{"spacing is trimmed", "203.0.113.9,   198.51.100.4  ", "10.0.0.1:1", "198.51.100.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := clientAddress(req); got != tc.want {
				t.Errorf("clientAddress = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStatusForPairsEachRefusalWithOneStatus keeps the browser and the JSON
// surface answering the same code with the same status, so one refusal cannot
// mean two things on two surfaces.
func TestStatusForPairsEachRefusalWithOneStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code identity.Code
		want int
	}{
		{identity.CodeEmailInvalid, http.StatusUnprocessableEntity},
		{identity.CodePasswordTooShort, http.StatusUnprocessableEntity},
		{identity.CodeCredentialsInvalid, http.StatusUnauthorized},
		{identity.CodeEmailUnverified, http.StatusForbidden},
		{identity.CodeAdminRequired, http.StatusForbidden},
		{identity.CodeLinkInvalid, http.StatusBadRequest},
		{identity.CodeTokenNameTaken, http.StatusConflict},
		{identity.CodeNotFound, http.StatusNotFound},
		{identity.CodeRateLimited, http.StatusTooManyRequests},
		{identity.CodeMailUnavailable, http.StatusServiceUnavailable},
		{identity.Code("something added later"), http.StatusInternalServerError},
	} {
		if got := statusFor(tc.code); got != tc.want {
			t.Errorf("statusFor(%q) = %d, want %d", tc.code, got, tc.want)
		}
	}
}
