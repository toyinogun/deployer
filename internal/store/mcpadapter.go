package store

import (
	"context"
	"errors"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/mcp"
)

// The store's two tool facing views. They are separate types because both
// interfaces name a Create, and because a tool that only reads apps has no
// business holding a handle that writes deployments.
type (
	// MCPApps is the app lookup and creation internal/mcp declares.
	MCPApps struct{ s *Store }
	// MCPDeployments is the deployment write and the waiting reads.
	MCPDeployments struct{ s *Store }
)

// ForMCPApps returns the app facing view of the store.
func ForMCPApps(s *Store) MCPApps { return MCPApps{s: s} }

// ForMCPDeployments returns the deployment facing view of the store.
func ForMCPDeployments(s *Store) MCPDeployments { return MCPDeployments{s: s} }

// Compile time proof that the adapters are what internal/mcp asked for.
var (
	_ mcp.Apps        = MCPApps{}
	_ mcp.Deployments = MCPDeployments{}
)

// appRow maps a stored app onto the fields a tool response reads.
func appRow(a App) mcp.App {
	return mcp.App{ID: a.ID, Slug: a.Slug, Name: a.Name}
}

// ByName returns the account's app under that name, or mcp.ErrNoApp.
func (a MCPApps) ByName(ctx context.Context, accountID, name string) (mcp.App, error) {
	app, err := a.s.GetAppByName(ctx, accountID, name)
	if errors.Is(err, ErrNotFound) {
		return mcp.App{}, mcp.ErrNoApp
	}
	if err != nil {
		return mcp.App{}, err
	}
	return appRow(app), nil
}

// Get reads the app a deployment belongs to, which is where a status payload's
// name, slug, and url come from.
func (a MCPApps) Get(ctx context.Context, appID string) (mcp.App, error) {
	app, err := a.s.GetApp(ctx, appID)
	if err != nil {
		return mcp.App{}, err
	}
	return appRow(app), nil
}

// Create registers an app, deriving its permanent slug from the name.
func (a MCPApps) Create(ctx context.Context, accountID, name string) (mcp.App, error) {
	app, err := a.s.CreateApp(ctx, accountID, name)
	if err != nil {
		return mcp.App{}, err
	}
	return appRow(app), nil
}

// Config lists the app's keys the way a response may see them: a secret key
// comes back without its value, which the query decides rather than this
// adapter (spec 0010, AC-2).
func (a MCPApps) Config(ctx context.Context, appID string) ([]mcp.ConfigEntry, error) {
	entries, err := a.s.ListConfigForResponse(ctx, appID)
	if err != nil {
		return nil, err
	}
	return configRows(entries), nil
}

// ConfigValues lists the app's keys with their real values. Its callers are the
// size bounds and the log redaction, neither of which puts what it read into a
// response.
func (a MCPApps) ConfigValues(ctx context.Context, appID string) ([]mcp.ConfigEntry, error) {
	entries, err := a.s.ListConfigForDeploy(ctx, appID)
	if err != nil {
		return nil, err
	}
	return configRows(entries), nil
}

// SetConfig writes every entry or none of them.
func (a MCPApps) SetConfig(ctx context.Context, appID string, entries []mcp.ConfigEntry) error {
	rows := make([]ConfigEntry, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, ConfigEntry{Key: e.Key, Value: e.Value, IsSecret: e.Secret})
	}
	return a.s.SetConfigBatch(ctx, appID, rows)
}

// UnsetConfig removes every key or none of them, mapping a key that is not set
// onto the tool surface's own sentinel.
func (a MCPApps) UnsetConfig(ctx context.Context, appID string, keys []string) error {
	err := a.s.UnsetConfigBatch(ctx, appID, keys)
	if errors.Is(err, ErrNotFound) {
		return mcp.ErrNoConfigKey
	}
	return err
}

// ReleaseConfig reads what the app's current release ran with, which is how a
// value the running pod already printed is still blanked after it was rotated
// (spec 0010, AC-11).
func (a MCPApps) ReleaseConfig(ctx context.Context, appID string) (map[string]string, error) {
	return a.s.CurrentReleaseConfig(ctx, appID)
}

// configRows maps stored configuration onto what the tool surface reads.
func configRows(entries []ConfigEntry) []mcp.ConfigEntry {
	out := make([]mcp.ConfigEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, mcp.ConfigEntry{Key: e.Key, Value: e.Value, Secret: e.IsSecret})
	}
	return out
}

// Create writes a queued deployment. Anything already in flight for the app is
// superseded inside the same transaction, which is the store's rule, not a
// decision this adapter makes.
func (a MCPDeployments) Create(ctx context.Context, appID, accountID, uploadID string) (string, error) {
	dep, _, err := a.s.CreateDeployment(ctx, CreateDeploymentInput{
		AppID:     appID,
		AccountID: accountID,
		UploadID:  &uploadID,
	})
	if err != nil {
		return "", err
	}
	return dep.ID, nil
}

// mcpDeploymentRow maps a stored deployment onto what a status read projects.
func mcpDeploymentRow(d Deployment) mcp.Deployment {
	return mcp.Deployment{
		ID:        d.ID,
		AppID:     d.AppID,
		AccountID: d.AccountID,
		State:     domain.State(d.State),
		Reason:    domain.Reason(deref(d.FailureReason)),
		BuildPath: deref(d.BuildPath),
	}
}

// Get reads one deployment, mapping an unknown id onto the tool surface's own
// sentinel so the handler answers unknown and forbidden identically.
func (a MCPDeployments) Get(ctx context.Context, deploymentID string) (mcp.Deployment, error) {
	dep, err := a.s.GetDeployment(ctx, deploymentID)
	if errors.Is(err, ErrNotFound) {
		return mcp.Deployment{}, mcp.ErrNoDeployment
	}
	if err != nil {
		return mcp.Deployment{}, err
	}
	return mcpDeploymentRow(dep), nil
}

// LatestForApp reads an app's most recent deployment, which is what a status
// read by name reports. An app that has never been deployed reads as unknown.
func (a MCPDeployments) LatestForApp(ctx context.Context, appID string) (mcp.Deployment, error) {
	dep, err := a.s.GetLatestDeploymentForApp(ctx, appID)
	if errors.Is(err, ErrNotFound) {
		return mcp.Deployment{}, mcp.ErrNoDeployment
	}
	if err != nil {
		return mcp.Deployment{}, err
	}
	return mcpDeploymentRow(dep), nil
}

// NextForApp returns the id of the app's next deployment after this one, which
// is what superseded_by is derived from. No later deployment yet is an empty
// string rather than an error: the next poll resolves it.
func (a MCPDeployments) NextForApp(ctx context.Context, appID, after string) (string, error) {
	dep, err := a.s.GetNextDeploymentForApp(ctx, appID, after)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return dep.ID, nil
}

// Events returns a deployment's timeline, projected here rather than in the
// caller: detail and from_state are dropped at this boundary, so no write site
// can leak through them (spec 0005, AC-8).
func (a MCPDeployments) Events(ctx context.Context, deploymentID string) ([]mcp.Event, error) {
	rows, err := a.s.ListDeploymentEvents(ctx, deploymentID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.Event, 0, len(rows))
	for _, e := range rows {
		out = append(out, mcp.Event{
			State:  domain.State(e.ToState),
			At:     e.OccurredAt,
			Reason: domain.Reason(deref(e.Reason)),
		})
	}
	return out, nil
}

// Release reads the release a healthy deployment minted, so the response carries
// the number and digest that were written rather than recomputed ones.
func (a MCPDeployments) Release(ctx context.Context, deploymentID string) (mcp.Release, error) {
	rel, err := a.s.GetReleaseByDeployment(ctx, deploymentID)
	if err != nil {
		return mcp.Release{}, err
	}
	return mcp.Release{Number: rel.ReleaseNumber, Digest: rel.ImageDigest}, nil
}
