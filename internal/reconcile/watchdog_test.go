package reconcile_test

import (
	"os"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/store"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// spent is a deploy budget already gone by the time anything is read, which is
// the watchdog's whole condition: created_at plus the budget is in the past.
func spent(o *reconcile.Options) { o.DeployTimeout = time.Nanosecond }

// jobsIn lists the build Jobs that exist, listed rather than fetched because a
// get is what the build stubs intercept.
func (w *world) jobsIn(t *testing.T) []batchv1.Job {
	t.Helper()
	list, err := w.clientset.BatchV1().Jobs("deployer-builds").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing build jobs: %v", err)
	}
	return list.Items
}

// failedWith reads the deployment back and insists it ended on that code.
func (w *world) failedWith(t *testing.T, reason domain.Reason) {
	t.Helper()
	dep, err := w.store.GetDeployment(t.Context(), w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateFailed) {
		t.Fatalf("state = %s, want failed", dep.State)
	}
	if dep.FailureReason == nil || *dep.FailureReason != string(reason) {
		t.Errorf("reason = %v, want %s", dep.FailureReason, reason)
	}
}

func TestADeploymentPastItsBudgetIsFailedWithoutBeingDriven(t *testing.T) {
	// covers: AC-14, AC-14a, AC-18
	w := setup(t)
	dep := toLoop(w.deployment)

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, spent).Drive(t.Context(), dep)

	w.failedWith(t, domain.ReasonTimeout)
	// Never driven, so no build was started for it (AC-14a).
	if jobs := w.jobsIn(t); len(jobs) != 0 {
		t.Errorf("build jobs = %d, want none", len(jobs))
	}
	// Terminal, so the tarball goes with it (AC-18).
	if _, err := os.Stat(w.uploadPath); !os.IsNotExist(err) {
		t.Errorf("the tarball is still at %s", w.uploadPath)
	}
}

func TestTheSweepFailsAnOverdueDeploymentAndDeletesItsBuild(t *testing.T) {
	// covers: AC-14, AC-15, AC-18
	ctx := t.Context()
	w := setup(t)

	// A deployment that got as far as a build, the way a restart would find one.
	jobName := build.JobName(w.deployment.ID)
	if _, err := w.clientset.BatchV1().Jobs("deployer-builds").Create(ctx,
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "deployer-builds"}},
		metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the build job: %v", err)
	}
	if _, err := w.store.Transition(ctx, w.deployment.ID, domain.StateBuilding, "", ""); err != nil {
		t.Fatalf("moving the deployment to building: %v", err)
	}
	if err := w.store.RecordBuildResult(ctx, w.deployment.ID,
		store.BuildResult{BuildJobName: jobName}); err != nil {
		t.Fatalf("recording the build job: %v", err)
	}

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, spent).Sweep(ctx)

	w.failedWith(t, domain.ReasonTimeout)
	if jobs := w.jobsIn(t); len(jobs) != 0 {
		t.Errorf("build jobs = %d, want the overdue one deleted", len(jobs))
	}
	if _, err := os.Stat(w.uploadPath); !os.IsNotExist(err) {
		t.Errorf("the tarball is still at %s", w.uploadPath)
	}
}

func TestAJobThatIsAlreadyGoneDoesNotStopTheFailure(t *testing.T) {
	// covers: AC-15
	ctx := t.Context()
	w := setup(t)

	// The row names a Job the cluster does not have, which is what a crash
	// between the delete and the transition leaves behind.
	if _, err := w.store.Transition(ctx, w.deployment.ID, domain.StateBuilding, "", ""); err != nil {
		t.Fatalf("moving the deployment to building: %v", err)
	}
	if err := w.store.RecordBuildResult(ctx, w.deployment.ID,
		store.BuildResult{BuildJobName: build.JobName(w.deployment.ID)}); err != nil {
		t.Fatalf("recording the build job: %v", err)
	}

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, spent).Sweep(ctx)

	w.failedWith(t, domain.ReasonTimeout)
}

// queuedDeploymentFor adds a second app with its own uploaded tarball and one
// queued deployment, which is what a second agent deploying at the same time
// leaves behind.
func (w *world) queuedDeploymentFor(t *testing.T, name string) store.Deployment {
	t.Helper()
	ctx := t.Context()

	app, err := w.store.CreateApp(ctx, w.app.AccountID, name, 100)
	if err != nil {
		t.Fatalf("creating the app %s: %v", name, err)
	}
	up, err := w.uploads.Accept(ctx, w.app.AccountID,
		tarball(t, "main.go"))
	if err != nil {
		t.Fatalf("accepting the upload for %s: %v", name, err)
	}
	dep, _, err := w.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: app.ID, AccountID: w.app.AccountID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("creating the deployment for %s: %v", name, err)
	}
	return dep
}

// stateOf reads one deployment's state and failure reason back.
func (w *world) stateOf(t *testing.T, id string) (string, string) {
	t.Helper()
	dep, err := w.store.GetDeployment(t.Context(), id)
	if err != nil {
		t.Fatalf("reading deployment %s back: %v", id, err)
	}
	var reason string
	if dep.FailureReason != nil {
		reason = *dep.FailureReason
	}
	return dep.State, reason
}

func TestAQueuedDeploymentWaitsOutALongDriveAndIsThenSwept(t *testing.T) {
	// covers: AC-14, AC-14a, AC-16
	//
	// The scenario that proves the two enforcement points are not redundant: a
	// drive holds the loop's single goroutine for the length of its own budget,
	// so the sweep cannot reach a second app's queued row while it runs, and the
	// phase boundary check cannot reach that row at all because nothing is
	// driving it. Each one covers what the other cannot.
	ctx := t.Context()
	w := setup(t)
	w.buildNeverEnds()
	// The queued row is the older one, so its budget is provably spent by the
	// time the younger row's drive reaches the end of its own. Creating it second
	// would leave the gap between the two creations as a race.
	queued := w.deployment
	driven := w.queuedDeploymentFor(t, "second")

	budget := func(o *reconcile.Options) {
		o.DeployTimeout = 50 * time.Millisecond
		o.BuildTimeout = time.Hour
	}
	r := w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, budget)

	// The long drive, which ends only at its own budget.
	r.Drive(ctx, toLoop(driven))
	if state, reason := w.stateOf(t, driven.ID); state != string(domain.StateFailed) || reason != string(domain.ReasonTimeout) {
		t.Fatalf("the driven deployment is %s/%s, want failed/%s", state, reason, domain.ReasonTimeout)
	}

	// The queued row spent that whole time past its own budget and was left
	// alone, because the one goroutine was busy with the drive.
	if state, _ := w.stateOf(t, queued.ID); state != string(domain.StateQueued) {
		t.Errorf("the queued deployment is %s, want queued: nothing should reach it mid drive", state)
	}
	// It is the next tick's sweep, not the drive, that ends it.
	r.Sweep(ctx)

	state, reason := w.stateOf(t, queued.ID)
	if state != string(domain.StateFailed) || reason != string(domain.ReasonTimeout) {
		t.Errorf("the queued deployment is %s/%s, want failed/%s", state, reason, domain.ReasonTimeout)
	}
	// Never driven, so the sweep started no build for it (AC-14a).
	if jobs := w.jobsIn(t); len(jobs) != 0 {
		t.Errorf("build jobs = %d, want none left", len(jobs))
	}
}

func TestTheDrivenDeploymentIsFailedAtItsOwnBudget(t *testing.T) {
	// covers: AC-14, AC-15, AC-16
	ctx := t.Context()
	w := setup(t)
	// A build that never reports finished, so the only thing that can end this
	// deployment is the budget.
	w.buildNeverEnds()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, func(o *reconcile.Options) {
		o.DeployTimeout = 50 * time.Millisecond
		o.BuildTimeout = time.Hour
	}).Drive(ctx, toLoop(w.deployment))

	w.failedWith(t, domain.ReasonTimeout)
	// The build goes with the deployment, rather than running on against a row
	// nothing will read (AC-15).
	if jobs := w.jobsIn(t); len(jobs) != 0 {
		t.Errorf("build jobs = %d, want the abandoned one deleted", len(jobs))
	}
	// Nothing was written after the failure: failed is terminal (AC-16).
	events, err := w.store.ListDeploymentEvents(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	if last := events[len(events)-1]; last.ToState != string(domain.StateFailed) {
		t.Errorf("the last event is %s, want failed", last.ToState)
	}
}
