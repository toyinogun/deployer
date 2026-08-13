package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// ConfigEntry is one configuration key as a tool response sees it. Value is empty
// for a secret key, and IsSecret says why.
type ConfigEntry struct {
	Key      string
	Value    string
	IsSecret bool
}

// ListConfigForResponse is the only read path a tool response may use. It returns
// every key with its secret flag, and the value only for keys that are not
// secret.
func (s *Store) ListConfigForResponse(ctx context.Context, appID string) ([]ConfigEntry, error) {
	rows, err := s.q.ListConfigForResponse(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("store: reading configuration for app %s: %w", appID, err)
	}
	out := make([]ConfigEntry, 0, len(rows))
	for _, r := range rows {
		var value string
		if v, ok := r.Value.(string); ok {
			value = v
		}
		out = append(out, ConfigEntry{Key: r.Key, Value: value, IsSecret: r.IsSecret == 1})
	}
	return out, nil
}

// ListConfigForDeploy returns full key and value pairs, secrets included. Only
// the deploy path and the release snapshot may call it, never a tool response.
func (s *Store) ListConfigForDeploy(ctx context.Context, appID string) ([]ConfigEntry, error) {
	rows, err := s.q.ListConfigForDeploy(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("store: reading configuration for app %s: %w", appID, err)
	}
	out := make([]ConfigEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, ConfigEntry{Key: r.Key, Value: r.Value, IsSecret: r.IsSecret == 1})
	}
	return out, nil
}

// CurrentReleaseConfig returns the configuration the app's current release ran
// with, secret values included. An app with no release yet has an empty one,
// which is not an error: it has simply never run.
//
// This is what makes redacting a rotated secret possible. The pod that is
// running printed the old value, and the old value only survives here
// (spec 0010, AC-11).
func (s *Store) CurrentReleaseConfig(ctx context.Context, appID string) (map[string]string, error) {
	app, err := s.GetApp(ctx, appID)
	if err != nil {
		return nil, err
	}
	releaseID := deref(app.CurrentReleaseID)
	if releaseID == "" {
		return map[string]string{}, nil
	}
	rel, err := s.q.GetRelease(ctx, releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading release %s: %w", releaseID, err)
	}
	values := map[string]string{}
	if rel.ConfigSnapshot == "" {
		return values, nil
	}
	if err := json.Unmarshal([]byte(rel.ConfigSnapshot), &values); err != nil {
		return nil, fmt.Errorf("store: decoding the configuration snapshot of release %s: %w", releaseID, err)
	}
	return values, nil
}

// SetConfig writes one configuration key, replacing any existing value.
func (s *Store) SetConfig(ctx context.Context, appID, key, value string, isSecret bool) error {
	return s.SetConfigBatch(ctx, appID, []ConfigEntry{{Key: key, Value: value, IsSecret: isSecret}})
}

// SetConfigBatch writes every entry or none of them. The transaction is what
// makes a refused call leave nothing behind, so a caller never has to undo half
// a write (spec 0010, AC-1).
func (s *Store) SetConfigBatch(ctx context.Context, appID string, entries []ConfigEntry) error {
	now := s.now()
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		for _, e := range entries {
			if !domain.ValidConfigKey(e.Key) {
				return fmt.Errorf("store: %q: %w", e.Key, ErrInvalidKey)
			}
			secret := int64(0)
			if e.IsSecret {
				secret = 1
			}
			err := q.SetConfig(ctx, sqlcgen.SetConfigParams{
				AppID:    appID,
				Key:      e.Key,
				Value:    e.Value,
				IsSecret: secret,
				Now:      now,
			})
			if err != nil {
				// The key shape is a CHECK constraint, so a bad key comes back as a
				// constraint failure rather than something Go rejected first.
				if isConstraintViolation(err) {
					return fmt.Errorf("store: %q: %w", e.Key, ErrInvalidKey)
				}
				return fmt.Errorf("store: setting %q on app %s: %w", e.Key, appID, err)
			}
		}
		return nil
	})
}

// UnsetConfig removes one configuration key.
func (s *Store) UnsetConfig(ctx context.Context, appID, key string) error {
	return s.UnsetConfigBatch(ctx, appID, []string{key})
}

// UnsetConfigBatch removes every key or none of them. A key the app does not
// have rolls the whole transaction back with ErrNotFound, which is what makes a
// partly wrong call leave the app exactly as it was (spec 0010, AC-3).
func (s *Store) UnsetConfigBatch(ctx context.Context, appID string, keys []string) error {
	return s.inTx(ctx, func(q *sqlcgen.Queries) error {
		for _, key := range keys {
			n, err := q.UnsetConfig(ctx, sqlcgen.UnsetConfigParams{AppID: appID, Key: key})
			if err != nil {
				return fmt.Errorf("store: unsetting %q on app %s: %w", key, appID, err)
			}
			if n == 0 {
				return ErrNotFound
			}
		}
		return nil
	})
}
