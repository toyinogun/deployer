package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The public edge, spec 0021. What these test is which host answers which route,
// which is decided by the mux's own most specific match rule rather than by a
// classifier beside it (AC-2a).

// edgeMux mirrors what the composition root builds: the page routes, plus the
// two surfaces that live on the same mux and are registered on the bare host
// only. Without those two, a 404 on the console would prove nothing, because an
// unregistered route 404s everywhere.
func (h *harness) edgeMux() *http.ServeMux {
	mux := http.NewServeMux()
	answered := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(name))
		}
	}
	mux.HandleFunc("POST /mcp", answered("mcp"))
	mux.HandleFunc("/v1/", answered("v1"))
	h.srv.Register(mux)
	return mux
}

// onHost runs one request against the edge mux with an explicit Host.
func (h *harness) onHost(t *testing.T, mux *http.ServeMux, method, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://"+host+path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestTheConsoleHostAnswersOnlyThePublicRoutes is AC-2 and AC-2a. Every admin
// page, /mcp and every /v1/ path answers 404 on the console host, and the public
// pages answer.
func TestTheConsoleHostAnswersOnlyThePublicRoutes(t *testing.T) {
	// covers: AC-2, AC-2a
	t.Parallel()
	h := newHarness(t, nil)
	mux := h.edgeMux()

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/accounts"},
		{http.MethodGet, "/admin/invites"},
		{http.MethodGet, "/admin/backups"},
		{http.MethodPost, "/admin/backups/run"},
		{http.MethodPost, "/admin/invites"},
		{http.MethodPost, "/admin/accounts/acc_1/disable"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/v1/auth/me"},
		{http.MethodPost, "/v1/uploads"},
	} {
		rec := h.onHost(t, mux, tc.method, testConsoleHost, tc.path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the console: got %d, want 404, so the open internet can reach it",
				tc.method, tc.path, rec.Code)
		}
	}

	// The same paths on the tailnet name are answered by whatever owns them:
	// the admin pages redirect a signed out caller to sign in, and the two other
	// surfaces answer for themselves. What matters is that none of them 404s.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/accounts"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/v1/auth/me"},
	} {
		rec := h.onHost(t, mux, tc.method, "deploy.example.test", tc.path)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s on the tailnet: got 404, so the tailnet stopped being a complete door",
				tc.method, tc.path)
		}
	}
}

// TestEveryPublicRouteAnswersOnBothHosts is AC-2a and AC-3. Each is registered
// twice, method included: a route doubled as GET only would 404 its own POST on
// the one host it exists to serve.
func TestEveryPublicRouteAnswersOnBothHosts(t *testing.T) {
	// covers: AC-2a, AC-3, AC-4
	t.Parallel()
	h := newHarness(t, nil)
	mux := h.edgeMux()

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/login"},
		{http.MethodPost, "/login"},
		{http.MethodGet, "/register"},
		{http.MethodPost, "/register"},
		{http.MethodGet, "/verify"},
		{http.MethodGet, "/unverified"},
		{http.MethodPost, "/resend"},
		{http.MethodGet, "/forgot"},
		{http.MethodPost, "/forgot"},
		{http.MethodGet, "/reset"},
		{http.MethodPost, "/reset"},
		{http.MethodPost, "/logout"},
		{http.MethodGet, "/apps"},
		{http.MethodGet, "/apps/hello"},
		{http.MethodGet, "/apps/hello/releases"},
		{http.MethodGet, "/apps/hello/logs"},
		{http.MethodGet, "/apps/hello/config"},
		{http.MethodPost, "/apps/hello/config/KEY/reveal"},
		// Joining's two routes, doubled method and all: a GET registered alone
		// would leave the mint form to the catch all on the one host the page
		// exists to serve (spec 0023, AC-1, AC-2).
		{http.MethodGet, "/connect"},
		{http.MethodPost, "/connect"},
		{http.MethodGet, "/tokens"},
		{http.MethodPost, "/tokens"},
		{http.MethodPost, "/tokens/tok_1/revoke"},
		{http.MethodGet, "/static/app.css"},
	} {
		// Three hosts: the console, the tailnet, and something that is neither,
		// which is what an in cluster caller and a health probe arrive as.
		for _, host := range []string{testConsoleHost, "deploy.example.test", "10.42.0.3:8080"} {
			rec := h.onHost(t, mux, tc.method, host, tc.path)
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s %s on %s: got 404, so a public route is missing a registration",
					tc.method, tc.path, host)
			}
		}
	}
}

// TestARouteRegisteredOnceIsPrivate is the direction this fails in. Registration
// is opt in, so a future route nobody remembers to double is unreachable from the
// open internet rather than exposed on it.
func TestARouteRegisteredOnceIsPrivate(t *testing.T) {
	// covers: AC-2a
	t.Parallel()
	h := newHarness(t, nil)
	mux := h.edgeMux()
	mux.HandleFunc("GET /something-added-later", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if rec := h.onHost(t, mux, http.MethodGet, "deploy.example.test", "/something-added-later"); rec.Code != http.StatusOK {
		t.Fatalf("the new route does not answer on the tailnet: got %d", rec.Code)
	}
	if rec := h.onHost(t, mux, http.MethodGet, testConsoleHost, "/something-added-later"); rec.Code != http.StatusNotFound {
		t.Errorf("a route registered once answered on the console with %d, so forgetting to double a route publishes it", rec.Code)
	}
}

// TestTheConsoleRefusalWritesNoAuditRow is AC-2. No caller has been identified at
// that point, so there is nobody to record the refusal against.
func TestTheConsoleRefusalWritesNoAuditRow(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t, nil)
	mux := h.edgeMux()
	h.onHost(t, mux, http.MethodGet, testConsoleHost, "/admin/accounts")
	if got := len(h.audit.rows); got != 0 {
		t.Errorf("the refusal wrote %d audit rows, want 0", got)
	}
}

// TestTheAdminNavigationIsAbsentOnTheConsole is AC-5. The split is visible in the
// interface rather than discovered as an error, so an administrator signed in on
// the console is shown no link to a page that would answer 404.
func TestTheAdminNavigationIsAbsentOnTheConsole(t *testing.T) {
	// covers: AC-5
	t.Parallel()
	h := newHarness(t, nil)
	mux := h.edgeMux()
	cookie := h.signIn(t, "admin@example.test") // the first account is the admin

	body := func(host string) string {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/apps", nil)
		req.Host = host
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if !strings.Contains(body("deploy.example.test"), "/admin/accounts") {
		t.Fatal("the admin navigation is missing on the tailnet, so this test is not measuring the console")
	}
	for _, link := range []string{"/admin/accounts", "/admin/invites", "/admin/backups"} {
		if strings.Contains(body(testConsoleHost), link) {
			t.Errorf("the console renders a link to %s, which it would answer 404 for", link)
		}
	}
}
