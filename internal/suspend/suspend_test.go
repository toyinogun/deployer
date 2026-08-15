// Package suspend_test drives the suspension use case over a real SQLite file
// and the client-go fake clientset. It is an external test package because it
// uses internal/store, which imports internal/suspend.
//
// The store is never faked: the two reads this feature added are predicates, and
// a fake that answers them by hand would prove the predicate it was written
// against rather than the one that ships.
package suspend_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/suspend"
)

const adminID = "acc_the_admin"

// harness is the suspension service over a real store, a real identity service,
// and a scaler a test can make refuse.
type harness struct {
	svc     *suspend.Service
	store   *store.Store
	scaler  *fakeScaler
	audit   *recorder
	account store.Account
}

// fakeScaler is the cluster half. It records the replica count each namespace was
// asked for and can refuse one, which is the only way the partial outcome has to
// happen.
type fakeScaler struct {
	scaled map[string]int32
	refuse map[string]bool
	calls  int
	// onScale runs just before a scale lands, which is the seam a restore has to
	// arrive through for the sweep's re-read to mean anything.
	onScale func(namespace string)
}

func (f *fakeScaler) ScaleWorkload(_ context.Context, namespace, name string, replicas int32) error {
	f.calls++
	if f.onScale != nil {
		f.onScale(namespace)
	}
	if name != deploy.WorkloadName {
		return errors.New("the scale named something other than the app workload: " + name)
	}
	if f.refuse[namespace] {
		return errors.New("the cluster refused this namespace")
	}
	f.scaled[namespace] = replicas
	return nil
}

// recorder keeps the audit rows so both directions can be read back.
type recorder struct{ rows []auth.Audit }

func (r *recorder) RecordAudit(_ context.Context, a auth.Audit) error {
	r.rows = append(r.rows, a)
	return nil
}

// rowsFor is every audit row against one target type.
func (r *recorder) rowsFor(targetType string) []auth.Audit {
	var out []auth.Audit
	for _, row := range r.rows {
		if row.TargetType == targetType {
			out = append(out, row)
		}
	}
	return out
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(store.Options{Path: filepath.Join(dir, "deployer.db")})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	svcIdentity := identity.NewService(store.ForIdentity(st), nil, ids.SystemClock{},
		identity.Options{PublicURL: "https://deploy.example.org", Hasher: identity.NewHasherWith(2, 64, 1)})
	scaler := &fakeScaler{scaled: map[string]int32{}, refuse: map[string]bool{}}
	audit := &recorder{}

	h := &harness{
		svc:    suspend.New(store.ForSuspend(st), svcIdentity, scaler, audit),
		store:  st,
		scaler: scaler,
		audit:  audit,
	}
	h.account = h.enroll(t, "person@example.test")
	return h
}

// enroll registers a verified account, the state every real caller reaches.
func (h *harness) enroll(t *testing.T, email string) store.Account {
	t.Helper()
	acc, err := h.store.CreateIdentityAccount(t.Context(), store.NewIdentityAccount{
		Email: email, PasswordHash: "argon2id$fake", DisplayName: email,
	})
	if err != nil {
		t.Fatalf("registering %s: %v", email, err)
	}
	if err := h.store.MarkEmailVerified(t.Context(), acc.ID); err != nil {
		t.Fatalf("verifying %s: %v", email, err)
	}
	return acc
}

// deployedApp creates an app that has reached the cluster, which is what makes it
// one a suspension stops. An app with no release is created by neverDeployedApp.
func (h *harness) deployedApp(t *testing.T, account store.Account, name string) store.App {
	t.Helper()
	app := h.neverDeployedApp(t, account, name)
	// Driven through the real states rather than written in, because
	// current_release_id is what the live predicate reads and the only honest way
	// to have one is to have deployed.
	ctx := t.Context()
	up, err := h.store.CreateUpload(ctx, store.NewUpload{
		AccountID: account.ID, Path: "/tmp/" + name + ".tar.gz", SizeBytes: 2048,
		SHA256: "abc", FetchTokenHash: "fetch-" + name,
		ExpiresAt: ids.Stamp(time.Now().UTC().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("creating the upload for %s: %v", name, err)
	}
	dep, _, err := h.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: app.ID, AccountID: account.ID, UploadID: &up.ID,
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
	live, err := h.store.GetApp(ctx, app.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", name, err)
	}
	if live.CurrentReleaseID == nil {
		t.Fatalf("%s has no current release after a healthy deploy, so the fixture is wrong", name)
	}
	return live
}

func (h *harness) neverDeployedApp(t *testing.T, account store.Account, name string) store.App {
	t.Helper()
	app, err := h.store.CreateApp(t.Context(), account.ID, name, 10)
	if err != nil {
		t.Fatalf("creating app %s: %v", name, err)
	}
	return app
}

// suspended reads the account's state back out of the database, which is the one
// place suspension lives.
func (h *harness) suspended(t *testing.T, accountID string) bool {
	t.Helper()
	acc, err := h.store.GetAccount(t.Context(), accountID)
	if err != nil {
		t.Fatalf("reading account %s: %v", accountID, err)
	}
	return acc.DisabledAt != nil
}

// TestSuspendStopsEveryDeployedAppAndRestoreStartsThemAgain is the happy path in
// both directions. The apps that run go to zero and come back to one, the same
// constant a deploy composes with, and nothing new is written to bring them back.
// covers: AC-2, AC-3, AC-4
func TestSuspendStopsEveryDeployedAppAndRestoreStartsThemAgain(t *testing.T) {
	h := newHarness(t)
	first := h.deployedApp(t, h.account, "first")
	second := h.deployedApp(t, h.account, "second")

	result, err := h.svc.Suspend(t.Context(), adminID, h.account.ID)
	if err != nil {
		t.Fatalf("suspending: %v", err)
	}
	if len(result.NotStopped) != 0 {
		t.Fatalf("a clean suspension reported apps that did not stop: %v", result.NotStopped)
	}
	if !h.suspended(t, h.account.ID) {
		t.Error("the account was not suspended, so the lockout half never landed")
	}
	for _, app := range []store.App{first, second} {
		if got := h.scaler.scaled[deploy.NamespaceName(app.Slug)]; got != 0 {
			t.Errorf("%s is at %d replicas after a suspension, want 0", app.Slug, got)
		}
	}

	// Restore brings them back to exactly what a deploy would have composed, and
	// mints nothing: no deployment row, no release beyond the one each already had.
	before := h.countRows(t, "deployments")
	releasesBefore := h.countRows(t, "releases")
	if _, err := h.svc.Restore(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if h.suspended(t, h.account.ID) {
		t.Error("the account is still suspended after a restore")
	}
	for _, app := range []store.App{first, second} {
		if got := h.scaler.scaled[deploy.NamespaceName(app.Slug)]; got != deploy.ServingReplicas {
			t.Errorf("%s is at %d replicas after a restore, want %d", app.Slug, got, deploy.ServingReplicas)
		}
	}
	if after := h.countRows(t, "deployments"); after != before {
		t.Errorf("a restore wrote %d deployment row(s); it must rebuild nothing", after-before)
	}
	if after := h.countRows(t, "releases"); after != releasesBefore {
		t.Errorf("a restore minted %d release(s); it must rebuild nothing", after-releasesBefore)
	}
}

// TestAnAppThatNeverDeployedIsLeftAlone is AC-3's other half: the set is apps
// that have something to stop, so one that never reached the cluster is neither
// stopped nor restored. covers: AC-3
func TestAnAppThatNeverDeployedIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	ghost := h.neverDeployedApp(t, h.account, "ghost")

	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	if _, asked := h.scaler.scaled[deploy.NamespaceName(ghost.Slug)]; asked {
		t.Error("an app that never deployed was scaled, so the live predicate is wrong")
	}
	if h.scaler.calls != 0 {
		t.Errorf("the cluster was called %d time(s) for an account with nothing running", h.scaler.calls)
	}
}

// TestASoftDeletedAppIsLeftAlone is the same predicate from the other side: an
// app deleted while its owner was suspended must not come back on restore, which
// is precisely why the set is recomputed rather than remembered. covers: AC-3
func TestASoftDeletedAppIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	gone := h.deployedApp(t, h.account, "gone")
	kept := h.deployedApp(t, h.account, "kept")

	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	if err := h.store.SoftDeleteApp(t.Context(), gone.ID); err != nil {
		t.Fatalf("deleting %s: %v", gone.Slug, err)
	}
	h.scaler.scaled = map[string]int32{}

	if _, err := h.svc.Restore(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if _, brought := h.scaler.scaled[deploy.NamespaceName(gone.Slug)]; brought {
		t.Error("a restore brought back an app that was deleted while the account was suspended")
	}
	if got := h.scaler.scaled[deploy.NamespaceName(kept.Slug)]; got != deploy.ServingReplicas {
		t.Errorf("the kept app is at %d replicas, want %d", got, deploy.ServingReplicas)
	}
}

// TestOneRefusedAppNeverBlocksTheLockout is AC-6. The lockout is a database write
// and it lands first, so a cluster outage can never leave an abuser signed in.
// The apps that did not stop come back as data, not as an error string, because
// both admin surfaces render them. covers: AC-6
func TestOneRefusedAppNeverBlocksTheLockout(t *testing.T) {
	h := newHarness(t)
	stubborn := h.deployedApp(t, h.account, "stubborn")
	obliging := h.deployedApp(t, h.account, "obliging")
	h.scaler.refuse[deploy.NamespaceName(stubborn.Slug)] = true

	result, err := h.svc.Suspend(t.Context(), adminID, h.account.ID)
	if err != nil {
		t.Fatalf("a refused app failed the whole suspension: %v", err)
	}
	if !h.suspended(t, h.account.ID) {
		t.Error("a refused app left the account signed in, which is the one thing this must never do")
	}
	if len(result.NotStopped) != 1 || result.NotStopped[0] != stubborn.Slug {
		t.Errorf("NotStopped = %v, want exactly [%s]", result.NotStopped, stubborn.Slug)
	}
	// The loop kept going rather than stopping at the first failure.
	if got := h.scaler.scaled[deploy.NamespaceName(obliging.Slug)]; got != 0 {
		t.Errorf("the app after the refused one is at %d replicas, want 0", got)
	}
}

// TestBothDirectionsAreAudited is AC-16: one admin row naming the target account
// and which way it went, plus one row per app, and an app that failed to stop
// recorded as not allowed. covers: AC-16
func TestBothDirectionsAreAudited(t *testing.T) {
	h := newHarness(t)
	stubborn := h.deployedApp(t, h.account, "stubborn")
	obliging := h.deployedApp(t, h.account, "obliging")
	h.scaler.refuse[deploy.NamespaceName(stubborn.Slug)] = true

	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}

	accountRows := h.audit.rowsFor("account")
	if len(accountRows) != 1 {
		t.Fatalf("a suspension wrote %d account row(s), want 1: %+v", len(accountRows), accountRows)
	}
	row := accountRows[0]
	if row.AccountID != adminID || row.TargetID != h.account.ID || row.Action != auth.ActionAdmin ||
		!row.Allowed || row.Reason != "suspend" {
		t.Errorf("the account row is %+v, want an allowed admin suspend naming the admin and the target", row)
	}

	appRows := h.audit.rowsFor("app")
	if len(appRows) != 2 {
		t.Fatalf("a suspension wrote %d app row(s), want one per app: %+v", len(appRows), appRows)
	}
	for _, r := range appRows {
		wantAllowed := r.TargetID == obliging.ID
		if r.Allowed != wantAllowed {
			t.Errorf("app row %+v: Allowed = %v, want %v", r, r.Allowed, wantAllowed)
		}
		if r.Reason != "suspend" {
			t.Errorf("app row %+v: Reason = %q, want suspend", r, r.Reason)
		}
	}

	h.audit.rows = nil
	if _, err := h.svc.Restore(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if got := h.audit.rowsFor("account"); len(got) != 1 || got[0].Reason != "restore" {
		t.Errorf("a restore wrote %+v, want one account row reading restore", got)
	}
	if got := len(h.audit.rowsFor("app")); got != 2 {
		t.Errorf("a restore wrote %d app row(s), want one per app", got)
	}
}

// TestSweepHoldsSuspendedAppsAtZero is AC-7 and AC-8. Something put an app back
// to one replica behind the platform's back; the next tick takes it down again,
// and no app of an active account is touched at all. covers: AC-7, AC-8
func TestSweepHoldsSuspendedAppsAtZero(t *testing.T) {
	h := newHarness(t)
	stopped := h.deployedApp(t, h.account, "stopped")
	other := h.enroll(t, "other@example.test")
	running := h.deployedApp(t, other, "running")

	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	// Something brought it back up. The sweep's whole job is that this does not
	// last a tick.
	h.scaler.scaled = map[string]int32{deploy.NamespaceName(stopped.Slug): 1}

	h.svc.SweepSuspended(t.Context())

	if got := h.scaler.scaled[deploy.NamespaceName(stopped.Slug)]; got != 0 {
		t.Errorf("a suspended account's app is at %d replicas after a sweep, want 0", got)
	}
	if _, touched := h.scaler.scaled[deploy.NamespaceName(running.Slug)]; touched {
		t.Error("the sweep touched an active account's app, which it must never do")
	}
}

// TestSweepNeverScalesUp is AC-8 stated as the invariant rather than the case:
// there is exactly one caller that raises a replica count, and it is not this
// one. covers: AC-8
func TestSweepNeverScalesUp(t *testing.T) {
	h := newHarness(t)
	app := h.deployedApp(t, h.account, "site")
	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	h.scaler.scaled = map[string]int32{}

	h.svc.SweepSuspended(t.Context())

	for ns, replicas := range h.scaler.scaled {
		if replicas != 0 {
			t.Errorf("the sweep set %s to %d replicas; it may only ever scale down", ns, replicas)
		}
	}
	if _, swept := h.scaler.scaled[deploy.NamespaceName(app.Slug)]; !swept {
		t.Error("the sweep skipped a suspended account's app entirely")
	}
}

// TestARestoreInsideASweepSurvivesIt is AC-24. The sweep's list is a snapshot and
// its writes are not instantaneous, so the account is confirmed suspended
// immediately before each write. Without that re-read the sweep would take down
// an app the admin had just brought back.
//
// The race is made deterministic by restoring from inside the scaler, which is
// the moment between the list and the write. covers: AC-24
func TestARestoreInsideASweepSurvivesIt(t *testing.T) {
	h := newHarness(t)
	first := h.deployedApp(t, h.account, "aaa-first")
	second := h.deployedApp(t, h.account, "bbb-second")

	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}
	// The restore lands after the sweep has read its list. Clearing disabled_at
	// directly is what the restore does to this account's row, without the
	// scaling half racing the sweep's own.
	h.scaler.scaled = map[string]int32{}
	h.scaler.onScale = func(namespace string) {
		if namespace != deploy.NamespaceName(first.Slug) {
			return
		}
		if _, err := h.store.DB().ExecContext(t.Context(),
			`UPDATE accounts SET disabled_at = NULL WHERE id = ?`, h.account.ID); err != nil {
			t.Errorf("restoring mid sweep: %v", err)
		}
	}

	h.svc.SweepSuspended(t.Context())

	if _, swept := h.scaler.scaled[deploy.NamespaceName(second.Slug)]; swept {
		t.Error("the sweep scaled an app down after its account was restored, so it trusted its snapshot")
	}
}

// countRows is how many rows a table holds, read straight out of the database so
// nothing about the Go layer can flatter the answer.
func (h *harness) countRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// TestASuspensionNeverEndsOnItsOwn is AC-21. There is no expiry, no timer, and no
// path back except an admin restoring it, so the sweep running many times over
// leaves the account exactly as suspended as it started. covers: AC-21
func TestASuspensionNeverEndsOnItsOwn(t *testing.T) {
	h := newHarness(t)
	h.deployedApp(t, h.account, "site")
	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}

	for range 5 {
		h.svc.SweepSuspended(t.Context())
	}

	if !h.suspended(t, h.account.ID) {
		t.Error("the account came back on its own, so something other than a restore clears a suspension")
	}
	if got := h.scaler.scaled[deploy.NamespaceName("site")]; got != 0 {
		t.Errorf("its app is at %d replicas after five sweeps, want 0", got)
	}
}

// TestASuspendedAccountsAppStillDeletes is AC-23. Suspension gates callers, never
// the platform's own cleanup: leaving a delete blocked behind a suspension is how
// a suspended namespace outlives its app. covers: AC-23
func TestASuspendedAccountsAppStillDeletes(t *testing.T) {
	h := newHarness(t)
	app := h.deployedApp(t, h.account, "doomed")
	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}

	if err := h.store.SoftDeleteApp(t.Context(), app.ID); err != nil {
		t.Fatalf("deleting a suspended account's app: %v", err)
	}

	// And it drops straight out of everything this feature reads, so the sweep
	// stops asking the cluster about a namespace that is being torn down.
	h.scaler.scaled = map[string]int32{}
	h.svc.SweepSuspended(t.Context())
	if _, swept := h.scaler.scaled[deploy.NamespaceName(app.Slug)]; swept {
		t.Error("the sweep is still holding a deleted app at zero")
	}
}

// TestASuspendedAppKeepsEverythingElse is AC-22 stated where it can be: the only
// cluster write a suspension makes is the replica count, so the Ingress, Service,
// Secret, policies, namespace and slug are all untouched by construction. The
// scaler refuses any call that names something other than the app workload.
// covers: AC-22
func TestASuspendedAppKeepsEverythingElse(t *testing.T) {
	h := newHarness(t)
	app := h.deployedApp(t, h.account, "kept")

	if _, err := h.svc.Suspend(t.Context(), adminID, h.account.ID); err != nil {
		t.Fatalf("suspending: %v", err)
	}

	if h.scaler.calls != 1 {
		t.Errorf("a suspension made %d cluster calls for one app, want exactly 1", h.scaler.calls)
	}
	// The slug is never freed: the app row survives, so nobody else can take the
	// hostname while its owner is stopped.
	live, err := h.store.GetAppBySlug(t.Context(), app.Slug)
	if err != nil {
		t.Fatalf("the slug is no longer held: %v", err)
	}
	if live.DeletedAt != nil {
		t.Error("a suspension soft deleted the app, which would free its hostname")
	}
}
