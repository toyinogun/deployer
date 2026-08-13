package mcp

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// MaxAppListing is how many apps list_apps returns. A Go constant rather than a
// DEPLOYER_* variable for the same reason MaxReleaseListing is one: it is a
// product decision about what fits an agent's context window, not a knob for
// whoever runs the platform (spec 0012, Value sourcing).
const MaxAppListing = 50

// ErrAppInFlight means a deploy or rollback is still running for the app, which
// refuses a delete whole: nothing is written and nothing is torn down
// (spec 0012, AC-15).
var ErrAppInFlight = errors.New("mcp: the app has a deployment in flight")

// AppSummary is one row of the app listing, already projected: what the app is
// serving and how its last deploy ended, as two independent facts (AC-5).
//
// The absent cases are zero values, because that is what the query behind it
// projects: ServingRelease is zero for an app that has never been healthy,
// LastDeploymentID is empty for one never deployed, and LastDeployedAt is empty
// until something has finished. No configuration field exists here on purpose.
type AppSummary struct {
	Name                 string
	Slug                 string
	CreatedAt            string
	ServingRelease       int64
	LastDeploymentID     string
	LastDeploymentState  string
	LastDeploymentReason string
	LastDeployedAt       string
}

// Cluster is the one method of Kubernetes this package needs. It exists because
// a delete is the first tool action that has to reach the cluster at all:
// everything else here writes a row and lets the reconcile loop act
// (spec 0012, Consequences).
type Cluster interface {
	// DeleteNamespace tears an app's whole namespace down by slug. A namespace
	// that is already gone, or already terminating, is success rather than a
	// fault, decided at the edge so no handler here reads a Kubernetes error.
	DeleteNamespace(ctx context.Context, slug string) error
}

// listAppsInput is the tool's whole argument surface, which is nothing at all:
// the account comes from the token, and the bound is the platform's.
type listAppsInput struct{}

// servingOut is what the app is serving right now. It rides as a pointer on the
// row, so an app that has never been healthy has no serving object rather than
// release zero (AC-3).
type servingOut struct {
	ReleaseNumber int64 `json:"release_number"`
}

// lastDeploymentOut is how the app's most recent deploy ended, or that it has
// not ended: an in flight one reports its live state here (AC-4).
type lastDeploymentOut struct {
	DeploymentID string `json:"deployment_id"`
	State        string `json:"state"`
	// Reason is present only on a failed or cancelled deployment, because that
	// is the only case that has one.
	Reason string `json:"reason,omitempty"`
}

// appRowOut is one app in the listing. serving and last_deployment are separate
// objects and either can be present without the other: the pair is the honest
// answer, and nothing here collapses them into one state (AC-5).
type appRowOut struct {
	Name           string             `json:"name"`
	Slug           string             `json:"slug"`
	URL            string             `json:"url"`
	CreatedAt      string             `json:"created_at"`
	LastDeployedAt string             `json:"last_deployed_at,omitempty"`
	Serving        *servingOut        `json:"serving,omitempty"`
	LastDeployment *lastDeploymentOut `json:"last_deployment,omitempty"`
}

// listAppsOutput is the whole listing. Apps is never null: an account with no
// apps gets an empty list rather than a refusal (AC-9).
type listAppsOutput struct {
	Apps []appRowOut `json:"apps"`
}

// deleteAppInput names the app to tear down. There is no confirmation flag and
// no slug: the name is what every other tool is addressed by.
type deleteAppInput struct {
	Name string `json:"name" jsonschema:"the app's name, the same one deploy_app was given"`
}

// deleteAppOutput is what a completed delete reports.
type deleteAppOutput struct {
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	Deleted bool   `json:"deleted"`
}

// listAppsDescription is contract rather than decoration: the bound and the two
// fact shape are what a caller cannot discover by trying (AC-12).
const listAppsDescription = `List the apps you have deployed, newest first.

Each app reports two separate facts, and reading them as one is the mistake this
listing exists to prevent. serving is the release the app is running right now,
and last_deployment is how your most recent deploy ended. An app whose last
deploy failed is usually still serving its previous release perfectly well, so a
row with serving set and a failed last_deployment means the app is up and your
new version is not.

An app that has never had a healthy deploy has no serving object. An app that
has never been deployed at all has no last_deployment object. A deploy still
running appears in last_deployment with its live state, such as "building".

The url is the app's permanent address, the same one deploy_app reports.

This returns at most the newest 50 apps and there is no way to page past them.
Older apps still exist, but no tool reaches them.

No environment variables appear here; use get_config for those.`

// deleteAppDescription carries what a caller cannot discover by trying: that it
// cannot be undone, that the hostname is gone for good, that it does not wait,
// and what it deliberately keeps (AC-30).
const deleteAppDescription = `Delete an app and release everything it holds on the cluster.

This cannot be undone. The app's workload, its route, its network policies, its
quota and its whole namespace are deleted together, and the app stops serving.

The hostname is never reused. Creating an app with the same name afterwards
gives you a new app with a new slug and so a new address, and any link you
handed out is dead for good.

The call does not wait for the cluster to finish tearing the namespace down. It
returns once the delete is recorded and issued.

A deploy or rollback still in flight refuses the whole call with
deployment_in_flight. Wait for it to finish, then delete.

Release history and configuration rows are kept rather than purged, so the
record of what ran outlives the app. Nothing reads them back: every tool answers
app_unknown for a deleted app.`

// listApps reads the caller's apps. A pure read: it observes, never acts, and
// makes no Kubernetes call at all (AC-8).
func (s *Server) listApps(ctx context.Context, account auth.Account, _ listAppsInput) (*mcp.CallToolResult, listAppsOutput, error) {
	// Scoped to the calling account for every caller, is_admin included: no MCP
	// tool widens on the admin flag (AC-10).
	rows, err := s.apps.ListSummaries(ctx, account.ID, MaxAppListing)
	if err != nil {
		return nil, listAppsOutput{}, s.denyConfig(ctx, account.ID, "", auth.ActionAppList, domain.ReasonInternal, err)
	}

	out := listAppsOutput{Apps: make([]appRowOut, 0, len(rows))}
	for _, a := range rows {
		row := appRowOut{
			Name:           a.Name,
			Slug:           a.Slug,
			URL:            s.appURL(a.Slug),
			CreatedAt:      a.CreatedAt,
			LastDeployedAt: a.LastDeployedAt,
		}
		if a.ServingRelease > 0 {
			row.Serving = &servingOut{ReleaseNumber: a.ServingRelease}
		}
		if a.LastDeploymentID != "" {
			last := &lastDeploymentOut{DeploymentID: a.LastDeploymentID, State: a.LastDeploymentState}
			// Only an ended deployment carries a reason, and only these two ends
			// have one worth reporting.
			if state := domain.State(a.LastDeploymentState); state == domain.StateFailed || state == domain.StateCancelled {
				last.Reason = a.LastDeploymentReason
			}
			row.LastDeployment = last
		}
		out.Apps = append(out.Apps, row)
	}
	return nil, out, nil
}

// deleteApp retires an app and tears its namespace down, in that order: the
// database write first and the cluster call second, so there is no path that
// deletes a namespace for an app whose row is still live (AC-14).
func (s *Server) deleteApp(ctx context.Context, account auth.Account, in deleteAppInput) (*mcp.CallToolResult, deleteAppOutput, error) {
	app, err := s.resolveOwned(ctx, account, in.Name, auth.ActionAppDelete)
	if err != nil {
		return nil, deleteAppOutput{}, err
	}

	switch err := s.apps.Delete(ctx, app.ID); {
	case err == nil:
	case errors.Is(err, ErrAppInFlight):
		// Decided inside the same transaction that would have written deleted_at,
		// so nothing was written and nothing is torn down (AC-15).
		return nil, deleteAppOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionAppDelete,
			domain.ReasonDeploymentInFlight, err)
	case errors.Is(err, ErrNoApp):
		// A second concurrent delete updates no row. It reads exactly like a name
		// that never existed, which is what makes the two indistinguishable
		// (AC-20). No app was retired, so the row carries no target.
		return nil, deleteAppOutput{}, s.denyConfig(ctx, account.ID, "", auth.ActionAppDelete,
			domain.ReasonAppUnknown, err)
	default:
		return nil, deleteAppOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionAppDelete,
			domain.ReasonInternal, err)
	}

	// A local run with no cluster credential reads the same way a namespace
	// delete that failed does: the row is gone, the teardown did not happen, and
	// the caller is not told it did.
	if s.cluster == nil {
		return nil, deleteAppOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionAppDelete,
			domain.ReasonInternal, errors.New("no cluster access, so the app's namespace was left in place"))
	}

	// One call, and Kubernetes cascades everything inside the namespace. Gone and
	// terminating are both success, decided in internal/kube so no handler here
	// reads a Kubernetes error (AC-16, AC-18).
	if err := s.cluster.DeleteNamespace(ctx, app.Slug); err != nil {
		// The row is deleted and the namespace is not, which the reaper finishes.
		// The caller is told internal, because the teardown this call promised did
		// not happen (AC-19).
		return nil, deleteAppOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionAppDelete,
			domain.ReasonInternal, err)
	}

	auth.Record(ctx, s.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionAppDelete,
		TargetType: "app", TargetID: app.ID, Allowed: true,
	})
	return nil, deleteAppOutput{Name: app.Name, Slug: app.Slug, Deleted: true}, nil
}
