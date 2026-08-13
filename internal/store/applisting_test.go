package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// TestTheAppListingReadsServingAndTheLastDeployIndependently is the case the
// query exists for, proven against real SQLite: an app serving release 1 whose
// newest deployment failed reports both, and neither is derived from the other.
func TestTheAppListingReadsServingAndTheLastDeployIndependently(t *testing.T) {
	// covers: AC-3, AC-4, AC-5, AC-6, AC-8
	t.Parallel()
	ctx := t.Context()
	s, clock := newStore(t)
	f := newFixture(t, s)

	_, release := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")
	// A second deploy that fails, which leaves the app serving the first one.
	clock.Advance(time.Minute)
	failed := mustCreateDeployment(t, s, f, newUpload(t, s, f.account.ID, "hash-2").ID)
	mustTransition(t, s, failed.ID, domain.StateBuilding)
	if _, err := s.Transition(ctx, failed.ID, domain.StateFailed, string(domain.ReasonBuildFailed), "the build did not complete"); err != nil {
		t.Fatalf("failing the second deployment: %v", err)
	}

	rows, err := s.ListAppSummaries(ctx, f.account.ID, 50)
	if err != nil {
		t.Fatalf("listing app summaries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the listing holds %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.ServingRelease != release.ReleaseNumber {
		t.Errorf("serving release = %d, want %d: a failed deploy does not move the pointer", got.ServingRelease, release.ReleaseNumber)
	}
	if got.LastDeploymentID != failed.ID {
		t.Errorf("last deployment = %s, want the newest one %s", got.LastDeploymentID, failed.ID)
	}
	if got.LastDeploymentState != string(domain.StateFailed) {
		t.Errorf("last deployment state = %s, want failed", got.LastDeploymentState)
	}
	if got.LastDeploymentReason != string(domain.ReasonBuildFailed) {
		t.Errorf("last deployment reason = %s, want build_failed", got.LastDeploymentReason)
	}
	if got.LastDeployedAt == "" {
		t.Error("last deployed at is empty, want the newest finish")
	}
}

// TestAnAppThatHasNeverRunListsWithNeitherFact pins the two absent cases as zero
// values rather than as a missing row.
func TestAnAppThatHasNeverRunListsWithNeitherFact(t *testing.T) {
	// covers: AC-3, AC-4, AC-6
	t.Parallel()
	s, _ := newStore(t)
	f := newFixture(t, s)

	rows, err := s.ListAppSummaries(t.Context(), f.account.ID, 50)
	if err != nil {
		t.Fatalf("listing app summaries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the listing holds %d rows, want the registered app", len(rows))
	}
	got := rows[0]
	if got.ServingRelease != 0 || got.LastDeploymentID != "" || got.LastDeployedAt != "" {
		t.Errorf("row = %+v, want no serving release, no deployment, and no finish", got)
	}
	if got.Slug != f.app.Slug || got.Name != f.app.Name {
		t.Errorf("row = %+v, want the app's own name and slug", got)
	}
}

// TestADeletedAppNeverAppearsInTheListing pins the soft delete filter, which is
// the one thing standing between a deleted app and a caller.
func TestADeletedAppNeverAppearsInTheListing(t *testing.T) {
	// covers: AC-9
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SoftDeleteApp(ctx, f.app.ID); err != nil {
		t.Fatalf("deleting the app: %v", err)
	}

	rows, err := s.ListAppSummaries(ctx, f.account.ID, 50)
	if err != nil {
		t.Fatalf("listing app summaries: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("the listing holds %+v, want a deleted app to be invisible", rows)
	}
	// The slug stays reserved forever, so its hostname is never handed out again
	// (AC-22).
	taken, err := s.SlugTaken(ctx, f.app.Slug)
	if err != nil {
		t.Fatalf("checking the slug: %v", err)
	}
	if !taken {
		t.Error("the slug came free after a delete, want it reserved for good")
	}
}

// TestTheAppListingCarriesNoConfiguration pins the projection rather than the
// handler above it: the listing's query never reads app_config or a release's
// config_snapshot, so no key and no value enters the process at all.
func TestTheAppListingCarriesNoConfiguration(t *testing.T) {
	// covers: AC-7
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	if err := s.SetConfig(ctx, f.app.ID, "PLAIN_KEY", "plain-value", false); err != nil {
		t.Fatalf("setting the plain key: %v", err)
	}
	if err := s.SetConfig(ctx, f.app.ID, "SECRET_KEY", "secret-value", true); err != nil {
		t.Fatalf("setting the secret key: %v", err)
	}

	rows, err := s.ListAppSummaries(ctx, f.account.ID, 50)
	if err != nil {
		t.Fatalf("listing app summaries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the listing holds %d rows, want 1", len(rows))
	}
	// Every field of the row at once, so a configuration field added later fails
	// here rather than leaking.
	whole := fmt.Sprintf("%+v", rows[0])
	for _, forbidden := range []string{"PLAIN_KEY", "plain-value", "SECRET_KEY", "secret-value"} {
		if strings.Contains(whole, forbidden) {
			t.Errorf("the listing row carries %q: %s", forbidden, whole)
		}
	}
}

// TestADeletedAppsNameIsFreeButItsSlugIsNot pins both halves of AC-22 together,
// because they pull opposite ways: the name has to come back so the account can
// reuse it, and the slug must not, so the old hostname stays dead.
func TestADeletedAppsNameIsFreeButItsSlugIsNot(t *testing.T) {
	// covers: AC-22
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SoftDeleteApp(ctx, f.app.ID); err != nil {
		t.Fatalf("deleting the app: %v", err)
	}
	again, err := s.CreateApp(ctx, f.account.ID, f.app.Name)
	if err != nil {
		t.Fatalf("creating an app under the deleted app's name: %v", err)
	}
	if again.ID == f.app.ID {
		t.Fatal("the deleted app came back, want a new app")
	}
	if again.Slug == f.app.Slug {
		t.Errorf("the new app took the slug %q back, want a new one so the old hostname stays dead", again.Slug)
	}
}

// TestTheListingIsScopedToOneAccount pins that the query never crosses accounts,
// which is what the tool's whole access model rests on.
func TestTheListingIsScopedToOneAccount(t *testing.T) {
	// covers: AC-10
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	other, err := s.CreateAccount(ctx, "someone-else")
	if err != nil {
		t.Fatalf("creating the second account: %v", err)
	}
	rows, err := s.ListAppSummaries(ctx, other.ID, 50)
	if err != nil {
		t.Fatalf("listing the second account's apps: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("the second account saw %+v, want none of the first account's apps (%s)", rows, f.app.Slug)
	}
}

// TestTheListingHonoursTheLimitItIsGiven pins the bound at the query rather than
// as a slice taken afterwards.
func TestTheListingHonoursTheLimitItIsGiven(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	for _, name := range []string{"second", "third"} {
		if _, err := s.CreateApp(ctx, f.account.ID, name); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
	}

	rows, err := s.ListAppSummaries(ctx, f.account.ID, 2)
	if err != nil {
		t.Fatalf("listing app summaries: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("the listing holds %d rows, want the limit of 2", len(rows))
	}
}

// TestLiveAppSlugsSkipsDeletedApps is the read the reaper deletes namespaces
// against, so what it leaves out is the dangerous half.
func TestLiveAppSlugsSkipsDeletedApps(t *testing.T) {
	// covers: AC-24
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	kept, err := s.CreateApp(ctx, f.account.ID, "kept")
	if err != nil {
		t.Fatalf("creating the second app: %v", err)
	}
	if err := s.SoftDeleteApp(ctx, f.app.ID); err != nil {
		t.Fatalf("deleting the first app: %v", err)
	}

	slugs, err := s.LiveAppSlugs(ctx)
	if err != nil {
		t.Fatalf("reading live app slugs: %v", err)
	}
	if len(slugs) != 1 || slugs[0] != kept.Slug {
		t.Errorf("live slugs = %v, want only %s", slugs, kept.Slug)
	}
}

// TestADeleteIsRefusedWhileADeploymentIsInFlight pins the store's own decision,
// which is what the tool's deployment_in_flight refusal rests on.
func TestADeleteIsRefusedWhileADeploymentIsInFlight(t *testing.T) {
	// covers: AC-15, AC-21
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	dep := mustCreateDeployment(t, s, f, f.upload.ID)

	err := s.SoftDeleteApp(ctx, f.app.ID)
	if !errors.Is(err, store.ErrDeploymentInFlight) {
		t.Fatalf("deleting an app mid deploy = %v, want ErrDeploymentInFlight", err)
	}
	app, err := s.GetApp(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading the app back: %v", err)
	}
	if app.ID != f.app.ID {
		t.Error("the app row is gone, want the refusal to have written nothing")
	}
	// The deployment rows themselves are untouched by any of this (AC-21).
	if _, err := s.GetDeployment(ctx, dep.ID); err != nil {
		t.Errorf("reading the deployment back: %v", err)
	}
}
