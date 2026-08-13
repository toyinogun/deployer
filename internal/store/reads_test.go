package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// TestHistoryReads covers the read surface later slices poll: an app's
// deployments and its releases, both paged, newest first.
func TestHistoryReads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, clock := newStore(t)
	f := newFixture(t, s)

	_, first := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")
	clock.Advance(time.Minute)
	second := newUpload(t, s, f.account.ID, "hash-2")
	_, latest := deployToHealthy(t, s, f, second.ID, "sha256:bbb")

	deps, err := s.ListDeploymentsByApp(ctx, f.app.ID, store.Page{})
	if err != nil {
		t.Fatalf("listing deployments: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("got %d deployments, want 2", len(deps))
	}
	if deps[0].CreatedAt < deps[1].CreatedAt {
		t.Error("deployments came back oldest first")
	}

	rels, err := s.ListReleasesByApp(ctx, f.app.ID, store.Page{})
	if err != nil {
		t.Fatalf("listing releases: %v", err)
	}
	if len(rels) != 2 || rels[0].ID != latest.ID {
		t.Errorf("releases came back in the wrong order: %+v", rels)
	}
	if rels[1].ID != first.ID {
		t.Errorf("the older release is %s, want %s", rels[1].ID, first.ID)
	}

	// A limit above the cap is clamped rather than honoured.
	page, err := s.ListReleasesByApp(ctx, f.app.ID, store.Page{Limit: 10_000})
	if err != nil {
		t.Fatalf("listing releases with a huge limit: %v", err)
	}
	if len(page) != 2 {
		t.Errorf("got %d releases, want 2", len(page))
	}
}

// TestMissingRowsReadAsNotFound covers the lookup error path across the store.
func TestMissingRowsReadAsNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)

	if _, err := s.GetAccount(ctx, "acc_nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAccount returned %v, want ErrNotFound", err)
	}
	if _, err := s.GetApp(ctx, "app_nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetApp returned %v, want ErrNotFound", err)
	}
	if _, err := s.GetAppBySlug(ctx, "nobody-aaaaaa"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAppBySlug returned %v, want ErrNotFound", err)
	}
	if _, err := s.GetDeployment(ctx, "dep_nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetDeployment returned %v, want ErrNotFound", err)
	}
	if _, err := s.GetRelease(ctx, "rel_nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetRelease returned %v, want ErrNotFound", err)
	}
	if _, err := s.GetUpload(ctx, "upl_nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetUpload returned %v, want ErrNotFound", err)
	}
	if _, err := s.Transition(ctx, "dep_nobody", domain.StateBuilding, "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Transition returned %v, want ErrNotFound", err)
	}
	if err := s.RecordBuildResult(ctx, "dep_nobody", store.BuildResult{ImageDigest: "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RecordBuildResult returned %v, want ErrNotFound", err)
	}
	if _, _, err := s.MarkHealthy(ctx, "dep_nobody", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("MarkHealthy returned %v, want ErrNotFound", err)
	}
	if err := s.SoftDeleteApp(ctx, "app_nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SoftDeleteApp returned %v, want ErrNotFound", err)
	}
	if err := s.RevokeAPIToken(ctx, "tok_nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RevokeAPIToken returned %v, want ErrNotFound", err)
	}
	if _, err := s.ClaimNext(ctx, "pod-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ClaimNext with nothing queued returned %v, want ErrNotFound", err)
	}
}

// TestRollbackToAnotherAppsReleaseIsRefused checks that a release cannot be
// promoted onto an app it does not belong to.
func TestRollbackToAnotherAppsReleaseIsRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _ := newStore(t)
	f := newFixture(t, s)
	_, rel := deployToHealthy(t, s, f, f.upload.ID, "sha256:aaa")

	other, err := s.CreateApp(ctx, f.account.ID, "Billing")
	if err != nil {
		t.Fatalf("creating the second app: %v", err)
	}
	_, _, err = s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: other.ID, AccountID: f.account.ID, SourceReleaseID: &rel.ID,
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound for a foreign release", err)
	}
	if _, err := s.GetDeployment(ctx, "dep_any"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the refused rollback left a row behind: %v", err)
	}

	unknown := "rel_nobody"
	if _, _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: other.ID, AccountID: f.account.ID, SourceReleaseID: &unknown,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound for an unknown release", err)
	}
}

// TestTouchTokenAndSlugLookup covers the two small helpers the auth path uses.
func TestTouchTokenAndSlugLookup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, clock := newStore(t)
	f := newFixture(t, s)

	tok, err := s.CreateAPIToken(ctx, store.NewToken{
		AccountID: f.account.ID, Name: "laptop", TokenHash: "hash-live", Prefix: "dep_1234",
	})
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if tok.LastUsedAt != nil {
		t.Error("a fresh token should not look used")
	}
	clock.Advance(time.Minute)
	if err := s.TouchToken(ctx, tok.ID); err != nil {
		t.Fatalf("touching: %v", err)
	}
	_, used, err := s.ResolveToken(ctx, "hash-live")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if used.LastUsedAt == nil {
		t.Error("touching the token did not record when")
	}

	taken, err := s.SlugTaken(ctx, f.app.Slug)
	if err != nil {
		t.Fatalf("checking the slug: %v", err)
	}
	if !taken {
		t.Errorf("%q is in use but reads as free", f.app.Slug)
	}
	free, err := s.SlugTaken(ctx, "nobody-aaaaaa")
	if err != nil {
		t.Fatalf("checking a free slug: %v", err)
	}
	if free {
		t.Error("an unused slug reads as taken")
	}
}
