// Package mcp is the agent facing tool surface: one tool, deploy_app, served
// over the streamable HTTP transport beside the upload endpoint.
//
// The handler observes and never acts. It resolves the upload, resolves or
// creates the app, writes a queued deployment, and then only ever reads
// committed state until that deployment is terminal. Everything that happens in
// between is the reconcile loop's (spec 0004, Key invariants).
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
)

// ErrNoApp means the account has no app under that name yet, which is the first
// deploy rather than a failure.
var ErrNoApp = errors.New("mcp: no such app")

// ErrNoUpload means the upload id resolved to nothing.
var ErrNoUpload = errors.New("mcp: no such upload")

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

// Outcome is where a deployment has got to, read back off committed state.
type Outcome struct {
	State  domain.State
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
	// Create registers an app, deriving its permanent slug from the name.
	Create(ctx context.Context, accountID, name string) (App, error)
}

// Deployments is the slice of persistence this package needs for the wait.
type Deployments interface {
	// Create writes a queued deployment, superseding anything in flight.
	Create(ctx context.Context, appID, accountID, uploadID string) (string, error)
	// Outcome reads a deployment's committed state.
	Outcome(ctx context.Context, deploymentID string) (Outcome, error)
	// Release reads the release a healthy deployment minted.
	Release(ctx context.Context, deploymentID string) (Release, error)
}

// Uploads reads the tarball a deploy names.
type Uploads interface {
	Get(ctx context.Context, id string) (Upload, error)
}

// Options is what the tool surface needs from configuration.
type Options struct {
	PublicURL    string
	AppDomain    string
	DeployBudget time.Duration // DEPLOYER_DEPLOY_TIMEOUT_SECONDS
	PollInterval time.Duration // DEPLOYER_RECONCILE_INTERVAL_SECONDS, so the
	// handler can never poll faster than state can change
}

// Server is the MCP surface.
type Server struct {
	auth        *auth.Authenticator
	auditor     auth.Auditor
	apps        Apps
	deployments Deployments
	uploads     Uploads
	opts        Options
}

// New returns the MCP surface.
func New(a *auth.Authenticator, auditor auth.Auditor, apps Apps, d Deployments, u Uploads, opts Options) *Server {
	return &Server{auth: a, auditor: auditor, apps: apps, deployments: d, uploads: u, opts: opts}
}

// deployInput is the tool's whole argument surface.
type deployInput struct {
	Name     string `json:"name" jsonschema:"the app's name, which fixes its hostname for good; reuse it to redeploy the same app"`
	UploadID string `json:"upload_id" jsonschema:"the id returned by POST /v1/uploads"`
}

// deployOutput is what a successful deploy reports.
type deployOutput struct {
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	URL           string `json:"url"`
	DeploymentID  string `json:"deployment_id"`
	ReleaseNumber int64  `json:"release_number"`
	ImageDigest   string `json:"image_digest"`
}

// toolDescription is part of the contract rather than decoration: it is the only
// place an agent learns that the source must be uploaded first, where to upload
// it, and what an app has to do to be deployable (spec 0004, API surface).
func (s *Server) toolDescription() string {
	return fmt.Sprintf(`Deploy an application to the cluster and return its public URL.

Upload the source first, from the app's root directory:

  curl -sS -X POST %s/v1/uploads \
    -H "Authorization: Bearer $DEPLOYER_TOKEN" \
    --data-binary @- < <(tar czf - .)

Pass the upload_id it returns here. This call runs for minutes on a first
build, because the source is built with Cloud Native Buildpacks from cold.

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

// serverFor builds the one tool server bound to the calling account. A server
// per request is what keeps the account out of the tool arguments, where a
// caller could have chosen it.
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
	return srv
}

// deploy is the tool itself: validate, record, wait, report.
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

	outcome, err := s.wait(ctx, deploymentID)
	if err != nil {
		return nil, deployOutput{}, toolError(domain.ReasonTimeout, err)
	}
	if outcome.State != domain.StateHealthy {
		return nil, deployOutput{}, toolError(outcome.Reason,
			fmt.Errorf("deployment %s ended %s", deploymentID, outcome.State))
	}

	release, err := s.deployments.Release(ctx, deploymentID)
	if err != nil {
		return nil, deployOutput{}, toolError(domain.ReasonInternal, err)
	}
	return nil, deployOutput{
		Name:          app.Name,
		Slug:          app.Slug,
		URL:           "https://" + app.Slug + "." + s.opts.AppDomain,
		DeploymentID:  deploymentID,
		ReleaseNumber: release.Number,
		ImageDigest:   release.Digest,
	}, nil
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

// wait polls committed state until the deployment is terminal. It never
// transitions anything: the loop owns every move, and this only reads.
func (s *Server) wait(ctx context.Context, deploymentID string) (Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.DeployBudget)
	defer cancel()

	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()
	for {
		outcome, err := s.deployments.Outcome(ctx, deploymentID)
		if err != nil {
			slog.WarnContext(ctx, "reading a deployment failed, retrying", "deployment", deploymentID, "error", err)
		} else if outcome.State.Terminal() {
			return outcome, nil
		}
		select {
		case <-ctx.Done():
			// The call ran out of budget. The deployment itself is still the
			// loop's to finish or fail; nothing here abandons it.
			return Outcome{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// deny records a refusal and turns it into the one line a caller sees.
func (s *Server) deny(ctx context.Context, accountID, appID string, reason domain.Reason, cause error) error {
	entry := auth.Audit{AccountID: accountID, Action: auth.ActionDeploy, Reason: string(reason)}
	if appID != "" {
		entry.TargetType, entry.TargetID = "app", appID
	}
	auth.Record(ctx, s.auditor, entry)
	return toolError(reason, cause)
}

// toolError is the only shape a failure leaves this package in: a reason code
// and its one sanitized line. The cause is logged here and goes no further, so
// no build output, cluster message, or wrapped error reaches a caller (AC-16).
func toolError(reason domain.Reason, cause error) error {
	if cause != nil {
		slog.Error("deploy_app failed", "reason", reason, "error", cause)
	}
	return fmt.Errorf("%s: %s", reason, reason.Message())
}
