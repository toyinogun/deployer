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
