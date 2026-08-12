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

// Create registers an app, deriving its permanent slug from the name.
func (a MCPApps) Create(ctx context.Context, accountID, name string) (mcp.App, error) {
	app, err := a.s.CreateApp(ctx, accountID, name)
	if err != nil {
		return mcp.App{}, err
	}
	return appRow(app), nil
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

// Outcome reads a deployment's committed state, which is all the waiting handler
// is ever allowed to look at.
func (a MCPDeployments) Outcome(ctx context.Context, deploymentID string) (mcp.Outcome, error) {
	dep, err := a.s.GetDeployment(ctx, deploymentID)
	if err != nil {
		return mcp.Outcome{}, err
	}
	return mcp.Outcome{
		State:  domain.State(dep.State),
		Reason: domain.Reason(deref(dep.FailureReason)),
	}, nil
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
