package reconcile_test

import (
	"errors"
	"testing"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
	batchv1 "k8s.io/api/batch/v1"
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
