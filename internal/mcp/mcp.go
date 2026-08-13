// Package mcp is the agent facing tool surface: deploy_app and
// deployment_status, served over the streamable HTTP transport beside the upload
// endpoint.
//
// The handler observes and never acts. deploy_app resolves the upload, resolves
// or creates the app, writes a queued deployment, and returns without reading
// anything back; deployment_status is a pure read. Everything in between is the
// reconcile loop's (spec 0004, Key invariants; spec 0005, AC-3).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/logs"
)

// ErrNoApp means the account has no app under that name yet, which is the first
// deploy rather than a failure.
var ErrNoApp = errors.New("mcp: no such app")

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

// Apps is the slice of persistence this package needs for the get or create.
type Apps interface {
	// ByName returns the account's app under that name, or ErrNoApp.
	ByName(ctx context.Context, accountID, name string) (App, error)
	// Get reads the app a deployment belongs to.
	Get(ctx context.Context, appID string) (App, error)
	// Create registers an app, deriving its permanent slug from the name.
	Create(ctx context.Context, accountID, name string) (App, error)
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
}

// Uploads reads the tarball a deploy names.
type Uploads interface {
	Get(ctx context.Context, id string) (Upload, error)
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
	PublicURL string
	AppDomain string
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
	opts        Options
}

// New returns the MCP surface. pods may be nil, which is a local run with no
// cluster to read: get_logs then fails as internal rather than pretending an app
// printed nothing.
func New(a *auth.Authenticator, auditor auth.Auditor, apps Apps, d Deployments, u Uploads, pods Pods, opts Options) *Server {
	return &Server{auth: a, auditor: auditor, apps: apps, deployments: d, uploads: u, pods: pods, opts: opts}
}

// deployInput is the tool's whole argument surface.
type deployInput struct {
	Name     string `json:"name" jsonschema:"the app's name, which fixes its hostname for good; reuse it to redeploy the same app"`
	UploadID string `json:"upload_id" jsonschema:"the id returned by POST /v1/uploads"`
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

Upload the source first, from the app's root directory:

  curl -sS -X POST %s/v1/uploads \
    -H "Authorization: Bearer $DEPLOYER_TOKEN" \
    --data-binary @- < <(tar czf - .)

Pass the upload_id it returns here. This call returns straight away with a
deployment_id and the state "queued"; it does not wait for the build. Call
deployment_status with that deployment_id to learn how the deploy ended.

A Dockerfile at the root of the upload is built as written. With no Dockerfile
there, the source is built with Cloud Native Buildpacks, which need no
configuration. Nothing here selects between them: remove the Dockerfile to get
Buildpacks. deployment_status reports which one ran as build_path.

A first build takes a few minutes, because both engines start cold and neither
caches layers; a Buildpacks build of the same app again is faster.

The URL in this response is the app's permanent hostname, but it does not serve
anything until deployment_status reports "healthy".

The app must listen on the port given in the PORT environment variable, and
its image must run as a non root user. Deploying the same name again replaces
the running app and keeps the same hostname.`, s.opts.PublicURL)
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
	return srv
}

// deploy is the tool itself: validate, record, report. It does not wait: the
// deployment is the loop's from the moment the row is written, and the caller
// reads its outcome back through deployment_status (spec 0005, AC-1).
func (s *Server) deploy(ctx context.Context, account auth.Account, in deployInput) (*mcp.CallToolResult, deployOutput, error) {
	if in.Name == "" || in.UploadID == "" {
		return nil, deployOutput{}, s.deny(ctx, account.ID, "", domain.ReasonUploadInvalid,
			errors.New("name and upload_id are both required"))
	}

	// The upload is checked before the app is touched, so a call that fails on a
	// bad upload_id audits with a null target and creates no app row (AC-19).
	if err := s.checkUpload(ctx, account, in.UploadID); err != nil {
		return nil, deployOutput{}, err
	}

	app, err := s.resolveApp(ctx, account.ID, in.Name)
	if err != nil {
		return nil, deployOutput{}, s.deny(ctx, account.ID, "", domain.ReasonInternal, err)
	}

	deploymentID, err := s.deployments.Create(ctx, app.ID, account.ID, in.UploadID)
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
func (s *Server) resolveApp(ctx context.Context, accountID, name string) (App, error) {
	app, err := s.apps.ByName(ctx, accountID, name)
	if err == nil {
		return app, nil
	}
	if !errors.Is(err, ErrNoApp) {
		return App{}, fmt.Errorf("resolving the app: %w", err)
	}
	app, err = s.apps.Create(ctx, accountID, name)
	if err != nil {
		return App{}, fmt.Errorf("creating the app: %w", err)
	}
	return app, nil
}

// deny records a refusal against deploy_app and turns it into the one line a
// caller sees.
func (s *Server) deny(ctx context.Context, accountID, appID string, reason domain.Reason, cause error) error {
	entry := auth.Audit{AccountID: accountID, Action: auth.ActionDeploy, Reason: string(reason)}
	if appID != "" {
		entry.TargetType, entry.TargetID = "app", appID
	}
	auth.Record(ctx, s.auditor, entry)
	return toolError(auth.ActionDeploy, reason, cause)
}

// toolError is the only shape a failure leaves this package in: a reason code
// and its one sanitized line. The cause is logged here and goes no further, so
// no build output, cluster message, or wrapped error reaches a caller (AC-16).
func toolError(action string, reason domain.Reason, cause error) error {
	if cause != nil {
		slog.Error("an mcp tool failed", "tool", action, "reason", reason, "error", cause)
	}
	return fmt.Errorf("%s: %s", reason, reason.Message())
}
