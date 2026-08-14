package store_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// stranded is one claimed deployment left sitting in building, which is what a
// drive that died without writing the row terminal leaves behind.
func stranded(t *testing.T, s *store.Store, f fixture) store.Deployment {
	t.Helper()
	dep := mustCreateDeployment(t, s, f, f.upload.ID)
	if _, err := s.ClaimNext(t.Context(), "deployer-0"); err != nil {
		t.Fatalf("claiming the deployment: %v", err)
	}
	return mustTransition(t, s, dep.ID, domain.StateBuilding)
}

func TestReleasingAClaimLetsTheLoopAdoptTheRow(t *testing.T) {
	// covers: spec 0014 AC-3, AC-5
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	dep := stranded(t, s, f)

	ok, err := s.ReleaseBuildingClaim(ctx, dep.ID)
	if err != nil {
		t.Fatalf("releasing the claim: %v", err)
	}
	if !ok {
		t.Error("the release reported that it changed nothing, on a row it did release")
	}

	released, err := s.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	// claimed_at is the one that matters: it is what the claim query tests, so a
	// test asserting only claimed_by would pass against a broken release.
	if released.ClaimedAt != nil {
		t.Errorf("claimed_at = %v, want null", *released.ClaimedAt)
	}
	if released.ClaimedBy != nil {
		t.Errorf("claimed_by = %v, want null", *released.ClaimedBy)
	}
	if released.State != string(domain.StateBuilding) {
		t.Errorf("state = %s, want building: releasing a claim never moves the row", released.State)
	}

	adopted, err := s.ClaimNext(ctx, "deployer-0")
	if err != nil {
		t.Fatalf("adopting the released deployment: %v", err)
	}
	if adopted.ID != dep.ID {
		t.Errorf("adopted %s, want the released row %s", adopted.ID, dep.ID)
	}
}

func TestReleasingAClaimOnARowSomethingElseEndedWritesNothing(t *testing.T) {
	// covers: spec 0014 AC-5a, AC-10
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	dep := stranded(t, s, f)
	// The supersession that lands between the cluster read and the release.
	mustTransition(t, s, dep.ID, domain.StateCancelled)

	ok, err := s.ReleaseBuildingClaim(ctx, dep.ID)
	if err != nil {
		t.Fatalf("releasing the claim: %v", err)
	}
	// Reported rather than swallowed, so the loop can log the race apart from a
	// real release instead of calling both a success (AC-10).
	if ok {
		t.Error("the release reported success on a row its guard could not have matched")
	}

	after, err := s.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if after.State != string(domain.StateCancelled) {
		t.Errorf("state = %s, want cancelled", after.State)
	}
	if after.ClaimedAt == nil {
		t.Error("claimed_at was cleared on a row that is no longer building")
	}
}

func TestQueuedWorkIsClaimedAheadOfAnOlderStrandedRow(t *testing.T) {
	// covers: spec 0014 AC-5
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	// The stray comes first, so its id sorts ahead of the queued row's: only the
	// state preference can put the queued one in front.
	stray := stranded(t, s, f)
	if _, err := s.ReleaseBuildingClaim(ctx, stray.ID); err != nil {
		t.Fatalf("releasing the claim: %v", err)
	}
	// A second app, because one deployment in flight per app is a schema rule.
	other, err := s.CreateApp(ctx, f.account.ID, "Billing Service")
	if err != nil {
		t.Fatalf("creating the second app: %v", err)
	}
	up := newUpload(t, s, f.account.ID, "hash-2")
	fresh := mustCreateDeployment(t, s, fixture{account: f.account, app: other, upload: up}, up.ID)

	claimed, err := s.ClaimNext(ctx, "deployer-0")
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if claimed.ID != fresh.ID {
		t.Fatalf("claimed %s, want the queued row %s: recovery must never overtake fresh work", claimed.ID, fresh.ID)
	}

	// Only once the queue is empty does the stray get picked up.
	next, err := s.ClaimNext(ctx, "deployer-0")
	if err != nil {
		t.Fatalf("claiming the stray: %v", err)
	}
	if next.ID != stray.ID {
		t.Errorf("claimed %s, want the stray %s", next.ID, stray.ID)
	}
}
