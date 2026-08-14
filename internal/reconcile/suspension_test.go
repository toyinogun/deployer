package reconcile_test

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
)

// suspendOwner stamps disabled_at on the account that owns this world's app,
// which is the whole of what a suspension is to the drive.
func (w *world) suspendOwner(t *testing.T) {
	t.Helper()
	if _, err := w.store.DB().ExecContext(t.Context(),
		`UPDATE accounts SET disabled_at = ?`, ids.Stamp(time.Now().UTC())); err != nil {
		t.Fatalf("suspending the owning account: %v", err)
	}
}

// TestASuspensionLandingMidBuildEndsTheDeployment is AC-14, and specifically the
// half a single read at drive entry would miss.
//
// The account is active when the drive starts and the build is accepted. The
// suspension lands while the drive is blocked waiting for that build, which is
// where a real one spends minutes. The check has to run again at the next phase
// boundary or the app comes up under a suspended account. covers: AC-14
func TestASuspensionLandingMidBuildEndsTheDeployment(t *testing.T) {
	w := setup(t)
	w.appComesUp()
	w.buildEnds(batchv1.JobComplete)
	// The account is suspended at the moment the drive asks about the build,
	// which is inside awaitBuild's wait rather than before it. Prepended after
	// buildEnds so it runs first and falls through to it.
	w.clientset.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		w.suspendOwner(t)
		return false, nil, nil
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonAccountSuspended))

	// The drive got past its first phase boundary before the suspension stopped
	// it, which is the whole point: a check that only ran at entry would have let
	// this deployment through, because the account was active when it started.
	events, err := w.store.ListDeploymentEvents(t.Context(), w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	var reachedBuilding bool
	for _, e := range events {
		if e.ToState == string(domain.StateBuilding) {
			reachedBuilding = true
		}
	}
	if !reachedBuilding {
		t.Fatalf("the drive never reached building, so this proves nothing about a later boundary: %+v", events)
	}

	// Nothing it built was promoted: no release was minted, and the app is still
	// pointing at whatever it was pointing at before, which here is nothing.
	if _, err := w.store.GetReleaseByDeployment(t.Context(), w.deployment.ID); err == nil {
		t.Error("a suspended deployment minted a release, so something it built was promoted")
	}
	app, err := w.store.GetApp(t.Context(), w.app.ID)
	if err != nil {
		t.Fatalf("reading the app back: %v", err)
	}
	if app.CurrentReleaseID != nil {
		t.Errorf("the app now points at release %s, which a suspended deploy must never set", *app.CurrentReleaseID)
	}

	// The build Job goes with the deployment, the same way a spent budget takes
	// it, so nothing is left running on the cluster for a stopped account.
	jobs, err := w.clientset.BatchV1().Jobs("deployer-builds").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing build jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("%d build job(s) survived the suspension", len(jobs.Items))
	}
}

// TestASuspensionBeforeTheDriveStartsEndsItToo is the plain case: the account was
// already suspended when the row was claimed, so the very first phase boundary
// refuses it and no build is ever created. covers: AC-14
func TestASuspensionBeforeTheDriveStartsEndsItToo(t *testing.T) {
	w := setup(t)
	w.suspendOwner(t)

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonAccountSuspended))
	jobs, err := w.clientset.BatchV1().Jobs("deployer-builds").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing build jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("a suspended account's deploy created %d build job(s)", len(jobs.Items))
	}
}

// TestAnActiveAccountStillDeploys is the control: the phase boundary check reads
// a real column, so a drive that reads it and finds nothing must be unaffected.
// covers: AC-14
func TestAnActiveAccountStillDeploys(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), toLoop(w.deployment))

	dep, err := w.store.GetDeployment(t.Context(), w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("an active account's deploy ended %s (%v), want healthy", dep.State, dep.FailureReason)
	}
}
