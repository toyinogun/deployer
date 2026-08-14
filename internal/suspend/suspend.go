// Package suspend is the one implementation of stopping an account and letting
// it back in. The admin page, the JSON admin route, and the sweep all call it,
// so a suspension means the same thing whichever door it came through
// (spec 0018, AC-19).
//
// It orchestrates and holds no rules of its own: the lockout is the identity
// service's, the namespace and workload names are internal/deploy's, and the
// replica count is the same constant a deploy composes with. It declares the
// narrow interfaces it needs, so client-go and the store stay at the edges.
package suspend

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/deploy"
)

// App is one of an account's apps, carrying only what a scale needs.
type App struct {
	ID   string
	Slug string
}

// SuspendedApp is one app of a suspended account, as the sweep reads them. The
// account travels with it because the sweep confirms that account is still
// suspended immediately before it writes.
type SuspendedApp struct {
	App
	AccountID string
}

// Apps is the slice of persistence this package reads. It never writes: the one
// write a suspension makes is the identity service's.
type Apps interface {
	// DeployedAppsByAccount returns the account's apps that are not soft deleted
	// and have reached the cluster at least once, in slug order.
	DeployedAppsByAccount(ctx context.Context, accountID string) ([]App, error)
	// DeployedAppsOfSuspendedAccounts returns the same shape of app for every
	// account currently suspended.
	DeployedAppsOfSuspendedAccounts(ctx context.Context) ([]SuspendedApp, error)
	// AccountSuspended reports whether that account is suspended right now.
	AccountSuspended(ctx context.Context, accountID string) (bool, error)
}

// Lockout is the credential half of a suspension: the existing account disable,
// which revokes every live session and email link in its own transaction.
type Lockout interface {
	SetDisabled(ctx context.Context, accountID string, disabled bool) error
}

// Cluster is the one cluster write this package makes.
type Cluster interface {
	ScaleWorkload(ctx context.Context, namespace, name string, replicas int32) error
}

// Result is the outcome of a suspend or a restore.
//
// NotStopped is the third outcome, beside success and failure: the account was
// locked out, but these apps did not take the scale. It is data rather than an
// error string because both admin surfaces render it, the page in its message
// and the JSON route as a field, and neither should be parsing prose
// (spec 0018, AC-6).
type Result struct {
	// Stopped is the slug of every app that took the scale.
	Stopped []string
	// NotStopped is the slug of every app the cluster refused. Empty on a clean
	// run, which is the only thing a caller needs to test.
	NotStopped []string
}

// Service suspends and restores accounts.
type Service struct {
	apps    Apps
	lockout Lockout
	cluster Cluster
	auditor auth.Auditor
}

// New returns a suspension service. cluster may be nil, which is the local run
// with no cluster: the lockout still lands and nothing is scaled.
func New(apps Apps, lockout Lockout, cluster Cluster, auditor auth.Auditor) *Service {
	return &Service{apps: apps, lockout: lockout, cluster: cluster, auditor: auditor}
}

// Suspend locks an account out and stops everything it runs.
//
// The lockout is a database write and it lands first, so a cluster outage can
// never leave a suspended person still signed in (spec 0018, Key invariants).
// The scaling behind it is best effort: one app the cluster refuses is collected
// and reported, never allowed to fail the suspension, and the sweep is what
// catches it afterwards.
func (s *Service) Suspend(ctx context.Context, adminID, accountID string) (Result, error) {
	return s.set(ctx, adminID, accountID, true)
}

// Restore lets an account back in and starts its apps again on the image each
// was already serving. It creates no deployment, no release, and no build.
func (s *Service) Restore(ctx context.Context, adminID, accountID string) (Result, error) {
	return s.set(ctx, adminID, accountID, false)
}

// set is both directions. They differ only in the flag written and the replica
// count scaled to, so writing them twice would be two places for one rule.
func (s *Service) set(ctx context.Context, adminID, accountID string, suspended bool) (Result, error) {
	reason := "restore"
	replicas := deploy.ServingReplicas
	if suspended {
		reason = "suspend"
		replicas = 0
	}

	if err := s.lockout.SetDisabled(ctx, accountID, suspended); err != nil {
		return Result{}, fmt.Errorf("suspend: %s account %s: %w", reason, accountID, err)
	}
	auth.Record(ctx, s.auditor, auth.Audit{
		AccountID:  adminID,
		Action:     auth.ActionAdmin,
		TargetType: "account",
		TargetID:   accountID,
		Allowed:    true,
		Reason:     reason,
	})

	apps, err := s.apps.DeployedAppsByAccount(ctx, accountID)
	if err != nil {
		// The lockout landed, so this is not a failed suspension. It is a
		// suspension whose cluster half never started, which the sweep will finish.
		return Result{}, fmt.Errorf("suspend: reading the apps of account %s: %w", accountID, err)
	}

	var result Result
	for _, app := range apps {
		err := s.scale(ctx, app.Slug, replicas)
		auth.Record(ctx, s.auditor, auth.Audit{
			AccountID:  adminID,
			Action:     auth.ActionAdmin,
			TargetType: "app",
			TargetID:   app.ID,
			Allowed:    err == nil,
			Reason:     reason,
		})
		if err != nil {
			slog.ErrorContext(ctx, "scaling an app for an account suspension failed",
				"error", err, "app", app.Slug, "account", accountID, "direction", reason)
			result.NotStopped = append(result.NotStopped, app.Slug)
			continue
		}
		result.Stopped = append(result.Stopped, app.Slug)
	}
	return result, nil
}

// scale writes one app's replica count, composing the namespace and the workload
// name the same way the deploy path does rather than remembering either.
func (s *Service) scale(ctx context.Context, slug string, replicas int32) error {
	if s.cluster == nil {
		return nil
	}
	return s.cluster.ScaleWorkload(ctx, deploy.NamespaceName(slug), deploy.WorkloadName, replicas)
}

// SweepSuspended re-asserts zero replicas for every live app of every suspended
// account. It runs at boot and on each reconcile tick, and it is what makes the
// cluster half of a suspension eventually true rather than merely attempted
// (spec 0018, AC-7).
//
// It only ever scales down. The restore path is the single caller that scales
// anything up, so a bug in here cannot put a suspended account back on the
// network (AC-8). A namespace that refuses is logged and stepped over, leaving
// the rest of the sweep to run.
//
// The account's state is re-read immediately before each write rather than
// trusted from the list: the list is a snapshot, a tick is short, and an admin
// restore can land inside one. Without the re-read the sweep would scale an app
// the restore had just brought back (AC-24).
func (s *Service) SweepSuspended(ctx context.Context) {
	if s.cluster == nil {
		return
	}
	apps, err := s.apps.DeployedAppsOfSuspendedAccounts(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "reading the apps of suspended accounts failed", "error", err)
		return
	}
	for _, app := range apps {
		stillSuspended, err := s.apps.AccountSuspended(ctx, app.AccountID)
		if err != nil {
			slog.ErrorContext(ctx, "re-reading a suspended account failed",
				"error", err, "account", app.AccountID, "app", app.Slug)
			continue
		}
		if !stillSuspended {
			continue
		}
		if err := s.scale(ctx, app.Slug, 0); err != nil {
			slog.ErrorContext(ctx, "holding a suspended account's app at zero failed",
				"error", err, "app", app.Slug, "account", app.AccountID)
		}
	}
}
