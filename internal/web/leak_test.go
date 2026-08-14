package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/logs"
	"github.com/toyinogun/deployer/internal/store"
)

// TestNoPageOrLogLineCarriesACredential is AC-31, crawled the way the manual
// check crawls it: every page a signed in account can reach, plus the platform's
// own log at info level, asserted against the four things that must never appear
// in either. The session cookie is the one worth naming: it is why the
// synchroniser token is derived from the session id and not from the cookie.
// covers: AC-31
func TestNoPageOrLogLineCarriesACredential(t *testing.T) {
	// Capture the platform's own log for the length of the crawl.
	var captured bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&captured, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	const (
		secretValue = "postgres://user:hunter2@db/app"
		logLine     = "2026-08-14T09:00:00Z connecting with " + secretValue
	)
	h := newHarness(t, &fakePods{
		pods: []logs.PodStatus{{Name: "p1", ContainerStarted: true}},
		out:  logLine + "\n2026-08-14T09:00:01Z ready\n",
	})
	admin := h.signIn(t, "crawler@example.test")
	account := h.accountID(t, admin)

	app := h.ownApp(account, "crawled")
	app.CurrentReleaseID = ptr("rel_1")
	h.data.apps["crawled"] = app
	h.data.releases["rel_1"] = store.Release{ID: "rel_1", ReleaseNumber: 1, ImageDigest: "sha256:aaa"}
	h.data.byApp = []store.Release{{ID: "rel_1", ReleaseNumber: 1, ImageDigest: "sha256:aaa"}}
	h.data.summaries = []store.AppSummary{{
		ID: app.ID, Name: app.Name, Slug: app.Slug, ServingRelease: 1,
		LastDeploymentID: "dep_1", LastDeploymentState: string(domain.StateHealthy),
	}}
	h.data.deployment = store.Deployment{ID: "dep_1", AppID: app.ID, State: string(domain.StateHealthy)}
	h.data.config = []store.ConfigEntry{{Key: "DATABASE_URL", Value: secretValue, IsSecret: true}}
	h.data.ranConfig = map[string]string{"DATABASE_URL": secretValue}

	minted, err := h.srv.svc.MintToken(t.Context(),
		identity.Account{ID: account, Email: "crawler@example.test", Verified: true}, "agent", 0)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	// A live invite whose code nothing on any page may carry. It is a working
	// credential for as long as it is unspent, exactly as the session cookie and
	// the API token beside it are (spec 0015, AC-14).
	inviteCode := h.invite(t)

	forbidden := map[string]string{
		"the session cookie value": admin.Value,
		"a raw API token":          minted.Raw,
		"a secret config value":    secretValue,
		"a password":               testPassword,
		"a raw invite code":        inviteCode,
	}

	for _, path := range []string{
		"/apps",
		"/apps/crawled",
		"/apps/crawled?partial=status",
		"/apps/crawled/releases",
		"/apps/crawled/logs",
		"/apps/crawled/config",
		"/tokens",
		"/admin/accounts",
		"/admin/invites",
		"/login", "/register", "/forgot", "/reset", "/unverified",
		// The register page carries whatever code it was handed straight into a
		// hidden field, so it is exempt from the crawl's own code: what matters
		// there is that no OTHER page carries one, and that this one never
		// reveals whether the code is any good (AC-18).
	} {
		rec := h.get(t, path, admin)
		if rec.Code >= http.StatusInternalServerError {
			t.Errorf("GET %s: got %d", path, rec.Code)
		}
		for what, value := range forbidden {
			if strings.Contains(rec.Body.String(), value) {
				t.Errorf("GET %s carries %s in its body", path, what)
			}
		}
		// A redirect's Location is part of the response too, and a token in a URL
		// lands in browser history and every proxy log on the way.
		if location := rec.Header().Get("Location"); location != "" {
			for what, value := range forbidden {
				if strings.Contains(location, value) {
					t.Errorf("GET %s redirects to a URL carrying %s", path, what)
				}
			}
		}
	}

	for what, value := range forbidden {
		if strings.Contains(captured.String(), value) {
			t.Errorf("the platform log at info level carries %s", what)
		}
	}
}

// TestAVerificationLinkTokenNeverReachesAPage is the other half of AC-31: the
// link token is a credential for as long as it is unused, so it must not survive
// into the page the link renders. covers: AC-31
func TestAVerificationLinkTokenNeverReachesAPage(t *testing.T) {
	h := newHarness(t, nil)
	if rec := h.post(t, "/register", h.registration(t, "link@example.test"), nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("registering: got %d", rec.Code)
	}
	token := linkToken(t, h.mail.latest(t))

	rec := h.get(t, "/verify?token="+token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verifying: got %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Error("the verified page echoes the link token back into the body")
	}
}

// TestEveryPageForbidsCaching is what keeps a session gated read out of a shared
// cache, and keeps the back button from showing the previous account's list.
func TestEveryPageForbidsCaching(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "cache@example.test")

	for _, tc := range []struct {
		path string
		as   *http.Cookie
	}{
		{"/apps", cookie},
		{"/tokens", cookie},
		{"/apps/nosuch", cookie}, // the refusal page too
		{"/login", nil},          // signed in, /login redirects instead of rendering
	} {
		rec := h.get(t, tc.path, tc.as)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s has Cache-Control %q, want no-store", tc.path, got)
		}
	}
}
