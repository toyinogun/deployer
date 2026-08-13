package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// slugAttempts is how many fresh suffixes a create tries before giving up. A
// collision needs two six character suffixes to match, so reaching five means
// something is wrong rather than unlucky.
const slugAttempts = 5

// CreateApp registers an app and derives its permanent slug from the name. The
// slug is globally unique across every app row that has ever existed, soft
// deleted ones included, so a retired hostname is never reused. A suffix
// collision is retried with a fresh suffix up to slugAttempts times.
func (s *Store) CreateApp(ctx context.Context, accountID, name string) (App, error) {
	now := s.now()
	var lastErr error
	for range slugAttempts {
		app, err := s.q.CreateApp(ctx, sqlcgen.CreateAppParams{
			ID:        ids.New(ids.App, s.clock.Now()),
			AccountID: accountID,
			Name:      name,
			Slug:      domain.DeriveSlugWithSuffix(name, s.suffix()),
			CreatedAt: now,
			UpdatedAt: now,
		})
		switch {
		case err == nil:
			return app, nil
		case isUniqueViolation(err):
			// Either the slug collided, which a fresh suffix fixes, or the
			// account already has a live app by this name, which it does not.
			taken, checkErr := s.nameTaken(ctx, accountID, name)
			if checkErr != nil {
				return App{}, checkErr
			}
			if taken {
				return App{}, ErrAppNameTaken
			}
			lastErr = err
		default:
			return App{}, fmt.Errorf("store: creating app %q: %w", name, err)
		}
	}
	return App{}, fmt.Errorf("store: %d slug attempts for %q all collided: %w", slugAttempts, name, errors.Join(ErrSlugTaken, lastErr))
}

// nameTaken reports whether the account already has a live app with this name.
func (s *Store) nameTaken(ctx context.Context, accountID, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM apps WHERE account_id = ? AND name = ? AND deleted_at IS NULL`,
		accountID, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: checking whether %q is taken: %w", name, err)
	}
	return n > 0, nil
}

// GetApp reads one live app. A soft deleted app reads as not found.
func (s *Store) GetApp(ctx context.Context, id string) (App, error) {
	app, err := s.q.GetApp(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, fmt.Errorf("store: reading app %s: %w", id, err)
	}
	return app, nil
}

// GetAppBySlug reads one live app by its slug.
func (s *Store) GetAppBySlug(ctx context.Context, slug string) (App, error) {
	app, err := s.q.GetAppBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, fmt.Errorf("store: reading app %q: %w", slug, err)
	}
	return app, nil
}

// ListAppsByAccount returns one page of an account's live apps, newest first.
func (s *Store) ListAppsByAccount(ctx context.Context, accountID string, page Page) ([]App, error) {
	apps, err := s.q.ListAppsByAccount(ctx, sqlcgen.ListAppsByAccountParams{
		AccountID: accountID,
		Cursor:    page.Cursor,
		PageLimit: page.limit(),
	})
	if err != nil {
		return nil, fmt.Errorf("store: listing apps for account %s: %w", accountID, err)
	}
	return apps, nil
}

// AppSummary is one row of the app listing: what the app is serving and how its
// last deploy ended, read as two independent facts rather than one blurred
// state (spec 0012, AC-5).
//
// The absent cases are carried as zero values, because that is what the query
// projects: ServingRelease is zero for an app that has never been healthy,
// LastDeploymentID is empty for one that has never been deployed, and
// LastDeployedAt is empty until something has finished.
type AppSummary struct {
	ID                   string
	Name                 string
	Slug                 string
	CreatedAt            string
	ServingRelease       int64
	LastDeploymentID     string
	LastDeploymentState  string
	LastDeploymentReason string
	LastDeployedAt       string
}

// ListAppSummaries returns an account's live apps, newest first, at most limit
// of them, in one statement. It reads no configuration: the query names neither
// app_config nor a release's snapshot (AC-7, AC-8).
func (s *Store) ListAppSummaries(ctx context.Context, accountID string, limit int64) ([]AppSummary, error) {
	rows, err := s.q.ListAppSummariesByAccount(ctx, sqlcgen.ListAppSummariesByAccountParams{
		AccountID: accountID,
		PageLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: listing apps for account %s: %w", accountID, err)
	}
	out := make([]AppSummary, 0, len(rows))
	for _, r := range rows {
		summary := AppSummary{
			ID:                   r.ID,
			Name:                 r.Name,
			Slug:                 r.Slug,
			CreatedAt:            r.CreatedAt,
			LastDeploymentID:     deref(r.LastDeploymentID),
			LastDeploymentState:  deref(r.LastDeploymentState),
			LastDeploymentReason: deref(r.LastDeploymentReason),
			LastDeployedAt:       r.LastDeployedAt,
		}
		if r.ServingReleaseNumber != nil {
			summary.ServingRelease = *r.ServingReleaseNumber
		}
		out = append(out, summary)
	}
	return out, nil
}

// LiveAppSlugs returns the slug of every app that is not soft deleted. It is one
// query on purpose: the orphan reaper deletes namespaces against this answer, so
// a partial one would be a data loss bug rather than a display bug
// (spec 0012, AC-24).
func (s *Store) LiveAppSlugs(ctx context.Context) ([]string, error) {
	slugs, err := s.q.LiveAppSlugs(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing live app slugs: %w", err)
	}
	return slugs, nil
}

// SoftDeleteApp retires an app without removing anything: its deployments,
// events, and releases stay, and its slug stays reserved forever. A deployment
// still in flight blocks the delete.
func (s *Store) SoftDeleteApp(ctx context.Context, appID string) error {
	now := s.now()
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		inFlight, err := q.CountInFlightDeploymentsForApp(ctx, appID)
		if err != nil {
			return fmt.Errorf("store: checking in flight deployments for %s: %w", appID, err)
		}
		if inFlight > 0 {
			return ErrDeploymentInFlight
		}
		n, err := q.SoftDeleteApp(ctx, sqlcgen.SoftDeleteAppParams{Now: ptr(now), ID: appID})
		if err != nil {
			return fmt.Errorf("store: soft deleting app %s: %w", appID, err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SlugTaken reports whether a slug has ever been used, soft deleted apps
// included.
func (s *Store) SlugTaken(ctx context.Context, slug string) (bool, error) {
	taken, err := s.q.SlugExists(ctx, slug)
	if err != nil {
		return false, fmt.Errorf("store: checking slug %q: %w", slug, err)
	}
	return taken, nil
}
