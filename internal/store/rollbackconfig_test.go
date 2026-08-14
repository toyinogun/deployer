package store_test

import (
	"fmt"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// markRollbackHealthy walks a rollback of the given release to healthy with the
// configuration that release ran with, which is what the reconcile loop hands
// MarkHealthy for a rollback.
func markRollbackHealthy(t *testing.T, s *store.Store, f fixture, releaseID string) store.Release {
	t.Helper()
	ctx := t.Context()
	dep, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: f.app.ID, AccountID: f.account.ID, SourceReleaseID: &releaseID,
	})
	if err != nil {
		t.Fatalf("creating the rollback: %v", err)
	}
	mustTransition(t, s, dep.ID, domain.StateDeploying)

	snapshot, err := s.ReleaseConfigSnapshot(ctx, releaseID)
	if err != nil {
		t.Fatalf("reading the source release's snapshot: %v", err)
	}
	_, rel, err := s.MarkHealthy(ctx, dep.ID, snapshot)
	if err != nil {
		t.Fatalf("marking the rollback healthy: %v", err)
	}
	return rel
}

// configNow reads the app's stored configuration as a map, values and flags.
func configNow(t *testing.T, s *store.Store, appID string) map[string]store.ConfigEntry {
	t.Helper()
	entries, err := s.ListConfigForDeploy(t.Context(), appID)
	if err != nil {
		t.Fatalf("reading configuration: %v", err)
	}
	out := make(map[string]store.ConfigEntry, len(entries))
	for _, e := range entries {
		out[e.Key] = e
	}
	return out
}

// TestARollbackReplacesTheWholeConfigurationSet pins that the restore is a
// replacement and not a merge: a key the release did not have is gone
// afterwards, not left behind.
func TestARollbackReplacesTheWholeConfigurationSet(t *testing.T) {
	// covers: spec 0011 AC-13
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfigBatch(ctx, f.app.ID, []store.ConfigEntry{
		{Key: "LOG_LEVEL", Value: "info", IsSecret: false},
		{Key: "API_KEY", Value: "old-key", IsSecret: true},
	}); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	_, first := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	if err := s.SetConfigBatch(ctx, f.app.ID, []store.ConfigEntry{
		{Key: "LOG_LEVEL", Value: "debug", IsSecret: false},
		{Key: "FEATURE_X", Value: "on", IsSecret: false},
	}); err != nil {
		t.Fatalf("changing configuration: %v", err)
	}
	if err := s.UnsetConfig(ctx, f.app.ID, "API_KEY"); err != nil {
		t.Fatalf("removing a key: %v", err)
	}

	markRollbackHealthy(t, s, f, first.ID)

	got := configNow(t, s, f.app.ID)
	if len(got) != 2 {
		t.Fatalf("the app holds %d keys after the rollback, want the release's 2: %+v", len(got), got)
	}
	if got["LOG_LEVEL"].Value != "info" || got["LOG_LEVEL"].IsSecret {
		t.Errorf("LOG_LEVEL is %+v, want info and not secret", got["LOG_LEVEL"])
	}
	if got["API_KEY"].Value != "old-key" || !got["API_KEY"].IsSecret {
		t.Errorf("API_KEY is %+v, want old-key and secret", got["API_KEY"])
	}
	if _, ok := got["FEATURE_X"]; ok {
		t.Error("FEATURE_X survived a rollback to a release that never had it")
	}
}

// TestASetConfigDuringARollbackIsReverted is the race the spec chose not to
// close: the write succeeded, the caller was told so, and the snapshot still
// wins. It is here so the behaviour is pinned rather than discovered.
func TestASetConfigDuringARollbackIsReverted(t *testing.T) {
	// covers: spec 0011 AC-25
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfig(ctx, f.app.ID, "LOG_LEVEL", "info", false); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	_, first := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	rollback, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: f.app.ID, AccountID: f.account.ID, SourceReleaseID: &first.ID,
	})
	if err != nil {
		t.Fatalf("creating the rollback: %v", err)
	}
	mustTransition(t, s, rollback.ID, domain.StateDeploying)
	snapshot, err := s.ReleaseConfigSnapshot(ctx, first.ID)
	if err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}

	// The window AGENTS.md names: the readiness wait is long enough for a
	// set_config to land in the middle of it, and this one does.
	if err := s.SetConfig(ctx, f.app.ID, "LATE_KEY", "written mid rollback", false); err != nil {
		t.Fatalf("writing configuration mid rollback: %v", err)
	}

	if _, _, err := s.MarkHealthy(ctx, rollback.ID, snapshot); err != nil {
		t.Fatalf("marking the rollback healthy: %v", err)
	}

	got := configNow(t, s, f.app.ID)
	if _, ok := got["LATE_KEY"]; ok {
		t.Error("LATE_KEY survived, but a rollback replaces the whole set with no version check")
	}
	if got["LOG_LEVEL"].Value != "info" {
		t.Errorf("LOG_LEVEL is %q, want the snapshot's info", got["LOG_LEVEL"].Value)
	}
}

// TestAnOldShapeSnapshotRestoresEveryKeyAsSecret covers the one way door the
// spec accepted: a release written before this feature carries no flags, and
// guessing wrong in the other direction would leak a value.
func TestAnOldShapeSnapshotRestoresEveryKeyAsSecret(t *testing.T) {
	// covers: spec 0011 AC-14
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	_, first := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")
	// Rewritten to the shape releases were written in before this feature. Going
	// through SQL is the point: no Go path writes this shape any more, and the
	// rows that hold it were written by a binary that is gone.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE releases SET config_snapshot = ? WHERE id = ?`,
		`{"LOG_LEVEL":"info","API_KEY":"old-key"}`, first.ID); err != nil {
		t.Fatalf("planting the old shape snapshot: %v", err)
	}

	decoded, err := s.ReleaseConfigSnapshot(ctx, first.ID)
	if err != nil {
		t.Fatalf("decoding the old shape snapshot: %v", err)
	}
	if decoded["LOG_LEVEL"].Value != "info" || decoded["API_KEY"].Value != "old-key" {
		t.Fatalf("the old shape decoded to %+v", decoded)
	}
	for key, v := range decoded {
		if !v.Secret {
			t.Errorf("%q restored as not secret; a snapshot with no flag has to read as secret, "+
				"because the other guess leaks a value", key)
		}
	}

	markRollbackHealthy(t, s, f, first.ID)
	for key, e := range configNow(t, s, f.app.ID) {
		if !e.IsSecret {
			t.Errorf("%q came back not secret from an old shape snapshot", key)
		}
	}
}

// TestANewSnapshotRecordsEachKeysFlagOnEveryDeploy checks that the shape change
// is not a rollback only path: an ordinary deploy writes it too, or the next
// rollback of this app would still have nothing to restore.
func TestANewSnapshotRecordsEachKeysFlagOnEveryDeploy(t *testing.T) {
	// covers: spec 0011 AC-15
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	if err := s.SetConfigBatch(ctx, f.app.ID, []store.ConfigEntry{
		{Key: "LOG_LEVEL", Value: "info", IsSecret: false},
		{Key: "API_KEY", Value: "shhh", IsSecret: true},
	}); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	_, rel := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	decoded, err := s.ReleaseConfigSnapshot(ctx, rel.ID)
	if err != nil {
		t.Fatalf("decoding the snapshot: %v", err)
	}
	if decoded["LOG_LEVEL"].Secret {
		t.Error("a plain key was snapshotted as secret")
	}
	if !decoded["API_KEY"].Secret {
		t.Error("a secret key was snapshotted as plain, so a rollback would expose it")
	}
}

// TestARollbackSupersedesWhatIsInFlight checks that a rollback is not a special
// case of the supersession rule: it cancels a running deploy exactly as a
// redeploy does, and a later deploy cancels a rollback the same way.
func TestARollbackSupersedesWhatIsInFlight(t *testing.T) {
	// covers: spec 0011 AC-10
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	_, first := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	// A build deploy is running when the rollback arrives.
	second := newUpload(t, s, f.account.ID, "hash-2")
	inFlight := mustCreateDeployment(t, s, f, second.ID)
	rollback, superseded, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: f.app.ID, AccountID: f.account.ID, SourceReleaseID: &first.ID,
	})
	if err != nil {
		t.Fatalf("creating the rollback: %v", err)
	}
	if superseded != inFlight.ID {
		t.Errorf("the rollback superseded %q, want the in flight deploy %q", superseded, inFlight.ID)
	}
	cancelled, err := s.GetDeployment(ctx, inFlight.ID)
	if err != nil {
		t.Fatalf("reading the superseded deploy: %v", err)
	}
	if cancelled.State != string(domain.StateCancelled) ||
		cancelled.FailureReason == nil || *cancelled.FailureReason != string(domain.ReasonSuperseded) {
		t.Errorf("the in flight deploy is %s with reason %v, want cancelled and superseded",
			cancelled.State, cancelled.FailureReason)
	}

	// And the other direction: a later deploy supersedes the rollback.
	third := newUpload(t, s, f.account.ID, "hash-3")
	if _, superseded, err = s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: f.app.ID, AccountID: f.account.ID, UploadID: &third.ID,
	}); err != nil {
		t.Fatalf("creating the later deploy: %v", err)
	}
	if superseded != rollback.ID {
		t.Errorf("the later deploy superseded %q, want the in flight rollback %q", superseded, rollback.ID)
	}
}

// TestAReleaseNumberResolvesOnlyWithinItsOwnApp checks the lookup a rollback
// refuses on: the same number exists on both apps and each resolves to its own.
func TestAReleaseNumberResolvesOnlyWithinItsOwnApp(t *testing.T) {
	// covers: spec 0011 AC-7
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	_, mine := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	other, err := s.CreateApp(ctx, f.account.ID, "other", 100)
	if err != nil {
		t.Fatalf("creating the second app: %v", err)
	}
	otherUpload := newUpload(t, s, f.account.ID, "hash-other")
	otherFixture := fixture{account: f.account, app: other, upload: otherUpload}
	_, theirs := deployToHealthy(t, s, otherFixture, otherUpload.ID, "sha256:bbb")

	if mine.ReleaseNumber != theirs.ReleaseNumber {
		t.Fatalf("the two apps' first releases are numbered %d and %d; this test needs them equal",
			mine.ReleaseNumber, theirs.ReleaseNumber)
	}

	got, err := s.GetReleaseByNumber(ctx, f.app.ID, mine.ReleaseNumber)
	if err != nil {
		t.Fatalf("resolving release %d of the first app: %v", mine.ReleaseNumber, err)
	}
	if got.ID != mine.ID {
		t.Errorf("release %d of the first app resolved to %q, want %q", mine.ReleaseNumber, got.ID, mine.ID)
	}

	if _, err := s.GetReleaseByNumber(ctx, f.app.ID, mine.ReleaseNumber+99); err == nil {
		t.Error("a release number the app does not have resolved to something")
	}
}

// TestTheListingReadsFiveColumnsNewestFirst checks the bound and the order the
// tool relies on, against more releases than it returns.
func TestTheListingReadsFiveColumnsNewestFirst(t *testing.T) {
	// covers: spec 0011 AC-1
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	const releases = 25
	for i := range releases {
		up := newUpload(t, s, f.account.ID, fmt.Sprintf("listing-hash-%d", i))
		deployToHealthy(t, s, f, up.ID, "sha256:aaa")
	}

	rows, err := s.ListReleaseSummariesByApp(ctx, f.app.ID, 20)
	if err != nil {
		t.Fatalf("listing releases: %v", err)
	}
	if len(rows) != 20 {
		t.Fatalf("the listing returned %d rows, want the bound of 20", len(rows))
	}
	if rows[0].ReleaseNumber != releases {
		t.Errorf("the newest row is release %d, want %d: the listing is newest first",
			rows[0].ReleaseNumber, releases)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ReleaseNumber >= rows[i-1].ReleaseNumber {
			t.Fatalf("row %d is release %d after release %d, so the order is not newest first",
				i, rows[i].ReleaseNumber, rows[i-1].ReleaseNumber)
		}
	}
	for _, r := range rows {
		if r.ID == "" || r.ImageDigest == "" || r.DeploymentID == "" || r.CreatedAt == "" {
			t.Errorf("a summary row came back incomplete: %+v", r)
		}
	}
}
