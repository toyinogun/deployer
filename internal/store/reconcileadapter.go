package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// ReconcileStore adapts the store to the narrow interfaces internal/reconcile
// declares, so the loop that drives a deployment never sees this package, its
// generated row types, or its driver.
type ReconcileStore struct{ s *Store }

// ForReconcile returns the reconcile facing view of the store.
func ForReconcile(s *Store) ReconcileStore { return ReconcileStore{s: s} }

// Compile time proof that the adapter is what internal/reconcile asked for.
var (
	_ reconcile.Deployments = ReconcileStore{}
	_ reconcile.Apps        = ReconcileStore{}
)

// deploymentRow maps a stored deployment onto the fields the loop reads.
func deploymentRow(d Deployment) reconcile.Deployment {
	return reconcile.Deployment{
		ID:           d.ID,
		AppID:        d.AppID,
		UploadID:     deref(d.UploadID),
		State:        domain.State(d.State),
		ImageRepo:    deref(d.ImageRepo),
		ImageDigest:  deref(d.ImageDigest),
		CreatedAt:    stamped(d.CreatedAt),
		BuildJobName: deref(d.BuildJobName),
	}
}

// stamped reads a stored timestamp back. Every column is written through
// ids.Stamp, so anything unreadable here is a platform bug rather than data: it
// reads as the zero time, which the loop treats as a full deploy budget rather
// than as an already overdue deployment.
func stamped(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		slog.Error("a stored timestamp is unreadable", "value", s, "error", err)
		return time.Time{}
	}
	return t
}

// ClaimNext hands the loop the oldest queued deployment, mapping an empty queue
// onto the loop's own sentinel.
func (a ReconcileStore) ClaimNext(ctx context.Context, claimedBy string) (reconcile.Deployment, error) {
	dep, err := a.s.ClaimNext(ctx, claimedBy)
	if errors.Is(err, ErrNotFound) {
		return reconcile.Deployment{}, reconcile.ErrNoWork
	}
	if err != nil {
		return reconcile.Deployment{}, err
	}
	return deploymentRow(dep), nil
}

// ListNonTerminal returns everything still in flight, which the startup sweep
// reconciles against the cluster.
func (a ReconcileStore) ListNonTerminal(ctx context.Context) ([]reconcile.Deployment, error) {
	deps, err := a.s.ListNonTerminalDeployments(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]reconcile.Deployment, 0, len(deps))
	for _, d := range deps {
		out = append(out, deploymentRow(d))
	}
	return out, nil
}

// Transition moves a deployment, mapping a row that something else already
// ended onto the loop's own sentinel so a supersession reads as a race to stop
// on rather than as a fault.
func (a ReconcileStore) Transition(ctx context.Context, id string, to domain.State, reason, detail string) error {
	_, err := a.s.Transition(ctx, id, to, reason, detail)
	if errors.Is(err, ErrTerminal) {
		return fmt.Errorf("%w: %w", reconcile.ErrNotInFlight, err)
	}
	return err
}

// RecordBuild stores what the build produced, without moving the row.
func (a ReconcileStore) RecordBuild(ctx context.Context, id, jobName, imageRepo, imageDigest string) error {
	return a.s.RecordBuildResult(ctx, id, BuildResult{
		BuildPath:    "buildpacks",
		BuildJobName: jobName,
		ImageRepo:    imageRepo,
		ImageDigest:  imageDigest,
	})
}

// MarkHealthy runs the one transaction that mints a release.
func (a ReconcileStore) MarkHealthy(ctx context.Context, id string) (reconcile.Release, error) {
	_, rel, err := a.s.MarkHealthy(ctx, id)
	if errors.Is(err, ErrTerminal) {
		return reconcile.Release{}, fmt.Errorf("%w: %w", reconcile.ErrNotInFlight, err)
	}
	if err != nil {
		return reconcile.Release{}, err
	}
	return reconcile.Release{Number: rel.ReleaseNumber, Digest: rel.ImageDigest}, nil
}

// Get reads the app a deployment is for.
func (a ReconcileStore) Get(ctx context.Context, id string) (reconcile.App, error) {
	app, err := a.s.GetApp(ctx, id)
	if err != nil {
		return reconcile.App{}, err
	}
	return reconcile.App{ID: app.ID, Slug: app.Slug}, nil
}

// GetAppByName reads the one live app an account gave that name, which is the
// lookup every deploy starts with. The same pair always resolves to the same
// row, which is what keeps an app's hostname stable across deploys (AC-4).
func (s *Store) GetAppByName(ctx context.Context, accountID, name string) (App, error) {
	app, err := s.q.GetAppByName(ctx, sqlcgen.GetAppByNameParams{AccountID: accountID, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, fmt.Errorf("store: reading app %q for account %s: %w", name, accountID, err)
	}
	return app, nil
}

// GetReleaseByDeployment reads the release a deployment minted.
func (s *Store) GetReleaseByDeployment(ctx context.Context, deploymentID string) (Release, error) {
	rel, err := s.q.GetReleaseByDeployment(ctx, deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return Release{}, ErrNotFound
	}
	if err != nil {
		return Release{}, fmt.Errorf("store: reading the release of deployment %s: %w", deploymentID, err)
	}
	return rel, nil
}
