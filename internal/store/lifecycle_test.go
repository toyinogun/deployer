package store_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

// deployToHealthy runs a build deploy the whole way and returns its release.
func deployToHealthy(t *testing.T, s *store.Store, f fixture, uploadID, digest string) (store.Deployment, store.Release) {
	t.Helper()
	ctx := t.Context()
	dep := mustCreateDeployment(t, s, f, uploadID)
	mustTransition(t, s, dep.ID, domain.StateBuilding)
	mustTransition(t, s, dep.ID, domain.StatePushing)
	if err := s.RecordBuildResult(ctx, dep.ID, store.BuildResult{
		BuildPath: "buildpacks", ImageRepo: "registry.internal/checkout", ImageDigest: digest,
	}); err != nil {
		t.Fatalf("recording the build result: %v", err)
	}
	mustTransition(t, s, dep.ID, domain.StateDeploying)
	healthy, rel, err := s.MarkHealthy(ctx, dep.ID, configForDeploy(t, s, f.app.ID))
	if err != nil {
		t.Fatalf("marking healthy: %v", err)
	}
	return healthy, rel
}

// configForDeploy stands in for the read the reconciler does when it composes
// the workload, which is the config MarkHealthy is then handed.
func configForDeploy(t *testing.T, s *store.Store, appID string) map[string]string {
	t.Helper()
	entries, err := s.ListConfigForDeploy(t.Context(), appID)
	if err != nil {
		t.Fatalf("reading the app's configuration: %v", err)
	}
	values := make(map[string]string, len(entries))
	for _, e := range entries {
		values[e.Key] = e.Value
	}
	return values
}

// TestMarkHealthyIsAllOrNothing forces the release insert to fail and checks
// that no part of the transaction survived. Verifies AC-9.
func TestMarkHealthyIsAllOrNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	dep := mustCreateDeployment(t, s, f, f.upload.ID)
	mustTransition(t, s, dep.ID, domain.StateBuilding)
	mustTransition(t, s, dep.ID, domain.StatePushing)
	if err := s.RecordBuildResult(ctx, dep.ID, store.BuildResult{ImageDigest: "sha256:aaa"}); err != nil {
		t.Fatalf("recording the build result: %v", err)
	}
	mustTransition(t, s, dep.ID, domain.StateDeploying)

	// A release already claiming this deployment makes the insert inside
	// MarkHealthy fail, which is the torn state the transaction has to prevent.
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO releases (id, app_id, deployment_id, release_number, image_digest, config_snapshot, created_at)
		 VALUES ('rel_manual', ?, ?, 1, 'sha256:zzz', '{}', ?)`,
		f.app.ID, dep.ID, ids.Stamp(testStart)); err != nil {
		t.Fatalf("planting the conflicting release: %v", err)
	}

	before, err := s.ListDeploymentEvents(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	if _, _, err := s.MarkHealthy(ctx, dep.ID, nil); !errors.Is(err, store.ErrReleaseExists) {
		t.Fatalf("MarkHealthy returned %v, want ErrReleaseExists", err)
	}

	current, err := s.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the deployment: %v", err)
	}
	if current.State == string(domain.StateHealthy) {
		t.Error("the deployment is healthy but its release was never written")
	}
	after, err := s.ListDeploymentEvents(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("the rolled back transaction left %d event rows behind", len(after)-len(before))
	}
	app, err := s.GetApp(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading the app: %v", err)
	}
	if app.CurrentReleaseID != nil {
		t.Errorf("the app was pointed at %v by a transaction that failed", *app.CurrentReleaseID)
	}
}

// TestMarkHealthyNeedsADigest checks AC-9's guard: no image, no release.
func TestMarkHealthyNeedsADigest(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	f := newFixture(t, s)
	dep := mustCreateDeployment(t, s, f, f.upload.ID)
	mustTransition(t, s, dep.ID, domain.StateBuilding)
	mustTransition(t, s, dep.ID, domain.StatePushing)
	mustTransition(t, s, dep.ID, domain.StateDeploying)

	if _, _, err := s.MarkHealthy(t.Context(), dep.ID, nil); !errors.Is(err, store.ErrNoDigest) {
		t.Fatalf("got %v, want ErrNoDigest", err)
	}
}

// TestRollbackFidelity checks that a rollback re promotes the exact image and
// takes the short path, and that it never rewrites the release it came from.
// Verifies AC-10.
func TestRollbackFidelity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfig(ctx, f.app.ID, "LOG_LEVEL", "info", false); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	_, first := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	// The environment moves on between the good release and the rollback.
	if err := s.SetConfig(ctx, f.app.ID, "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("changing configuration: %v", err)
	}
	second := newUpload(t, s, f.account.ID, "hash-2")
	deployToHealthy(t, s, f, second.ID, "sha256:bbb")

	rollback, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: f.app.ID, AccountID: f.account.ID, SourceReleaseID: &first.ID,
	})
	if err != nil {
		t.Fatalf("creating the rollback: %v", err)
	}
	if rollback.UploadID != nil {
		t.Error("a rollback must not name an upload")
	}
	if rollback.ImageDigest == nil || *rollback.ImageDigest != "sha256:aaa" {
		t.Fatalf("the rollback carries digest %v, want the one from release 1", rollback.ImageDigest)
	}

	// The short path: queued straight to deploying, no build and no push.
	mustTransition(t, s, rollback.ID, domain.StateDeploying)
	_, rel, err := s.MarkHealthy(ctx, rollback.ID, configForDeploy(t, s, f.app.ID))
	if err != nil {
		t.Fatalf("marking the rollback healthy: %v", err)
	}
	if rel.ImageDigest != first.ImageDigest {
		t.Errorf("the rollback release records %q, want %q", rel.ImageDigest, first.ImageDigest)
	}
	if rel.ReleaseNumber != 3 {
		t.Errorf("the rollback is release %d, want 3: numbers are never reused", rel.ReleaseNumber)
	}

	events, err := s.ListDeploymentEvents(ctx, rollback.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("the rollback left %d events, want 3", len(events))
	}
	for _, e := range events {
		if e.ToState == string(domain.StateBuilding) || e.ToState == string(domain.StatePushing) {
			t.Errorf("the rollback entered %q, which it must skip", e.ToState)
		}
	}

	// The old release keeps the environment it actually ran with.
	old, err := s.GetRelease(ctx, first.ID)
	if err != nil {
		t.Fatalf("reading release 1: %v", err)
	}
	if got := snapshotValue(t, old.ConfigSnapshot, "LOG_LEVEL"); got != "info" {
		t.Errorf("release 1's snapshot says LOG_LEVEL=%q, want info", got)
	}
	if got := snapshotValue(t, rel.ConfigSnapshot, "LOG_LEVEL"); got != "debug" {
		t.Errorf("the rollback's snapshot says LOG_LEVEL=%q, want the current debug", got)
	}
}

func snapshotValue(t *testing.T, snapshot, key string) string {
	t.Helper()
	var values map[string]string
	if err := json.Unmarshal([]byte(snapshot), &values); err != nil {
		t.Fatalf("decoding the configuration snapshot: %v", err)
	}
	return values[key]
}

// TestSnapshotIncludesSecrets records the tradeoff the spec accepted explicitly:
// a release snapshot carries secret values so a rollback restores the exact
// environment as well as the exact image.
func TestSnapshotIncludesSecrets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	if err := s.SetConfig(ctx, f.app.ID, "API_KEY", "s3cret", true); err != nil {
		t.Fatalf("setting the secret: %v", err)
	}
	_, rel := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")
	if got := snapshotValue(t, rel.ConfigSnapshot, "API_KEY"); got != "s3cret" {
		t.Errorf("the snapshot holds %q for API_KEY, want the real value", got)
	}
}

// TestSnapshotIsTheConfigTheDeployComposedWith pins the window the reconciler
// waits through: the deploy reads configuration once to build the container's
// Secret, then waits for readiness, which can take minutes. A set_config landing
// in that window must not reach the release, because the pod it describes is
// running the earlier values. Verifies AC-10.
func TestSnapshotIsTheConfigTheDeployComposedWith(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfig(ctx, f.app.ID, "LOG_LEVEL", "info", false); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	dep := mustCreateDeployment(t, s, f, f.upload.ID)
	mustTransition(t, s, dep.ID, domain.StateBuilding)
	mustTransition(t, s, dep.ID, domain.StatePushing)
	if err := s.RecordBuildResult(ctx, dep.ID, store.BuildResult{ImageDigest: "sha256:aaa"}); err != nil {
		t.Fatalf("recording the build result: %v", err)
	}
	mustTransition(t, s, dep.ID, domain.StateDeploying)

	// What the workload was actually composed from, read before the wait.
	composed := configForDeploy(t, s, f.app.ID)

	// The caller changes configuration while the deployment is still coming up.
	if err := s.SetConfig(ctx, f.app.ID, "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("changing configuration mid deploy: %v", err)
	}

	_, rel, err := s.MarkHealthy(ctx, dep.ID, composed)
	if err != nil {
		t.Fatalf("marking healthy: %v", err)
	}
	if got := snapshotValue(t, rel.ConfigSnapshot, "LOG_LEVEL"); got != "info" {
		t.Errorf("the release snapshotted LOG_LEVEL=%q, want info: the pod was given info, "+
			"and debug only reaches the next deploy", got)
	}
}

// TestSlugsAreRetiredForever checks that a soft deleted app keeps its hostname
// reserved. Verifies AC-11, AC-12.
func TestSlugsAreRetiredForever(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	acc, err := s.CreateAccount(ctx, "toyin")
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}

	first, err := s.CreateApp(ctx, acc.ID, "Checkout")
	if err != nil {
		t.Fatalf("creating the first app: %v", err)
	}
	if err := s.SoftDeleteApp(ctx, first.ID); err != nil {
		t.Fatalf("soft deleting: %v", err)
	}
	if _, err := s.GetApp(ctx, first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a soft deleted app reads as %v, want ErrNotFound", err)
	}
	if _, err := s.GetAppBySlug(ctx, first.Slug); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a soft deleted slug still resolves: %v", err)
	}
	taken, err := s.SlugTaken(ctx, first.Slug)
	if err != nil {
		t.Fatalf("checking the retired slug: %v", err)
	}
	if !taken {
		t.Error("the retired slug is free again, so another app could take the hostname")
	}

	second, err := s.CreateApp(ctx, acc.ID, "Checkout")
	if err != nil {
		t.Fatalf("reusing the name after a delete: %v", err)
	}
	if second.Slug == first.Slug {
		t.Errorf("the new app took the retired slug %q", second.Slug)
	}

	// The deleted app's rows are still there; nothing was removed.
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM apps`).Scan(&n); err != nil {
		t.Fatalf("counting apps: %v", err)
	}
	if n != 2 {
		t.Errorf("found %d app rows, want both the live one and the deleted one", n)
	}
	apps, err := s.ListAppsByAccount(ctx, acc.ID, store.Page{})
	if err != nil {
		t.Fatalf("listing apps: %v", err)
	}
	if len(apps) != 1 || apps[0].ID != second.ID {
		t.Errorf("the listing returned %d apps, want only the live one", len(apps))
	}
}

// TestSlugCollisionRetries pins the suffix source so a collision is certain,
// which is the only way to exercise the retry. Verifies AC-11.
func TestSlugCollisionRetries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("a fresh suffix rescues the create", func(t *testing.T) {
		t.Parallel()
		calls := 0
		s := newStoreWithSuffix(t, func() string {
			calls++
			if calls <= 2 {
				return "aaaaaa"
			}
			return "bbbbbb"
		})
		acc, err := s.CreateAccount(ctx, "toyin")
		if err != nil {
			t.Fatalf("creating the account: %v", err)
		}
		first, err := s.CreateApp(ctx, acc.ID, "Checkout")
		if err != nil {
			t.Fatalf("creating the first app: %v", err)
		}
		if first.Slug != "checkout-aaaaaa" {
			t.Fatalf("first slug is %q, want the pinned one", first.Slug)
		}
		if err := s.SoftDeleteApp(ctx, first.ID); err != nil {
			t.Fatalf("soft deleting: %v", err)
		}
		second, err := s.CreateApp(ctx, acc.ID, "Checkout")
		if err != nil {
			t.Fatalf("the retry did not rescue the create: %v", err)
		}
		if second.Slug != "checkout-bbbbbb" {
			t.Errorf("second slug is %q, want the retried one", second.Slug)
		}
	})

	t.Run("a suffix that never frees up fails the create", func(t *testing.T) {
		t.Parallel()
		s := newStoreWithSuffix(t, func() string { return "aaaaaa" })
		acc, err := s.CreateAccount(ctx, "toyin")
		if err != nil {
			t.Fatalf("creating the account: %v", err)
		}
		first, err := s.CreateApp(ctx, acc.ID, "Checkout")
		if err != nil {
			t.Fatalf("creating the first app: %v", err)
		}
		if err := s.SoftDeleteApp(ctx, first.ID); err != nil {
			t.Fatalf("soft deleting: %v", err)
		}
		if _, err := s.CreateApp(ctx, acc.ID, "Checkout"); !errors.Is(err, store.ErrSlugTaken) {
			t.Fatalf("got %v, want ErrSlugTaken after the retries ran out", err)
		}
	})
}

func newStoreWithSuffix(t *testing.T, suffix func() string) *store.Store {
	t.Helper()
	s, err := store.Open(store.Options{
		Path:         filepath.Join(t.TempDir(), "deployer.db"),
		Clock:        &ids.FixedClock{T: testStart},
		SuffixSource: suffix,
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return s
}

// TestLiveNameIsUniquePerAccount checks the partial unique index on app names.
func TestLiveNameIsUniquePerAccount(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	if _, err := s.CreateApp(ctx, f.account.ID, "Checkout Service"); !errors.Is(err, store.ErrAppNameTaken) {
		t.Fatalf("got %v, want ErrAppNameTaken", err)
	}
}

// TestSoftDeleteWaitsForAnInFlightDeployment checks the guard on tearing an app
// down mid deploy.
func TestSoftDeleteWaitsForAnInFlightDeployment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	dep := mustCreateDeployment(t, s, f, f.upload.ID)

	if err := s.SoftDeleteApp(ctx, f.app.ID); !errors.Is(err, store.ErrDeploymentInFlight) {
		t.Fatalf("got %v, want ErrDeploymentInFlight", err)
	}
	mustTransition(t, s, dep.ID, domain.StateFailed)
	if err := s.SoftDeleteApp(ctx, f.app.ID); err != nil {
		t.Fatalf("deleting once the deployment finished: %v", err)
	}
}

// TestDeployingToADeletedAppIsRefused checks that a retired app cannot be
// deployed to. Verifies AC-12.
func TestDeployingToADeletedAppIsRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	if err := s.SoftDeleteApp(ctx, f.app.ID); err != nil {
		t.Fatalf("soft deleting: %v", err)
	}
	_, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: f.app.ID, AccountID: f.account.ID, UploadID: &f.upload.ID,
	})
	if !errors.Is(err, store.ErrAppDeleted) {
		t.Fatalf("got %v, want ErrAppDeleted", err)
	}
}

// TestPagingWalksTheWholeList checks that every list is paged and the cursor
// moves.
func TestPagingWalksTheWholeList(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, clock := newStore(t)
	acc, err := s.CreateAccount(ctx, "toyin")
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	const total = 7
	for i := range total {
		clock.Advance(time.Second)
		if _, err := s.CreateApp(ctx, acc.ID, "app"+string(rune('a'+i))); err != nil {
			t.Fatalf("creating app %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	for range total {
		page, err := s.ListAppsByAccount(ctx, acc.ID, store.Page{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatalf("listing apps: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			if seen[a.ID] {
				t.Fatalf("app %s came back on two pages", a.ID)
			}
			seen[a.ID] = true
		}
		if len(page) > 3 {
			t.Fatalf("a page of %d ignored the limit", len(page))
		}
		cursor = page[len(page)-1].ID
	}
	if len(seen) != total {
		t.Errorf("paging found %d apps, want %d", len(seen), total)
	}
}
