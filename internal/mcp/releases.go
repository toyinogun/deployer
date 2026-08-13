package mcp

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// MaxReleaseListing is how many releases list_releases returns. A Go constant
// rather than a DEPLOYER_* variable for the same reason the log bounds are: it
// is a product decision about what fits an agent's context window, not a knob
// for whoever runs the platform (spec 0011, Value sourcing).
const MaxReleaseListing = 20

// listReleasesInput is the tool's whole argument surface. There is no limit and
// no cursor: the bound is the platform's, not the caller's.
type listReleasesInput struct {
	Name string `json:"name" jsonschema:"the app's name"`
}

// releaseRow is one release in the listing.
type releaseRow struct {
	ReleaseNumber int64  `json:"release_number"`
	ImageDigest   string `json:"image_digest"`
	CreatedAt     string `json:"created_at"`
	Current       bool   `json:"current"`
	DeploymentID  string `json:"deployment_id"`
}

// listReleasesOutput is the whole listing. Releases is never null: an app that
// has never been healthy reports an empty list rather than a refusal.
type listReleasesOutput struct {
	Name     string       `json:"name"`
	Releases []releaseRow `json:"releases"`
}

// rollbackInput names the app and the release to go back to. ReleaseNumber is
// required: there is no "the previous one" default, because which release was
// last known good is the caller's judgement, not the platform's.
type rollbackInput struct {
	Name          string `json:"name" jsonschema:"the app's name"`
	ReleaseNumber int64  `json:"release_number" jsonschema:"the release_number to go back to, as list_releases reports it"`
}

// rollbackOutput is what an accepted rollback reports. It carries no release
// number for the release this will mint, because nothing has run yet.
type rollbackOutput struct {
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	URL          string `json:"url"`
	DeploymentID string `json:"deployment_id"`
	State        string `json:"state"`
}

// listReleasesDescription is contract rather than decoration: the bound and its
// consequence are the only place an agent learns that older releases exist and
// are not reachable from here (spec 0011, AC-22).
const listReleasesDescription = `List an app's recent releases, newest first.

A release is one image plus the exact configuration it was running with,
recorded the moment a deploy became healthy. Exactly one row has current true:
the release the app is serving now. An app that has never been healthy has no
releases yet, which is an empty list rather than an error.

This returns at most the newest 20 releases and there is no way to page past
them. Older releases still exist, but no tool reaches them.

Pass a release_number from here to rollback_app to go back to that release. No
configuration values appear in this response; use get_config for those.`

// rollbackDescription carries the three things a caller cannot discover by
// trying: that it does not wait, that it replaces configuration as well as the
// image, and that a set_config landing while it runs is reverted (AC-22, AC-25).
const rollbackDescription = `Roll an app back to one of its earlier releases.

Call list_releases first to see which release_number values exist. This call
returns straight away with a deployment_id and the state "queued"; it does not
wait for the rollout. Call deployment_status with that deployment_id to learn
how the rollback ended.

A rollback replaces the whole app, not just the image. It re promotes that
release's image digest, without building anything, and restores the environment
variables that release was running with: a key that release had comes back with
its value, and a key it did not have is removed. get_config afterwards reports
that release's configuration, so any variable you set since then is gone. A
set_config that lands while the rollback is still running is reverted too, even
though that call returned success.

A rollback supersedes whatever deploy or rollback was already in flight for the
app, cancelling it. Rolling back to the release that is already current is
allowed and re applies it.

Because no build runs, this is fast: it costs a rollout rather than the several
minutes a build takes. A rollback that fails leaves the app's recorded current
release alone, so one bad recovery attempt cannot poison the history.`

// listReleases reads an app's release history. A pure read: it observes and
// never acts.
func (s *Server) listReleases(ctx context.Context, account auth.Account, in listReleasesInput) (*mcp.CallToolResult, listReleasesOutput, error) {
	app, err := s.resolveOwned(ctx, account, in.Name, auth.ActionReleases)
	if err != nil {
		return nil, listReleasesOutput{}, err
	}

	rows, err := s.deployments.ListReleases(ctx, app.ID, MaxReleaseListing)
	if err != nil {
		return nil, listReleasesOutput{}, toolError(auth.ActionReleases, domain.ReasonInternal, err)
	}

	out := listReleasesOutput{Name: app.Name, Releases: make([]releaseRow, 0, len(rows))}
	for _, r := range rows {
		out.Releases = append(out.Releases, releaseRow{
			ReleaseNumber: r.Number,
			ImageDigest:   r.Digest,
			CreatedAt:     r.CreatedAt,
			// The app was read for the ownership check and carries the pointer, so
			// nothing extra is read to decide this. An app that has never been
			// healthy has an empty pointer, which no release id can equal.
			Current:      app.CurrentReleaseID != "" && r.ID == app.CurrentReleaseID,
			DeploymentID: r.DeploymentID,
		})
	}
	return nil, out, nil
}

// rollback records a rollback and reports it queued. Like deploy_app it does not
// wait: the deployment is the reconcile loop's from the moment the row is
// written.
func (s *Server) rollback(ctx context.Context, account auth.Account, in rollbackInput) (*mcp.CallToolResult, rollbackOutput, error) {
	// Ownership before anything else, so a caller cannot probe which release
	// numbers exist on somebody else's app (AC-8).
	app, err := s.resolveOwned(ctx, account, in.Name, auth.ActionRollback)
	if err != nil {
		return nil, rollbackOutput{}, err
	}

	// A number that could never have been minted is refused here rather than
	// asked of the store, and reads the same as one that simply does not exist.
	if in.ReleaseNumber < 1 {
		return nil, rollbackOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionRollback,
			domain.ReasonReleaseUnknown, errors.New("release_number must be a positive integer"))
	}
	releaseID, err := s.deployments.ReleaseIDByNumber(ctx, app.ID, in.ReleaseNumber)
	if errors.Is(err, ErrNoRelease) {
		return nil, rollbackOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionRollback,
			domain.ReasonReleaseUnknown, err)
	}
	if err != nil {
		return nil, rollbackOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionRollback,
			domain.ReasonInternal, err)
	}

	deploymentID, err := s.deployments.CreateRollback(ctx, app.ID, account.ID, releaseID)
	if err != nil {
		return nil, rollbackOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionRollback,
			domain.ReasonInternal, err)
	}
	auth.Record(ctx, s.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionRollback,
		TargetType: "app", TargetID: app.ID, Allowed: true,
	})

	// The row was just written queued and nothing is read back, so the state is
	// the constant rather than a read.
	return nil, rollbackOutput{
		Name:         app.Name,
		Slug:         app.Slug,
		URL:          s.appURL(app.Slug),
		DeploymentID: deploymentID,
		State:        string(domain.StateQueued),
	}, nil
}
