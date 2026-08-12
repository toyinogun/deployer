package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// statusDescription is contract rather than decoration, the same as deploy_app's:
// it is where an agent learns that polling is the intended shape, roughly how
// long to expect to poll for, and what a cancelled result means
// (spec 0005, API surface).
const statusDescription = `Report how a deployment is going, and how it got there.

Give exactly one of deployment_id (from deploy_app) or name (the app's name,
which reports that app's most recent deployment).

A deploy walks queued, building, pushing, deploying, healthy. Poll this every
few seconds until the state is healthy, failed, or cancelled. A first build
takes a few minutes; nothing is being missed while the state stays queued,
which only means another build is ahead of it.

healthy carries the release_number and image_digest that are now serving.
failed carries a reason code and one line saying what to change.
cancelled means a later deploy of the same app replaced this one, and
superseded_by names it: poll that deployment instead.`

// statusInput is the whole argument surface. Both are optional in the schema
// because exactly one is given, which is a rule no schema can express.
type statusInput struct {
	DeploymentID string `json:"deployment_id,omitempty" jsonschema:"the id deploy_app returned; give this or name, not both"`
	Name         string `json:"name,omitempty" jsonschema:"the app's name, reporting its most recent deployment; give this or deployment_id, not both"`
}

// timelineEntry is one recorded state change. It is the projection the events
// table's internal detail column has no field to arrive in (AC-8).
type timelineEntry struct {
	State  string `json:"state"`
	At     string `json:"at"`
	Reason string `json:"reason,omitempty"`
}

// statusOutput varies by state on purpose: an agent reads what is true now
// rather than a fixed shape of mostly nulls.
type statusOutput struct {
	DeploymentID  string          `json:"deployment_id"`
	AppName       string          `json:"app_name"`
	Slug          string          `json:"slug"`
	URL           string          `json:"url"`
	State         string          `json:"state"`
	ReleaseNumber int64           `json:"release_number,omitempty"`
	ImageDigest   string          `json:"image_digest,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Message       string          `json:"message,omitempty"`
	SupersededBy  string          `json:"superseded_by,omitempty"`
	Timeline      []timelineEntry `json:"timeline"`
}

// status resolves the deployment the caller asked about, scoped to their
// account, and projects it. It writes no audit row unless it refuses (AC-10).
func (s *Server) status(ctx context.Context, account auth.Account, in statusInput) (*mcp.CallToolResult, statusOutput, error) {
	if (in.DeploymentID == "") == (in.Name == "") {
		// Neither or both, which is refused before anything is read (AC-5).
		return nil, statusOutput{}, s.denyStatus(ctx, account.ID,
			errors.New("give exactly one of deployment_id and name"))
	}

	dep, err := s.resolveDeployment(ctx, account, in)
	switch {
	case errors.Is(err, ErrNoDeployment), errors.Is(err, ErrNoApp):
		// The only outcomes that are an access decision, and so the only ones
		// audited as a refusal (AC-9, AC-10).
		return nil, statusOutput{}, s.denyStatus(ctx, account.ID, err)
	case err != nil:
		// A fault is not a refusal: reporting it as unknown would tell a polling
		// agent its id is wrong and write a denial that never happened.
		return nil, statusOutput{}, toolError(auth.ActionStatus, domain.ReasonInternal, err)
	}
	app, err := s.apps.Get(ctx, dep.AppID)
	if err != nil {
		return nil, statusOutput{}, toolError(auth.ActionStatus, domain.ReasonInternal,
			fmt.Errorf("reading the app of deployment %s: %w", dep.ID, err))
	}

	out, err := s.project(ctx, dep, app)
	if err != nil {
		return nil, statusOutput{}, toolError(auth.ActionStatus, domain.ReasonInternal, err)
	}
	return nil, out, nil
}

// resolveDeployment finds the deployment either way in, and refuses one the
// caller does not own in the same words as one that does not exist, so status
// cannot be used to learn which ids exist (AC-9).
func (s *Server) resolveDeployment(ctx context.Context, account auth.Account, in statusInput) (Deployment, error) {
	if in.DeploymentID != "" {
		dep, err := s.deployments.Get(ctx, in.DeploymentID)
		if err != nil {
			return Deployment{}, fmt.Errorf("reading deployment %s: %w", in.DeploymentID, err)
		}
		// Scoped before a single field is projected.
		if dep.AccountID != account.ID {
			return Deployment{}, fmt.Errorf("deployment %s belongs to another account: %w", dep.ID, ErrNoDeployment)
		}
		return dep, nil
	}

	app, err := s.apps.ByName(ctx, account.ID, in.Name)
	if err != nil {
		return Deployment{}, fmt.Errorf("reading app %q: %w", in.Name, err)
	}
	dep, err := s.deployments.LatestForApp(ctx, app.ID)
	if err != nil {
		return Deployment{}, fmt.Errorf("reading the latest deployment of app %s: %w", app.ID, err)
	}
	return dep, nil
}

// project assembles the payload for the state the deployment is actually in.
func (s *Server) project(ctx context.Context, dep Deployment, app App) (statusOutput, error) {
	events, err := s.deployments.Events(ctx, dep.ID)
	if err != nil {
		return statusOutput{}, fmt.Errorf("reading the timeline of %s: %w", dep.ID, err)
	}
	timeline := make([]timelineEntry, 0, len(events))
	for _, e := range events {
		timeline = append(timeline, timelineEntry{
			State:  string(e.State),
			At:     e.At,
			Reason: string(e.Reason),
		})
	}

	out := statusOutput{
		DeploymentID: dep.ID,
		AppName:      app.Name,
		Slug:         app.Slug,
		URL:          s.appURL(app.Slug),
		State:        string(dep.State),
		Timeline:     timeline,
	}

	switch dep.State {
	case domain.StateHealthy:
		release, err := s.deployments.Release(ctx, dep.ID)
		if err != nil {
			return statusOutput{}, fmt.Errorf("reading the release of %s: %w", dep.ID, err)
		}
		out.ReleaseNumber, out.ImageDigest = release.Number, release.Digest
	case domain.StateFailed:
		out.Reason, out.Message = string(dep.Reason), dep.Reason.Message()
	case domain.StateCancelled:
		out.Reason = string(domain.ReasonSuperseded)
		// Derived, never stored: the app's next deployment by id. Empty when the
		// superseding row is not visible yet, which the next poll resolves.
		next, err := s.deployments.NextForApp(ctx, dep.AppID, dep.ID)
		if err != nil {
			return statusOutput{}, fmt.Errorf("reading what superseded %s: %w", dep.ID, err)
		}
		out.SupersededBy = next
	}
	return out, nil
}

// denyStatus records the refusal and gives back the one answer every refused
// status read gets, whatever it was that did not resolve.
func (s *Server) denyStatus(ctx context.Context, accountID string, cause error) error {
	auth.Record(ctx, s.auditor, auth.Audit{
		AccountID: accountID,
		Action:    auth.ActionStatus,
		Reason:    string(domain.ReasonDeploymentUnknown),
	})
	return toolError(auth.ActionStatus, domain.ReasonDeploymentUnknown, cause)
}
