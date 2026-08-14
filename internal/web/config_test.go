package web

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// configuredApp is an app carrying one secret key, one ordinary key with a
// value, and one key that has never been set.
func configuredApp(t *testing.T, h *harness, cookie *http.Cookie, slug string) {
	t.Helper()
	h.ownApp(h.accountID(t, cookie), slug)
	h.data.config = []store.ConfigEntry{
		{Key: "DATABASE_URL", Value: "", IsSecret: true},
		{Key: "REGION", Value: "eu-west-1"},
		{Key: "FEATURE_FLAG", Value: ""},
	}
}

// TestConfigMasksEveryValueAndOffersNoControlForASecret is AC-20's listing half.
// A missing button is a display decision, so the absence is worth pinning, but
// the route below is the guard that actually holds. covers: AC-20
func TestConfigMasksEveryValueAndOffersNoControlForASecret(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "config@example.test")
	configuredApp(t, h, cookie, "configured")

	body := h.get(t, "/apps/configured/config", cookie).Body.String()
	for _, want := range []string{"DATABASE_URL", "REGION", "FEATURE_FLAG"} {
		if !strings.Contains(body, want) {
			t.Errorf("the config page does not list %q", want)
		}
	}
	// No value is on the page until it is asked for, secret or not.
	if strings.Contains(body, "eu-west-1") {
		t.Error("the config page shows a value before it was revealed")
	}
	// A key that has never been set reads as unset, not as a masked empty value.
	if !strings.Contains(body, "not set") {
		t.Error("an unset key does not say so")
	}
	if strings.Contains(body, "/apps/configured/config/DATABASE_URL/reveal") {
		t.Error("the config page offers a reveal control for a secret key")
	}
	if !strings.Contains(body, "/apps/configured/config/REGION/reveal") {
		t.Error("the config page offers no reveal control for an ordinary key")
	}
}

// TestRevealingAnOrdinaryKeyShowsItAndWritesAnAuditRow is AC-20's happy path.
// The row has to name all three of account, app and key, or it cannot answer
// who saw what. covers: AC-20
func TestRevealingAnOrdinaryKeyShowsItAndWritesAnAuditRow(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "reveal@example.test")
	configuredApp(t, h, cookie, "configured")
	app := h.data.apps["configured"]

	rec := h.post(t, "/apps/configured/config/REGION/reveal",
		url.Values{"csrf": {h.csrfFor(t, cookie)}}, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revealing REGION: got %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "eu-west-1") {
		t.Error("revealing an ordinary key did not show its value")
	}
	row, ok := h.audit.last(auth.ActionConfigReveal)
	if !ok || !row.Allowed {
		t.Fatalf("revealing wrote %+v, want an allowed row", row)
	}
	if row.AccountID != h.accountID(t, cookie) || row.TargetID != app.ID+"/REGION" {
		t.Errorf("the reveal row is %+v, want it to name the account, the app and the key", row)
	}
}

// TestRevealingASecretKeyIsRefusedByTheRouteItself is AC-20's load bearing half:
// the browser is never a weaker door than the agent, so the guard cannot be the
// template's decision not to draw a button. covers: AC-20
func TestRevealingASecretKeyIsRefusedByTheRouteItself(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "secret@example.test")
	configuredApp(t, h, cookie, "configured")
	// A value is present in the row the store hands back, so this proves the
	// route's refusal and not merely that there was nothing to show.
	h.data.config[0].Value = "postgres://user:hunter2@db/app"
	app := h.data.apps["configured"]

	rec := h.post(t, "/apps/configured/config/DATABASE_URL/reveal",
		url.Values{"csrf": {h.csrfFor(t, cookie)}}, cookie, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revealing a secret key: got %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Error("the refusal still carried the secret value")
	}
	row, ok := h.audit.last(auth.ActionConfigReveal)
	if !ok || row.Allowed || row.Reason != "config_key_secret" {
		t.Errorf("refusing a secret key wrote %+v, want a refused config_key_secret row", row)
	}
	if row.TargetID != app.ID+"/DATABASE_URL" {
		t.Errorf("the refusal row names %q, want the app and the key", row.TargetID)
	}
}

// TestRevealingAnUnknownKeyIsRefusedAndRecorded keeps the reveal route from
// being a way to learn which keys exist. covers: AC-20
func TestRevealingAnUnknownKeyIsRefusedAndRecorded(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "unknown-key@example.test")
	configuredApp(t, h, cookie, "configured")

	secret := h.post(t, "/apps/configured/config/DATABASE_URL/reveal",
		url.Values{"csrf": {h.csrfFor(t, cookie)}}, cookie, nil)
	unknown := h.post(t, "/apps/configured/config/NO_SUCH_KEY/reveal",
		url.Values{"csrf": {h.csrfFor(t, cookie)}}, cookie, nil)

	if unknown.Code != secret.Code {
		t.Errorf("an unknown key gets %d and a secret one gets %d, want the same answer",
			unknown.Code, secret.Code)
	}
	if unknown.Body.String() != secret.Body.String() {
		t.Error("an unknown key is told apart from a secret one")
	}
	if !h.audit.hasReason(auth.ActionConfigReveal, string(domain.ReasonConfigKeyUnknown)) {
		t.Errorf("an unknown key wrote no config_key_unknown row: %+v", h.audit.all())
	}
}

// TestRevealIsCSRFGuarded keeps the one page action that returns a value behind
// the same guard every other POST is behind. covers: AC-12, AC-20
func TestRevealIsCSRFGuarded(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "reveal-csrf@example.test")
	configuredApp(t, h, cookie, "configured")

	rec := h.post(t, "/apps/configured/config/REGION/reveal", url.Values{}, cookie, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revealing with no csrf value: got %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "eu-west-1") {
		t.Error("an unguarded reveal returned the value anyway")
	}
}

// TestTheBrowserIsNotAThirdCallerOfListConfigForDeploy pins the invariant the
// logs page's redaction was built around. The page assembles its literals from
// ListConfigForResponse and CurrentReleaseConfig, so the deploy read keeps its
// two callers and the browser cannot become one by accident.
// covers: AC-20
func TestTheBrowserIsNotAThirdCallerOfListConfigForDeploy(t *testing.T) {
	t.Parallel()
	data := reflect.TypeOf((*Data)(nil)).Elem()
	for i := range data.NumMethod() {
		if name := data.Method(i).Name; name == "ListConfigForDeploy" {
			t.Fatal("the web package's Data interface declares ListConfigForDeploy")
		}
	}
}
