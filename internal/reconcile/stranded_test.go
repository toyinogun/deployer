package reconcile_test

import (
	"context"
	"errors"
	"testing"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/store"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// strand leaves this world's deployment exactly as a drive that died mid build
// leaves it: claimed, sitting in building, carrying the name of a Job nothing is
// waiting on any more.
func (w *world) strand(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	if _, err := w.store.ClaimNext(ctx, "deployer-0"); err != nil {
		t.Fatalf("claiming the deployment: %v", err)
	}
	if _, err := w.store.Transition(ctx, w.deployment.ID, domain.StateBuilding, "", ""); err != nil {
		t.Fatalf("moving the deployment to building: %v", err)
	}
	jobName := build.JobName(w.deployment.ID)
	if err := w.store.RecordBuildResult(ctx, w.deployment.ID,
		store.BuildResult{BuildJobName: jobName}); err != nil {
		t.Fatalf("recording the build job: %v", err)
	}
	return jobName
}

// jobReadFails makes every Job read return an error, which is a Kubernetes API
// blip rather than an answer about the build.
func (w *world) jobReadFails() {
	w.clientset.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the api server is having a moment")
	})
}

// reload reads this world's deployment back.
func (w *world) reload(t *testing.T) store.Deployment {
	t.Helper()
	dep, err := w.store.GetDeployment(t.Context(), w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	return dep
}

// stillBuilding insists the row was left exactly as it stood: in building, still
// claimed, and unfailed.
func (w *world) stillBuilding(t *testing.T) {
	t.Helper()
	dep := w.reload(t)
	if dep.State != string(domain.StateBuilding) {
		t.Fatalf("state = %s, want building (failure reason %v)", dep.State, dep.FailureReason)
	}
	if dep.ClaimedAt == nil {
		t.Error("the claim was cleared on a row with no evidence that its drive died")
	}
}

func TestAStrandedRowWhoseBuildFailedIsEndedWithTheReasonTheJobGave(t *testing.T) {
	// covers: AC-1, AC-2
	w := setup(t)
	w.strand(t)
	w.buildEnds(batchv1.JobFailed)

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Tick(t.Context())

	w.failedWith(t, domain.ReasonBuildFailed)
}

func TestAStrandedRowWhoseBuildIsGoneIsEndedAsBuildFailed(t *testing.T) {
	// covers: AC-2
	w := setup(t)
	// The Job name is recorded but no Job was ever created in the fake cluster,
	// which is what a Job that has been collected reads back as.
	w.strand(t)

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Tick(t.Context())

	w.failedWith(t, domain.ReasonBuildFailed)
}

func TestAStrandedRowWhoseBuildSucceededIsResumedRatherThanFailed(t *testing.T) {
	// covers: AC-3, AC-5
	w := setup(t)
	w.strand(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	ctx := t.Context()

	// One tick hands the row back and adopts it in the same pass, because the
	// claim is cleared before ClaimNext runs.
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Tick(ctx)

	dep := w.reload(t)
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("state = %s, want healthy (failure reason %v): a build that succeeded is work worth finishing",
			dep.State, dep.FailureReason)
	}
}

func TestAJobStateThatCannotBeReadLeavesTheRowAlone(t *testing.T) {
	// covers: AC-4
	w := setup(t)
	w.strand(t)
	w.jobReadFails()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Tick(t.Context())

	w.stillBuilding(t)
}

func TestARowWhoseBuildIsStillRunningIsLeftAlone(t *testing.T) {
	// covers: AC-4a
	w := setup(t)
	w.strand(t)
	w.buildNeverEnds()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Tick(t.Context())

	w.stillBuilding(t)
}

func TestAStrandedAndOverdueRowIsEndedWithTheTrueReasonNotTimeout(t *testing.T) {
	// covers: AC-6
	w := setup(t)
	w.strand(t)
	w.buildEnds(batchv1.JobFailed)

	// Both conditions at once: the recovery check runs first, so the reason the
	// cluster gave wins over the watchdog's.
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, spent).Tick(t.Context())

	w.failedWith(t, domain.ReasonBuildFailed)
}

func TestARowThatKeepsStrandingIsStillEndedByTheDeployBudget(t *testing.T) {
	// covers: AC-9
	w := setup(t)
	w.strand(t)
	w.buildEnds(batchv1.JobComplete)
	// The check hands the row back rather than failing it, and the budget pass in
	// the same tick then ends it from created_at. No adoption counter anywhere:
	// a row that keeps stranding is bounded by the budget it always had.
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, spent).Tick(t.Context())

	w.failedWith(t, domain.ReasonTimeout)
}

// failOnce is the real store with one named call failing the first time it is
// made. Everything else, including every other call of that same method, is the
// real SQLite store answering for itself.
//
// This is not mocking the store, which the project rule forbids, and the
// distinction matters because the next reader will check it against that rule:
// no store semantics are invented here. A passthrough returning a real error on
// one call fakes no behaviour, and it is the only way to reach the two faults
// spec 0014 exists for, both of which are internal to the platform. Forcing a
// genuine SQLite failure does not work: Tick reads ListNonTerminal before the
// check and skips the check when that read fails, so a broken connection would
// fail the read the test needs to succeed.
type failOnce struct {
	store.ReconcileStore
	// transitionTo is the state whose write fails, once. The drive writes several
	// transitions, and only the one ending the row is the fault being modelled.
	transitionTo domain.State
	// listNonTerminal fails the next listing, once, which is the startup sweep
	// reading nothing and leaving every in flight row unattended.
	listNonTerminal bool
	// releaseClaim fails the next release, once: the check decides to hand a row
	// back and loses that write.
	releaseClaim bool
	// releaseCalls counts every release attempt. The loop runs on one goroutine,
	// so a plain int is enough, and it is what makes "not retried inside the
	// tick" an assertion rather than an inference from the row's state.
	releaseCalls int
}

func (f *failOnce) Transition(ctx context.Context, id string, to domain.State, reason, detail string) error {
	if to == f.transitionTo {
		f.transitionTo = ""
		return errors.New("the database was busy")
	}
	return f.ReconcileStore.Transition(ctx, id, to, reason, detail)
}

func (f *failOnce) ListNonTerminal(ctx context.Context) ([]reconcile.Deployment, error) {
	if f.listNonTerminal {
		f.listNonTerminal = false
		return nil, errors.New("the database was busy")
	}
	return f.ReconcileStore.ListNonTerminal(ctx)
}

func (f *failOnce) ReleaseClaim(ctx context.Context, id string) (bool, error) {
	f.releaseCalls++
	if f.releaseClaim {
		f.releaseClaim = false
		return false, errors.New("the database was busy")
	}
	return f.ReconcileStore.ReleaseClaim(ctx, id)
}

func TestARowStrandedByAFailedStateWriteIsEndedOnTheNextTick(t *testing.T) {
	// covers: AC-1, AC-2
	// The core fault: the build fails, the reason is computed, and the write that
	// would have recorded it does not land. Nothing else notices, so without this
	// check the row sits in building until the deploy budget and is then recorded
	// as timeout rather than as the build failure it was.
	w := setup(t)
	w.deployments = &failOnce{
		ReconcileStore: store.ForReconcile(w.store),
		transitionTo:   domain.StateFailed,
	}
	w.buildEnds(batchv1.JobFailed)
	ctx := t.Context()
	r := w.reconciler(fakeRegistry{digest: testDigest, user: "1000"})

	// The drive that dies: it builds, learns the build failed, and loses the write.
	r.Tick(ctx)
	if dep := w.reload(t); dep.State != string(domain.StateBuilding) {
		t.Fatalf("state = %s, want building: the fault this test rests on did not happen", dep.State)
	}

	// The next tick asks the cluster what the Job did and ends the row on that.
	r.Tick(ctx)

	w.failedWith(t, domain.ReasonBuildFailed)
}

func TestRowsLeftUnattendedByAFailedStartupSweepAreRecoveredByTheTick(t *testing.T) {
	// covers: AC-1
	// The second fault, and the one where this check is the only recovery there
	// is: Sweep's listing errors, it logs one warning and returns, and the ticker
	// starts with every in flight row unattended.
	w := setup(t)
	w.deployments = &failOnce{
		ReconcileStore:  store.ForReconcile(w.store),
		listNonTerminal: true,
	}
	w.strand(t)
	w.buildEnds(batchv1.JobFailed)
	ctx := t.Context()
	r := w.reconciler(fakeRegistry{digest: testDigest, user: "1000"})

	r.Sweep(ctx)
	if dep := w.reload(t); dep.State != string(domain.StateBuilding) {
		t.Fatalf("state = %s, want building: the sweep was supposed to have failed", dep.State)
	}

	r.Tick(ctx)

	w.failedWith(t, domain.ReasonBuildFailed)
}

func TestAReleaseThatFailsChangesNothingAndIsNotRetriedInTheSameTick(t *testing.T) {
	// covers: AC-5b
	// The check decides to hand a succeeded build back and the write does not
	// land. That is the check recursing into the exact fault it exists for, so
	// the rule is one attempt per tick and no repair inside this one: the row is
	// left exactly as it stood, and the next tick reads the Job again and reaches
	// the same decision. A retry here would be the loop trying to out write a
	// database that just told it no.
	w := setup(t)
	f := &failOnce{
		ReconcileStore: store.ForReconcile(w.store),
		releaseClaim:   true,
	}
	w.deployments = f
	w.strand(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	ctx := t.Context()
	r := w.reconciler(fakeRegistry{digest: testDigest, user: "1000"})

	r.Tick(ctx)

	w.stillBuilding(t)
	if f.releaseCalls != 1 {
		t.Errorf("release attempts in one tick = %d, want 1: a failed release is not retried inside the tick", f.releaseCalls)
	}

	// Self healing rather than lost: the next tick asks the cluster the same
	// question, gets the same answer, and this time the release lands.
	r.Tick(ctx)

	dep := w.reload(t)
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("state = %s, want healthy (failure reason %v): the next tick should reach the same decision and finish the build",
			dep.State, dep.FailureReason)
	}
}

func TestASupersessionThatBeatsTheReleaseLeavesTheRowEnded(t *testing.T) {
	// covers: AC-5a, AC-10
	w := setup(t)
	w.strand(t)
	ctx := t.Context()
	// The Job read answers succeeded and, in the same breath, something else ends
	// the row: exactly the window between the read and the release.
	w.clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if _, err := w.store.Transition(ctx, w.deployment.ID, domain.StateCancelled, "", ""); err != nil {
			t.Errorf("superseding the deployment: %v", err)
		}
		name := action.(k8stesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}},
		}, nil
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Tick(ctx)

	dep := w.reload(t)
	if dep.State != string(domain.StateCancelled) {
		t.Fatalf("state = %s, want cancelled: a row something else ended must never be reopened", dep.State)
	}
	if dep.ClaimedAt == nil {
		t.Error("the claim was cleared on a row the guard could not have matched")
	}
}
