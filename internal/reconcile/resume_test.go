package reconcile_test

import (
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/reconcile"
)

// TestAResumedDeploymentGetsWhatIsLeftOfItsBudget checks the half of the budget
// rule that a spent budget cannot show: a deployment claimed with most of its
// budget already gone is driven on the remainder, not on a fresh window. This is
// what stops a control plane restart handing every in flight deployment a new
// full budget. Verifies spec 0005, AC-14a.
func TestAResumedDeploymentGetsWhatIsLeftOfItsBudget(t *testing.T) {
	// covers: AC-14a
	ctx := t.Context()
	w := setup(t)
	// A build that never reports finished, so the budget is the only thing that
	// can end this deployment, and the build's own timeout is far away.
	w.buildNeverEnds()

	const budget = 4 * time.Second
	const spentAlready = 3 * time.Second
	dep := toLoop(w.deployment)
	dep.CreatedAt = time.Now().Add(-spentAlready)

	start := time.Now()
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, func(o *reconcile.Options) {
		o.DeployTimeout = budget
		o.BuildTimeout = time.Hour
	}).Drive(ctx, dep)
	elapsed := time.Since(start)

	w.failedWith(t, domain.ReasonTimeout)
	// A fresh budget would have run the whole four seconds. The remainder is one,
	// and the check is loose enough that a slow machine does not fail it.
	if elapsed >= budget {
		t.Errorf("the drive took %s of a %s budget, want only the %s left after its age",
			elapsed, budget, budget-spentAlready)
	}
}
