package store

import (
	"context"
	"fmt"

	"github.com/toyinogun/deployer/internal/suspend"
)

// SuspendStore adapts the store to the narrow interface internal/suspend
// declares, so that package holds the rules without depending on this one.
type SuspendStore struct{ s *Store }

// ForSuspend returns the suspension facing view of the store.
func ForSuspend(s *Store) SuspendStore { return SuspendStore{s: s} }

// Compile time proof that the adapter is what internal/suspend asked for.
var _ suspend.Apps = SuspendStore{}

// DeployedAppsByAccount reads the apps a suspension stops and a restore starts:
// not soft deleted, and holding a release, so an app that never deployed
// successfully is left alone in both directions (spec 0018, AC-3).
func (a SuspendStore) DeployedAppsByAccount(ctx context.Context, accountID string) ([]suspend.App, error) {
	rows, err := a.s.q.ListDeployedAppsByAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: reading the deployed apps of account %s: %w", accountID, err)
	}
	apps := make([]suspend.App, 0, len(rows))
	for _, r := range rows {
		apps = append(apps, suspend.App{ID: r.ID, Slug: r.Slug})
	}
	return apps, nil
}

// DeployedAppsOfSuspendedAccounts reads the same shape across every suspended
// account, which is the sweep's whole list (spec 0018, AC-7).
func (a SuspendStore) DeployedAppsOfSuspendedAccounts(ctx context.Context) ([]suspend.SuspendedApp, error) {
	rows, err := a.s.q.ListDeployedAppsOfSuspendedAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: reading the apps of suspended accounts: %w", err)
	}
	apps := make([]suspend.SuspendedApp, 0, len(rows))
	for _, r := range rows {
		apps = append(apps, suspend.SuspendedApp{
			App:       suspend.App{ID: r.ID, Slug: r.Slug},
			AccountID: r.AccountID,
		})
	}
	return apps, nil
}

// AccountSuspended reports whether that account is suspended right now. The
// sweep asks immediately before each write, because its list is a snapshot and a
// restore can land inside one tick (spec 0018, AC-24).
func (a SuspendStore) AccountSuspended(ctx context.Context, accountID string) (bool, error) {
	acc, err := a.s.GetAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	return acc.DisabledAt != nil, nil
}
