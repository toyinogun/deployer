// Package store_test exercises the store against a real SQLite file in a
// temporary directory. Nothing here is mocked: the constraints, the indexes, and
// the transactions under test are the ones that will run in the cluster.
package store_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
)

var testStart = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// newStore opens a migrated database on a real file with a clock the test owns.
func newStore(t *testing.T) (*store.Store, *ids.FixedClock) {
	t.Helper()
	clock := &ids.FixedClock{T: testStart}
	s, err := store.Open(store.Options{
		Path:  filepath.Join(t.TempDir(), "deployer.db"),
		Clock: clock,
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
	return s, clock
}

// fixture is an account plus an app plus an upload, the minimum a deployment
// needs to exist.
type fixture struct {
	account store.Account
	app     store.App
	upload  store.Upload
}

func newFixture(t *testing.T, s *store.Store) fixture {
	t.Helper()
	ctx := t.Context()
	acc, err := s.CreateAccount(ctx, "toyin")
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	app, err := s.CreateApp(ctx, acc.ID, "Checkout Service", 100)
	if err != nil {
		t.Fatalf("creating the app: %v", err)
	}
	up := newUpload(t, s, acc.ID, "hash-1")
	return fixture{account: acc, app: app, upload: up}
}

func newUpload(t *testing.T, s *store.Store, accountID, tokenHash string) store.Upload {
	t.Helper()
	up, err := s.CreateUpload(t.Context(), store.NewUpload{
		AccountID:      accountID,
		Path:           "/data/uploads/" + tokenHash + ".tar.gz",
		SizeBytes:      2048,
		SHA256:         "abc",
		FetchTokenHash: tokenHash,
		ExpiresAt:      ids.Stamp(testStart.Add(time.Hour)),
	}, 0)
	if err != nil {
		t.Fatalf("creating the upload: %v", err)
	}
	return up
}

func mustCreateDeployment(t *testing.T, s *store.Store, f fixture, uploadID string) store.Deployment {
	t.Helper()
	dep, _, err := s.CreateDeployment(t.Context(), store.CreateDeploymentInput{
		AppID:     f.app.ID,
		AccountID: f.account.ID,
		UploadID:  &uploadID,
	})
	if err != nil {
		t.Fatalf("creating the deployment: %v", err)
	}
	return dep
}

func mustTransition(t *testing.T, s *store.Store, id string, to domain.State) store.Deployment {
	t.Helper()
	dep, err := s.Transition(t.Context(), id, to, "", "")
	if err != nil {
		t.Fatalf("moving %s to %s: %v", id, to, err)
	}
	return dep
}

// TestHappyPath walks a build deploy the whole way and checks the trail it leaves.
// Verifies AC-4, AC-5, AC-9.
func TestHappyPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	dep := mustCreateDeployment(t, s, f, f.upload.ID)
	if dep.State != string(domain.StateQueued) {
		t.Fatalf("a new deployment is %q, want queued", dep.State)
	}
	if dep.StartedAt != nil {
		t.Error("a queued deployment should not have started yet")
	}

	building := mustTransition(t, s, dep.ID, domain.StateBuilding)
	if building.StartedAt == nil {
		t.Error("leaving queued should stamp started_at")
	}
	mustTransition(t, s, dep.ID, domain.StatePushing)
	if err := s.RecordBuildResult(ctx, dep.ID, store.BuildResult{
		BuildPath:    "buildpacks",
		BuildJobName: "build-1",
		ImageRepo:    "registry.internal/checkout",
		ImageDigest:  "sha256:aaa",
	}); err != nil {
		t.Fatalf("recording the build result: %v", err)
	}
	mustTransition(t, s, dep.ID, domain.StateDeploying)

	healthy, rel, err := s.MarkHealthy(ctx, dep.ID, nil)
	if err != nil {
		t.Fatalf("marking healthy: %v", err)
	}
	if healthy.State != string(domain.StateHealthy) {
		t.Errorf("state is %q, want healthy", healthy.State)
	}
	if healthy.FinishedAt == nil {
		t.Error("entering a terminal state should stamp finished_at")
	}
	if rel.ReleaseNumber != 1 {
		t.Errorf("first release is number %d, want 1", rel.ReleaseNumber)
	}
	if rel.ImageDigest != "sha256:aaa" {
		t.Errorf("release digest is %q, want the one the build pushed", rel.ImageDigest)
	}

	events, err := s.ListDeploymentEvents(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	want := []struct{ from, to string }{
		{"", "queued"},
		{"queued", "building"},
		{"building", "pushing"},
		{"pushing", "deploying"},
		{"deploying", "healthy"},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, w := range want {
		var from string
		if events[i].FromState != nil {
			from = *events[i].FromState
		}
		if from != w.from || events[i].ToState != w.to {
			t.Errorf("event %d is %q to %q, want %q to %q", i, from, events[i].ToState, w.from, w.to)
		}
	}

	app, err := s.GetApp(ctx, f.app.ID)
	if err != nil {
		t.Fatalf("reading the app back: %v", err)
	}
	if app.CurrentReleaseID == nil || *app.CurrentReleaseID != rel.ID {
		t.Errorf("app points at %v, want release %s", app.CurrentReleaseID, rel.ID)
	}
}

// TestIllegalTransitionWritesNothing checks that a refused move leaves no trace.
// Verifies AC-6.
func TestIllegalTransitionWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	dep := mustCreateDeployment(t, s, f, f.upload.ID)
	mustTransition(t, s, dep.ID, domain.StateBuilding)
	mustTransition(t, s, dep.ID, domain.StateFailed)

	before, err := s.ListDeploymentEvents(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	if _, err := s.Transition(ctx, dep.ID, domain.StateBuilding, "", ""); !errors.Is(err, store.ErrIllegalTransition) {
		t.Fatalf("leaving a terminal state returned %v, want ErrIllegalTransition", err)
	}
	after, err := s.ListDeploymentEvents(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused transition wrote %d event rows", len(after)-len(before))
	}
	current, err := s.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the deployment: %v", err)
	}
	if current.State != string(domain.StateFailed) {
		t.Errorf("state moved to %q despite the refusal", current.State)
	}
}

// TestDatabaseRefusesAnUnknownState checks the CHECK constraint, not the Go code.
// Verifies AC-4.
func TestDatabaseRefusesAnUnknownState(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	f := newFixture(t, s)
	dep := mustCreateDeployment(t, s, f, f.upload.ID)

	_, err := s.DB().ExecContext(t.Context(),
		`UPDATE deployments SET state = 'pending' WHERE id = ?`, dep.ID)
	if err == nil {
		t.Fatal("the database accepted a state outside the seven")
	}
}

// TestSupersession checks that starting a deployment while one is in flight
// cancels the old one, with its event, and leaves exactly one active row.
// Verifies AC-5, AC-7.
func TestSupersession(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	first := mustCreateDeployment(t, s, f, f.upload.ID)
	mustTransition(t, s, first.ID, domain.StateBuilding)

	second := newUpload(t, s, f.account.ID, "hash-2")
	dep, supersededID, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID:     f.app.ID,
		AccountID: f.account.ID,
		UploadID:  &second.ID,
	})
	if err != nil {
		t.Fatalf("creating the second deployment: %v", err)
	}
	if supersededID != first.ID {
		t.Errorf("superseded %q, want %q", supersededID, first.ID)
	}

	old, err := s.GetDeployment(ctx, first.ID)
	if err != nil {
		t.Fatalf("reading the superseded deployment: %v", err)
	}
	if old.State != string(domain.StateCancelled) {
		t.Errorf("the superseded deployment is %q, want cancelled", old.State)
	}
	// The reason goes on the row as well as its event, so a status read of a
	// cancelled deployment needs no special case (spec 0005, AC-12).
	if old.FailureReason == nil || *old.FailureReason != string(domain.ReasonSuperseded) {
		t.Errorf("the superseded deployment reports %v, want superseded", old.FailureReason)
	}

	events, err := s.ListDeploymentEvents(ctx, first.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	last := events[len(events)-1]
	if last.ToState != string(domain.StateCancelled) || last.Reason == nil || *last.Reason != "superseded" {
		t.Errorf("last event is %q with reason %v, want cancelled/superseded", last.ToState, last.Reason)
	}

	inFlight, err := s.ListNonTerminalDeployments(ctx)
	if err != nil {
		t.Fatalf("listing in flight deployments: %v", err)
	}
	if len(inFlight) != 1 || inFlight[0].ID != dep.ID {
		t.Errorf("got %d in flight deployments, want only %s", len(inFlight), dep.ID)
	}
}

// TestIndexRefusesASecondInFlightRow checks the partial unique index directly,
// so the invariant does not rest on the store remembering to supersede.
// Verifies AC-7.
func TestIndexRefusesASecondInFlightRow(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	f := newFixture(t, s)
	mustCreateDeployment(t, s, f, f.upload.ID)

	second := newUpload(t, s, f.account.ID, "hash-2")
	_, err := s.DB().ExecContext(t.Context(),
		`INSERT INTO deployments (id, app_id, account_id, upload_id, state, created_at, updated_at)
		 VALUES ('dep_manual', ?, ?, ?, 'queued', ?, ?)`,
		f.app.ID, f.account.ID, second.ID, ids.Stamp(testStart), ids.Stamp(testStart))
	if err == nil {
		t.Fatal("the index allowed a second in flight deployment for one app")
	}
}

// TestClaimRaces checks that a contested queued row goes to exactly one caller.
// Verifies AC-8.
func TestClaimRaces(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	dep := mustCreateDeployment(t, s, f, f.upload.ID)

	const racers = 8
	type result struct {
		id  string
		err error
	}
	results := make(chan result, racers)
	start := make(chan struct{})
	for i := range racers {
		go func() {
			<-start
			d, err := s.ClaimNext(ctx, "pod-"+string(rune('a'+i)))
			results <- result{id: d.ID, err: err}
		}()
	}
	close(start)

	var winners, losers int
	for range racers {
		r := <-results
		switch {
		case r.err == nil:
			winners++
			if r.id != dep.ID {
				t.Errorf("claimed %q, want %q", r.id, dep.ID)
			}
		case errors.Is(r.err, store.ErrNotFound):
			losers++
		default:
			t.Errorf("unexpected claim error: %v", r.err)
		}
	}
	if winners != 1 || losers != racers-1 {
		t.Fatalf("%d winners and %d losers, want 1 and %d", winners, losers, racers-1)
	}

	claimed, err := s.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the claimed deployment: %v", err)
	}
	if claimed.ClaimedAt == nil || claimed.ClaimedBy == nil || *claimed.ClaimedBy == "" {
		t.Error("the winning claim did not record who took it")
	}
}

// TestDeploymentSourceIsExactlyOne checks the CHECK constraint and the store's
// own guard. Verifies AC-16.
func TestDeploymentSourceIsExactlyOne(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)

	t.Run("the store refuses neither", func(t *testing.T) {
		_, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{AppID: f.app.ID, AccountID: f.account.ID})
		if !errors.Is(err, store.ErrDeploymentSourceAmbiguous) {
			t.Errorf("got %v, want ErrDeploymentSourceAmbiguous", err)
		}
	})
	t.Run("the store refuses both", func(t *testing.T) {
		relID := "rel_whatever"
		_, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
			AppID: f.app.ID, AccountID: f.account.ID,
			UploadID: &f.upload.ID, SourceReleaseID: &relID,
		})
		if !errors.Is(err, store.ErrDeploymentSourceAmbiguous) {
			t.Errorf("got %v, want ErrDeploymentSourceAmbiguous", err)
		}
	})
	t.Run("the database refuses neither", func(t *testing.T) {
		_, err := s.DB().ExecContext(ctx,
			`INSERT INTO deployments (id, app_id, account_id, state, created_at, updated_at)
			 VALUES ('dep_none', ?, ?, 'queued', ?, ?)`,
			f.app.ID, f.account.ID, ids.Stamp(testStart), ids.Stamp(testStart))
		if err == nil {
			t.Error("the CHECK constraint allowed a deployment with no source")
		}
	})
}

// TestForeignKeysRestrict checks that nothing cascades. Verifies AC-16.
func TestForeignKeysRestrict(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	f := newFixture(t, s)
	mustCreateDeployment(t, s, f, f.upload.ID)

	if _, err := s.DB().ExecContext(t.Context(), `DELETE FROM apps WHERE id = ?`, f.app.ID); err == nil {
		t.Fatal("deleting an app with deployments succeeded; foreign keys are not being enforced")
	}
}
