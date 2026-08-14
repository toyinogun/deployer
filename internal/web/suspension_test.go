package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// TestTheAdminPageReadsSuspendAndRestore is AC-20. The words a person reads
// changed even though the column did not, because disable understated what the
// control now does. covers: AC-20
func TestTheAdminPageReadsSuspendAndRestore(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	victim := h.signIn(t, "second@example.test")
	target := h.accountID(t, victim)

	body := h.get(t, "/admin/accounts", admin).Body.String()
	if !strings.Contains(body, "Suspend account") {
		t.Error("the admin page has no Suspend control")
	}
	// The confirmation sentence has to say what suspending now does, because the
	// apps stopping is the part a person would not have guessed from disable.
	if !strings.Contains(body, "stops its apps serving") {
		t.Error("the confirmation does not say the account's apps will stop serving")
	}

	suspend := h.post(t, "/admin/accounts/"+target+"/disable", url.Values{
		"confirm_email": {"second@example.test"}, "csrf": {h.csrfFor(t, admin)},
	}, admin, nil)
	if suspend.Code != http.StatusSeeOther {
		t.Fatalf("suspending: got %d, want 303: %s", suspend.Code, suspend.Body)
	}

	body = h.get(t, "/admin/accounts", admin).Body.String()
	if !strings.Contains(body, "suspended") {
		t.Error("the column does not read suspended")
	}
	if !strings.Contains(body, "Restore") {
		t.Error("a suspended account has no Restore control")
	}
}

// TestAPartialStopIsRenderedRatherThanRedirected is AC-6 on the page. A cluster
// that refuses one app is a third outcome beside success and failure: the account
// is suspended, and the page names the app rather than pretending everything
// stopped. covers: AC-6, AC-19
func TestAPartialStopIsRenderedRatherThanRedirected(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	victim := h.signIn(t, "second@example.test")
	target := h.accountID(t, victim)
	app := h.deployedApp(t, target, "stubborn")
	h.scaler.refuse[deploy.NamespaceName(app.Slug)] = true

	got := h.post(t, "/admin/accounts/"+target+"/disable", url.Values{
		"confirm_email": {"second@example.test"}, "csrf": {h.csrfFor(t, admin)},
	}, admin, nil)

	if got.Code != http.StatusOK {
		t.Fatalf("a partial stop: got %d, want 200 with a message", got.Code)
	}
	if !strings.Contains(got.Body.String(), app.Slug) {
		t.Errorf("the page does not name the app that did not stop: %s", got.Body.String())
	}
	// The lockout landed anyway, which is the invariant the message exists to
	// report rather than to qualify.
	if got := h.get(t, "/apps", victim); got.Code != http.StatusSeeOther {
		t.Errorf("a partial stop left the suspended account's session alive: /apps got %d", got.Code)
	}
}

// TestAnAdminCannotSuspendThemselves is AC-17. The page renders no control on
// your own row, but a form post is not the page, so the refusal has to be in the
// handler or an admin can lock themselves out with nobody left to undo it.
// covers: AC-17
func TestAnAdminCannotSuspendThemselves(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	self := h.accountID(t, admin)

	got := h.post(t, "/admin/accounts/"+self+"/disable", url.Values{
		"confirm_email": {"first@example.test"}, "csrf": {h.csrfFor(t, admin)},
	}, admin, nil)

	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("suspending yourself: got %d, want 422", got.Code)
	}
	if !h.audit.hasReason(auth.ActionAdmin, "suspend: self") {
		t.Errorf("a self suspension wrote no audit row: %+v", h.audit.all())
	}
	// Still signed in, and still an admin.
	if got := h.get(t, "/admin/accounts", admin); got.Code != http.StatusOK {
		t.Errorf("the admin lost their own session: got %d", got.Code)
	}
}

// TestAnOrdinarySessionCannotSuspendAnyone is the authorization half of AC-17:
// the admin session is the authorization, and the typed address only ever
// confirms which row was meant. covers: AC-17
func TestAnOrdinarySessionCannotSuspendAnyone(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	ordinary := h.signIn(t, "second@example.test")
	target := h.accountID(t, admin)

	for _, path := range []string{"/disable", "/enable"} {
		got := h.post(t, "/admin/accounts/"+target+path, url.Values{
			"confirm_email": {"first@example.test"}, "csrf": {h.csrfFor(t, ordinary)},
		}, ordinary, nil)
		if got.Code == http.StatusSeeOther || got.Code == http.StatusOK {
			t.Errorf("an ordinary session reached %s: got %d", path, got.Code)
		}
	}
}

// TestSigningInIsNotAnAccountStateOracle is AC-13. A suspended person gets
// exactly the answer a wrong password gets, so the login form never becomes a way
// to learn which accounts exist and which are stopped. covers: AC-13
func TestSigningInIsNotAnAccountStateOracle(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.signIn(t, "first@example.test")
	victim := h.signIn(t, "second@example.test")
	target := h.accountID(t, victim)

	if got := h.post(t, "/admin/accounts/"+target+"/disable", url.Values{
		"confirm_email": {"second@example.test"}, "csrf": {h.csrfFor(t, admin)},
	}, admin, nil); got.Code != http.StatusSeeOther {
		t.Fatalf("suspending: got %d", got.Code)
	}

	// The same address both times, so the only thing that varies is whether the
	// password was right. The form echoes the address it was given, and a
	// comparison across two different addresses would be measuring that echo
	// rather than measuring what the platform revealed.
	rightPassword := h.attemptLogin(t, "second@example.test", testPassword)
	wrongPassword := h.attemptLogin(t, "second@example.test", "not-the-password")

	if rightPassword.Code != wrongPassword.Code {
		t.Errorf("a suspended account with the right password answers %d and a wrong password answers %d; "+
			"they must be identical", rightPassword.Code, wrongPassword.Code)
	}
	if rightPassword.Body.String() != wrongPassword.Body.String() {
		t.Error("the right password on a suspended account renders a different page than a wrong one, " +
			"so the form is an account state oracle")
	}
	// And an address that was never registered answers the same way, so the form
	// does not reveal which accounts exist either.
	if unknown := h.attemptLogin(t, "nobody@example.test", testPassword); unknown.Code != wrongPassword.Code {
		t.Errorf("an unknown address answers %d, want the same %d a suspended one does",
			unknown.Code, wrongPassword.Code)
	}
	// And the session it held before is dead, not merely unrenewable.
	if got := h.get(t, "/apps", victim); got.Code != http.StatusSeeOther {
		t.Errorf("a suspended account's session still resolves: /apps got %d", got.Code)
	}
}

// attemptLogin posts the sign in form and hands back the response untouched, so
// two attempts can be compared byte for byte.
func (h *harness) attemptLogin(t *testing.T, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, "/login", url.Values{"email": {email}, "password": {password}}, nil, nil)
}

// deployedApp gives an account an app that has reached the cluster, which is what
// makes it one a suspension stops. Driven through the real states, because
// current_release_id is what the live predicate reads.
func (h *harness) deployedApp(t *testing.T, accountID, name string) store.App {
	t.Helper()
	ctx := t.Context()
	app, err := h.store.CreateApp(ctx, accountID, name, 10)
	if err != nil {
		t.Fatalf("creating app %s: %v", name, err)
	}
	up, err := h.store.CreateUpload(ctx, store.NewUpload{
		AccountID: accountID, Path: "/tmp/" + name + ".tar.gz", SizeBytes: 2048,
		SHA256: "abc", FetchTokenHash: "fetch-" + name,
		ExpiresAt: ids.Stamp(time.Now().UTC().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("creating the upload for %s: %v", name, err)
	}
	dep, _, err := h.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: app.ID, AccountID: accountID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("creating the deployment for %s: %v", name, err)
	}
	for _, to := range []domain.State{domain.StateBuilding, domain.StatePushing, domain.StateDeploying} {
		if _, err := h.store.Transition(ctx, dep.ID, to, "", ""); err != nil {
			t.Fatalf("moving %s to %s: %v", name, to, err)
		}
	}
	if err := h.store.RecordBuildResult(ctx, dep.ID, store.BuildResult{
		BuildPath: "buildpacks", ImageRepo: "registry.example/" + app.Slug, ImageDigest: "sha256:abc",
	}); err != nil {
		t.Fatalf("recording the build for %s: %v", name, err)
	}
	if _, _, err := h.store.MarkHealthy(ctx, dep.ID, map[string]domain.ConfigValue{}); err != nil {
		t.Fatalf("marking %s healthy: %v", name, err)
	}
	return app
}
