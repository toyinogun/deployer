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
