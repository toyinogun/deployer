// Package mcp is the agent facing tool surface: deploy_app and
// deployment_status, served over the streamable HTTP transport beside the upload
// endpoint.
//
// The handler observes and never acts. deploy_app resolves the upload, resolves
// or creates the app, writes a queued deployment, and returns without reading
// anything back; deployment_status is a pure read. rollback_app is the same
// shape as deploy_app: it resolves the release a caller named, writes a queued
// deployment, and returns. Everything in between is the reconcile loop's
// (spec 0004, Key invariants; spec 0005, AC-3).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/logs"
	"github.com/toyinogun/deployer/internal/uploads"
)

// ErrNoApp means the account has no app under that name yet, which is the first
// deploy rather than a failure.
var ErrNoApp = errors.New("mcp: no such app")

// ErrAppLimit means the account already holds as many live apps as it may, so
// the app the deploy would have created was not created. It reaches this
// package from the transaction that would have inserted the row, which is the
// only place the answer is exact (spec 0016, AC-6).
var ErrAppLimit = errors.New("mcp: app limit reached")

// ErrAppNameReserved means the name derives to a hostname label the platform
// keeps for itself, so no app row was created. It reaches this package from the
// same call that decides the cap, which is what keeps a second create path from
// bypassing either (spec 0021, AC-6).
var ErrAppNameReserved = errors.New("mcp: app name reserved")

// ErrNoUpload means the upload id resolved to nothing.
var ErrNoUpload = errors.New("mcp: no such upload")

// ErrNoDeployment means the deployment id or the app's deployment history
// resolved to nothing. A deployment belonging to another account is refused with
// the same answer, so status cannot be used to learn which ids exist
// (spec 0005, AC-9).
var ErrNoDeployment = errors.New("mcp: no such deployment")

// App is one deployed application, carrying only what a response reads.
type App struct {
	ID   string
	Slug string
	Name string
	// CurrentReleaseID is the release the app is serving, empty when it has
	// never been healthy. It is the only source for the listing's current flag,
	// and it rides along on the app the ownership check already read, so marking
	// the current release costs no extra query (spec 0011, AC-2).
	CurrentReleaseID string
}

// Upload is what the handler checks before it touches an app.
type Upload struct {
	ID        string
	AccountID string
	ExpiresAt string
	Redeemed  bool
}

// Deployment is one deployment as a status read sees it. AccountID is carried so
// the scope check happens here, before any field is projected.
type Deployment struct {
	ID        string
	AppID     string
	AccountID string
	State     domain.State
	Reason    domain.Reason
	// BuildPath is which engine built this deployment, "buildpacks" or
	// "dockerfile". Empty until the build starts, since the row has no path
	// before one was chosen (spec 0009, AC-5).
	BuildPath string
}

// Event is one entry of a deployment's timeline, already projected: the events
// table's detail column has no field to arrive in (spec 0005, AC-8).
type Event struct {
	State  domain.State
	At     string
	Reason domain.Reason
}

// Release is the release a healthy deployment minted.
type Release struct {
	Number int64
	Digest string
}

// ErrNoRelease means the app has no release with that number. It is only ever
// reached on an app the caller owns, because ownership is decided first.
var ErrNoRelease = errors.New("mcp: no such release")

// ReleaseSummary is one release as the listing reports it. It has no
// configuration field on purpose: the snapshot never enters this package.
type ReleaseSummary struct {
	ID           string
	Number       int64
	Digest       string
	DeploymentID string
	CreatedAt    string
}

// Apps is the slice of persistence this package needs for the get or create, and
// for the configuration an app carries.
type Apps interface {
	// ByName returns the account's app under that name, or ErrNoApp.
	ByName(ctx context.Context, accountID, name string) (App, error)
	// Get reads the app a deployment belongs to.
	Get(ctx context.Context, appID string) (App, error)
	// Create registers an app, deriving its permanent slug from the name. limit
	// is how many live apps the account may hold; a create that would take it
	// past that answers ErrAppLimit and writes nothing.
	Create(ctx context.Context, accountID, name string, limit int) (App, error)
	// Count is how many live apps the account holds right now.
	Count(ctx context.Context, accountID string) (int, error)
	// Config is the response shaped read: a secret key comes back with its flag
	// and no value, decided in SQL rather than here, so no code path in this
	// package can forget (spec 0010, AC-2).
	Config(ctx context.Context, appID string) ([]ConfigEntry, error)
	// ConfigValues is the full read, secret values included. Only the size
	// bounds and the log redaction may call it, and neither puts what it read in
	// a response.
	ConfigValues(ctx context.Context, appID string) ([]ConfigEntry, error)
	// SetConfig writes every entry or none of them.
	SetConfig(ctx context.Context, appID string, entries []ConfigEntry) error
	// UnsetConfig removes every key or none of them, answering ErrNoConfigKey
	// when one of them is not set.
	UnsetConfig(ctx context.Context, appID string, keys []string) error
	// ReleaseConfig is the configuration the app's current release actually ran
	// with. An app that has never run has an empty one rather than an error.
	ReleaseConfig(ctx context.Context, appID string) (map[string]string, error)
	// ListSummaries reads the account's live apps, newest first, at most limit
	// of them, in one statement. What it returns carries no configuration, and
	// the query behind it names no configuration column (spec 0012, AC-7, AC-8).
	ListSummaries(ctx context.Context, accountID string, limit int64) ([]AppSummary, error)
	// Delete retires the app, answering ErrAppInFlight when a deployment is
	// still running for it and ErrNoApp when no live row was updated.
	Delete(ctx context.Context, appID string) error
}

// Deployments is the slice of persistence this package needs.
type Deployments interface {
	// Create writes a queued deployment, superseding anything in flight.
	Create(ctx context.Context, appID, accountID, uploadID string) (string, error)
	// Get reads one deployment, or ErrNoDeployment.
	Get(ctx context.Context, deploymentID string) (Deployment, error)
	// LatestForApp reads an app's most recent deployment by created_at, or
	// ErrNoDeployment when the app has never been deployed.
	LatestForApp(ctx context.Context, appID string) (Deployment, error)
	// NextForApp returns the id of the app's next deployment after this one,
	// ordered by id, or an empty string when there is none.
	NextForApp(ctx context.Context, appID, after string) (string, error)
	// Events returns a deployment's timeline in occurred_at order.
	Events(ctx context.Context, deploymentID string) ([]Event, error)
	// Release reads the release a healthy deployment minted.
	Release(ctx context.Context, deploymentID string) (Release, error)
	// CreateRollback writes a queued rollback deployment, superseding anything
	// in flight exactly as Create does. The digest is copied from the source
	// release by the store, because no build will run to fill it in.
	CreateRollback(ctx context.Context, appID, accountID, releaseID string) (string, error)
	// ReleaseIDByNumber resolves the per app number a caller named to the id the
	// deployment records, or ErrNoRelease. The caller never sends an id.
	ReleaseIDByNumber(ctx context.Context, appID string, number int64) (string, error)
	// ListReleases reads an app's newest releases, newest first, at most limit
	// of them. What it returns carries no configuration, and the query behind it
	// never selects the snapshot (spec 0011, AC-4).
	ListReleases(ctx context.Context, appID string, limit int64) ([]ReleaseSummary, error)
}

// Uploads reads the tarball a deploy names, and accepts one composed from the
// files a deploy carried inline.
type Uploads interface {
	Get(ctx context.Context, id string) (Upload, error)
	// Accept records a tarball under the account, applying the same size and
	// unclaimed caps the upload endpoint does.
	Accept(ctx context.Context, accountID string, body io.Reader) (Upload, error)
}

// Pods is the narrow slice of the cluster a log read needs. It is stated here,
// in the package that uses it, and satisfied at the edge by internal/kube, so
// nothing in this package ever sees a Kubernetes client (spec 0006, Build plan).
type Pods interface {
	// PodsForApp lists the app's own pods, newest first, with the status the
	// empty case is decided from.
	PodsForApp(ctx context.Context, namespace, slug string) ([]logs.PodStatus, error)
	// PodLog reads one container's tail with the kubelet's timestamps, either
	// the running container or the previous one it replaced.
	PodLog(ctx context.Context, namespace, pod string, tailLines int, previous bool) (string, error)
}

// Options is what the tool surface needs from configuration.
type Options struct {
	// MCPURL is the deploy path's public base address, derived from
	// DEPLOYER_MCP_HOST. It is the endpoint deploy_app's description tells an
	// agent to upload to, so a wrong value is an agent that cannot deploy
	// (spec 0022, AC-10).
	MCPURL    string
	AppDomain string
	// MaxUploadBytes is the ceiling the upload endpoint refuses a body over. It
	// is stated in the description rather than written there as a literal, so
	// the number an agent is told and the number the platform enforces are the
	// same one (spec 0022, AC-10).
	MaxUploadBytes int64
	// TrustedHosts are the platform's public hostnames, the ones
	// CF-Connecting-IP is read on. The same set every other surface holds
	// (spec 0022, AC-13, AC-14).
	TrustedHosts []string
	// Limiter is the deploy path's own token bucket, shared with the upload
	// endpoint so a caller spends from one bucket across both routes, and kept
	// apart from the sign in one so a burst here locks nobody out of the console
	// (spec 0022, AC-15).
	Limiter *identity.Limiter
	// MaxAppsPerAccount is how many live apps one account may hold. A deploy of
	// a name the account does not already have is refused once it is there
	// (spec 0016, AC-1).
	MaxAppsPerAccount int
	// SecretLiterals are values the platform itself placed in an app's namespace
	// and so knows for certain are secret, today the registry pull credential.
	// They are the only redaction that can be exact (spec 0006, AC-6).
	SecretLiterals []string
}

// Server is the MCP surface.
type Server struct {
	auth        *auth.Authenticator
	auditor     auth.Auditor
	apps        Apps
	deployments Deployments
	uploads     Uploads
	pods        Pods
	cluster     Cluster
	opts        Options
	// limiter is the deploy path's token bucket, never nil. New falls back to a
	// private one, so a caller that passed none is still bounded rather than
	// unbounded.
	limiter *identity.Limiter
}

// New returns the MCP surface. pods and cluster may both be nil, which is a
// local run with no cluster: get_logs then fails as internal rather than
// pretending an app printed nothing, and delete_app fails the same way rather
// than reporting a teardown that did not happen.
func New(a *auth.Authenticator, auditor auth.Auditor, apps Apps, d Deployments, u Uploads, pods Pods, cluster Cluster, opts Options) *Server {
	limiter := opts.Limiter
	if limiter == nil {
		// A private bucket, shared with nothing. Production always passes the same
		// instance here and to the upload route, so one caller's burst is one
		// budget; a test that relies on this fallback is bounded but holds no
		// shared budget, and proves nothing about AC-15. The lockout is unaffected
		// either way: it lives in auth.Authenticator, not here.
		limiter = identity.NewLimiter(ids.SystemClock{}, identity.DeployPathSettings())
	}
	return &Server{auth: a, auditor: auditor, apps: apps, deployments: d, uploads: u,
		pods: pods, cluster: cluster, opts: opts, limiter: limiter}
}

// deployInput is the tool's whole argument surface.
type deployInput struct {
	Name     string `json:"name" jsonschema:"the app's name, which fixes its hostname for good; reuse it to redeploy the same app"`
	UploadID string `json:"upload_id,omitempty" jsonschema:"the id returned by POST /v1/uploads; give this or files, never both"`
	// Files is the same source as an upload, carried inline, for a caller with no
	// shell to run the upload with. It is text only and it is packed here rather
	// than by the caller, so a client that cannot make an HTTP request or build a
	// tarball can still deploy (spec 0026).
	Files map[string]string `json:"files,omitempty" jsonschema:"the app's source as a map of path relative to the app root to file content; give this or upload_id, never both"`
	// Config is optional, and follows exactly set_config's rules. It is here so a
	// brand new app's first run is not a guaranteed crash on a missing variable
	// (spec 0010, AC-9).
	Config map[string]configValue `json:"config,omitempty" jsonschema:"optional environment variables to set before this deploy, each with a value and a required secret flag; omit it to leave the app's configuration untouched"`
}

// deployOutput is what an accepted deploy reports. It carries no release number
// and no digest, because the deploy has not been built yet (spec 0005, AC-2).
type deployOutput struct {
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	URL          string `json:"url"`
	DeploymentID string `json:"deployment_id"`
	State        string `json:"state"`
}

// toolDescription is part of the contract rather than decoration: it is the only
// place an agent learns that the source must be uploaded first, where to upload
// it, that the call does not wait for the build, and what an app has to do to be
// deployable (spec 0004, API surface; spec 0005, AC-4).
func (s *Server) toolDescription() string {
	return fmt.Sprintf(`Deploy an application to the cluster and return its public URL.

Give the source one of two ways, never both.

With no shell, pass it inline as files: a map of path relative to the app's
root to that file's content. Text only, and the same size ceiling below
applies to the whole set once packed.

With a shell, upload the source first, from the app's root directory, and pass
the upload_id it returns:

  curl -sS -X POST %s/v1/uploads \
    -H "Authorization: Bearer $DEPLOYER_TOKEN" \
    --data-binary @- < <(tar czf - .)

The archive must be at most %d MB. A larger one is refused with
upload_too_large before any of it is stored, so exclude build output, virtual
environments and version control directories before packing.

This call returns straight away with a
deployment_id and the state "queued"; it does not wait for the build. Call
deployment_status with that deployment_id to learn how the deploy ended.

A Dockerfile at the root of the upload is built as written. With no Dockerfile
there, the source is built with Cloud Native Buildpacks, which need no
configuration. Nothing here selects between them: remove the Dockerfile to get
Buildpacks. deployment_status reports which one ran as build_path.

Buildpacks cannot build a site that has no build step, so a plain HTML page
uploaded on its own fails detection with build_failed. Give it a Dockerfile
instead. This one serves a static site and satisfies both platform rules,
because the image already runs as a non root user and already listens on 8080:

  FROM nginxinc/nginx-unprivileged:1.29-alpine
  COPY . /usr/share/nginx/html

Plain nginx will not do: it runs as root and is refused with
image_runs_as_root.

A first build takes a few minutes, because both engines start cold and neither
caches layers; a Buildpacks build of the same app again is faster.

The URL in this response is the app's permanent hostname, but it does not serve
anything until deployment_status reports "healthy".

config is optional and follows set_config's rules exactly: each key needs a
value and a secret flag, keys look like environment variable names, and PORT
and APP_URL are refused because the platform sets them itself. Sending it here
sets those values before this deploy runs, which is how a new app's first run
avoids crashing on a missing variable. Keys you do not name are left alone, and
omitting config entirely changes nothing. Nothing you configure reaches the
build: values exist only in the running container, so a private package
registry is still out of reach at build time.

An account may hold a limited number of apps at once. Deploying a name the
account does not already have, once it is at that limit, is refused with
app_limit_reached; deleting an app frees a slot straight away. Redeploying an
app the account already has is never refused for this reason.

An account may also hold only a few uploads that no deploy has claimed yet. Once
it is at that limit a further upload is refused with upload_limit_reached, and
deploying one of the uploads already held, or waiting for them to expire, frees
a slot.

A deployed app reaches the internet on almost every port, but not quite all of
them. Outbound mail straight to a mail exchanger on port 25 is closed, as are
the common mining pool ports. A blocked connection times out rather than failing
fast, so an app that sends mail should use a provider's relay on port 587, which
is open, rather than delivering mail itself.

The app must listen on the port given in the PORT environment variable, and can
build links to itself from APP_URL, which the platform sets to its public
address. Its image must run as a non root user. Deploying the same name again
replaces the running app and keeps the same hostname.`,
		s.opts.MCPURL, s.opts.MaxUploadBytes>>20)
}

// Handler is the MCP endpoint, authenticated the same way the upload endpoint
// is: a bearer token on Authorization, no exemption for being MCP.
func (s *Server) Handler() http.Handler {
	return s.authenticate(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.serverFor(accountFrom(r.Context()))
	}, nil))
}

// serverFor builds the tool server bound to the calling account. A server per
// request is what keeps the account out of the tool arguments, where a caller
// could have chosen it.
func (s *Server) serverFor(account auth.Account) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "deployer",
		Title:   "Deployer",
		Version: "0.1.0",
	}, nil)
	if account.Disabled {
		srv.AddReceivingMiddleware(refuseSuspended)
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deploy_app",
		Title:       "Deploy an app",
		Description: s.toolDescription(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deployInput) (*mcp.CallToolResult, deployOutput, error) {
		return s.deploy(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_status",
		Title:       "Check a deployment",
		Description: statusDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, statusOutput, error) {
		return s.status(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_logs",
		Title:       "Read an app's recent output",
		Description: logsDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, logsOutput, error) {
		return s.getLogs(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_config",
		Title:       "Set an app's environment variables",
		Description: setConfigDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setConfigInput) (*mcp.CallToolResult, configOutput, error) {
		return s.setConfig(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "unset_config",
		Title:       "Remove an app's environment variables",
		Description: unsetConfigDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in unsetConfigInput) (*mcp.CallToolResult, configOutput, error) {
		return s.unsetConfig(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_config",
		Title:       "List an app's environment variables",
		Description: getConfigDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getConfigInput) (*mcp.CallToolResult, configOutput, error) {
		return s.getConfig(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_releases",
		Title:       "List an app's releases",
		Description: listReleasesDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listReleasesInput) (*mcp.CallToolResult, listReleasesOutput, error) {
		return s.listReleases(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_apps",
		Title:       "List your apps",
		Description: listAppsDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listAppsInput) (*mcp.CallToolResult, listAppsOutput, error) {
		return s.listApps(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_app",
		Title:       "Delete an app",
		Description: deleteAppDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteAppInput) (*mcp.CallToolResult, deleteAppOutput, error) {
		return s.deleteApp(ctx, account, in)
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rollback_app",
		Title:       "Roll an app back to an earlier release",
		Description: rollbackDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in rollbackInput) (*mcp.CallToolResult, rollbackOutput, error) {
		return s.rollback(ctx, account, in)
	})
	return srv
}

// deploy is the tool itself: validate, record, report. It does not wait: the
// deployment is the loop's from the moment the row is written, and the caller
// reads its outcome back through deployment_status (spec 0005, AC-1).
func (s *Server) deploy(ctx context.Context, account auth.Account, in deployInput) (*mcp.CallToolResult, deployOutput, error) {
	if in.Name == "" || (in.UploadID == "") == (len(in.Files) == 0) {
		return nil, deployOutput{}, s.deny(ctx, account.ID, "", domain.ReasonUploadInvalid,
			errors.New("name and exactly one of upload_id or files are required"))
	}

	uploadID := in.UploadID
	if len(in.Files) > 0 {
		var err error
		if uploadID, err = s.acceptFiles(ctx, account, in.Files); err != nil {
			return nil, deployOutput{}, err
		}
	}

	// The upload is checked before the app is touched, so a call that fails on a
	// bad upload_id audits with a null target and creates no app row (AC-19). One
	// this call just accepted passes it, which is deliberate: the check is about
	// the caller's claim on an id, and there is one path rather than two.
	if err := s.checkUpload(ctx, account, uploadID); err != nil {
		return nil, deployOutput{}, err
	}

	app, err := s.resolveApp(ctx, account, in.Name)
	if err != nil {
		return nil, deployOutput{}, err
	}

	// Written before the deployment row, through the same path set_config uses,
	// so the two can never enforce different rules. A refusal here writes no
	// configuration and starts no deployment (AC-9).
	if len(in.Config) > 0 {
		if reason := s.writeConfig(ctx, account, app, in.Config, auth.ActionConfigSet); reason != "" {
			return nil, deployOutput{}, s.deny(ctx, account.ID, app.ID, reason, nil)
		}
	}

	deploymentID, err := s.deployments.Create(ctx, app.ID, account.ID, uploadID)
	if err != nil {
		return nil, deployOutput{}, s.deny(ctx, account.ID, app.ID, domain.ReasonInternal, err)
	}
	auth.Record(ctx, s.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionDeploy,
		TargetType: "app", TargetID: app.ID, Allowed: true,
	})

	// The row was just written queued and nothing is read back, so the state is
	// the constant rather than a read (spec 0005, AC-2).
	return nil, deployOutput{
		Name:         app.Name,
		Slug:         app.Slug,
		URL:          s.appURL(app.Slug),
		DeploymentID: deploymentID,
		State:        string(domain.StateQueued),
	}, nil
}

// appURL is the app's permanent address, composed from its slug rather than
// stored, so it is known before anything is built.
func (s *Server) appURL(slug string) string {
	return "https://" + slug + "." + s.opts.AppDomain
}

// acceptFiles packs the source a caller carried inline and records it as an
// ordinary upload, returning its id. Every bound the upload endpoint applies is
// applied here, by the same service: this is a second way to reach that path,
// never a second path.
//
// The refusals are the endpoint's own reason codes, so a caller that hits the
// ceiling reads the same words whichever way it sent the source.
func (s *Server) acceptFiles(ctx context.Context, account auth.Account, files map[string]string) (string, error) {
	body, err := uploads.Pack(files)
	if err != nil {
		return "", s.deny(ctx, account.ID, "", domain.ReasonUploadInvalid, err)
	}
	up, err := s.uploads.Accept(ctx, account.ID, body)
	switch {
	case errors.Is(err, uploads.ErrTooLarge):
		return "", s.deny(ctx, account.ID, "", domain.ReasonUploadTooLarge, err)
	case errors.Is(err, uploads.ErrTooManyUnclaimed):
		return "", s.deny(ctx, account.ID, "", domain.ReasonUploadLimitReached, err)
	case err != nil:
		return "", s.deny(ctx, account.ID, "", domain.ReasonInternal, err)
	}
	return up.ID, nil
}

// checkUpload refuses an upload that is unknown, spent, expired, or another
// account's, in the same words for all four: a caller learns whether their own
// upload works, never whether someone else's exists.
func (s *Server) checkUpload(ctx context.Context, account auth.Account, uploadID string) error {
	up, err := s.uploads.Get(ctx, uploadID)
	if err != nil || up.AccountID != account.ID || up.Redeemed {
		return s.deny(ctx, account.ID, "", domain.ReasonUploadInvalid, err)
	}
	expires, err := time.Parse(time.RFC3339Nano, up.ExpiresAt)
	if err != nil {
		return s.deny(ctx, account.ID, "", domain.ReasonInternal,
			fmt.Errorf("unreadable expiry on upload %s: %w", uploadID, err))
	}
	if !time.Now().UTC().Before(expires) {
		return s.deny(ctx, account.ID, "", domain.ReasonUploadExpired, nil)
	}
	return nil
}

// resolveApp finds the account's app under that name, or creates it. The same
// pair always resolves to the same row, which is what keeps the hostname an
// agent has already handed someone working (AC-4).
//
// The per account cap is a rule about creating an app, never about deploying
// one, so it is only reached on the branch that creates: an account at or over
// its ceiling redeploys everything it already runs untouched (spec 0016, AC-4).
// The error this returns is already the refusal the caller sees, audited, so
// deploy hands it straight back.
func (s *Server) resolveApp(ctx context.Context, account auth.Account, name string) (App, error) {
	app, err := s.apps.ByName(ctx, account.ID, name)
	if err == nil {
		return app, nil
	}
	if !errors.Is(err, ErrNoApp) {
		return App{}, s.deny(ctx, account.ID, "", domain.ReasonInternal, fmt.Errorf("resolving the app: %w", err))
	}

	// Read before the create so a caller at the ceiling is told the numbers
	// rather than left to guess. The create counts again inside its own
	// transaction, which is what actually decides a race (AC-6); this read is
	// what makes the ordinary refusal legible.
	held, err := s.apps.Count(ctx, account.ID)
	if err != nil {
		return App{}, s.deny(ctx, account.ID, "", domain.ReasonInternal, fmt.Errorf("counting the account's apps: %w", err))
	}
	limit := s.opts.MaxAppsPerAccount
	if held >= limit {
		return App{}, s.deny(ctx, account.ID, "", domain.ReasonAppLimitReached, nil, usedOfLimit(held, limit))
	}

	app, err = s.apps.Create(ctx, account.ID, name, limit)
	if errors.Is(err, ErrAppLimit) {
		return App{}, s.deny(ctx, account.ID, "", domain.ReasonAppLimitReached, nil, usedOfLimit(limit, limit))
	}
	if errors.Is(err, ErrAppNameReserved) {
		return App{}, s.deny(ctx, account.ID, "", domain.ReasonAppNameReserved, nil)
	}
	if err != nil {
		return App{}, s.deny(ctx, account.ID, "", domain.ReasonInternal, fmt.Errorf("creating the app: %w", err))
	}
	return app, nil
}

// usedOfLimit is the detail a cap refusal carries: the account's own numbers,
// and nothing about the platform or anyone else (spec 0016, AC-3).
func usedOfLimit(held, limit int) string {
	return fmt.Sprintf("%d of %d used", held, limit)
}

// deny records a refusal against deploy_app and turns it into the one line a
// caller sees. detail is optional and is passed straight to toolError.
func (s *Server) deny(ctx context.Context, accountID, appID string, reason domain.Reason, cause error, detail ...string) error {
	entry := auth.Audit{AccountID: accountID, Action: auth.ActionDeploy, Reason: string(reason)}
	if appID != "" {
		entry.TargetType, entry.TargetID = "app", appID
	}
	auth.Record(ctx, s.auditor, entry)
	return toolError(auth.ActionDeploy, reason, cause, detail...)
}

// toolError is the only shape a failure leaves this package in: a reason code
// and its one sanitized line. The cause is logged here and goes no further, so
// no build output, cluster message, or wrapped error reaches a caller (AC-16).
//
// detail is at most one short phrase composed here, in parentheses after the
// static line. It exists so a refusal can carry the caller's own numbers while
// the domain message stays static and numberless; a call that passes none
// composes exactly as it did before there was one (spec 0016, AC-3).
func toolError(action string, reason domain.Reason, cause error, detail ...string) error {
	if cause != nil {
		slog.Error("an mcp tool failed", "tool", action, "reason", reason, "error", cause)
	}
	if len(detail) > 0 && detail[0] != "" {
		return fmt.Errorf("%s: %s (%s)", reason, reason.Message(), detail[0])
	}
	return fmt.Errorf("%s: %s", reason, reason.Message())
}
