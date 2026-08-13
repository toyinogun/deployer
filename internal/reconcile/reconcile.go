// Package reconcile drives a deployment from queued to healthy, or to failed.
//
// It is the only writer of deployment state after the row is created: the MCP
// handler writes a queued row and then only ever reads. One deployment is driven
// at a time, platform wide, so a second caller waits rather than competing for a
// node (spec 0004, AC-6).
//
// Every phase is a database write before it is an action, so a control plane
// that restarts mid build can tell a live build from a lost one and finish or
// fail it rather than leaving a row in flight forever (AC-18).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/source"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrNoWork means there was nothing queued, which is the normal answer on most
// ticks rather than a failure.
var ErrNoWork = errors.New("reconcile: nothing queued")

// ErrNotInFlight means the deployment reached a terminal state while this drive
// was running, which is what a redeploy of the same app does to the row under
// it. The drive stops at the next write and reports nothing: the row is already
// ended, correctly, by whoever ended it.
var ErrNotInFlight = errors.New("reconcile: the deployment is no longer in flight")

// Deployment is the row this package drives, carrying only the fields it reads.
//
// CreatedAt is what the deploy budget is measured from, and BuildJobName is what
// the watchdog deletes when it gives up on one (spec 0005, AC-14, AC-15).
type Deployment struct {
	ID           string
	AppID        string
	UploadID     string
	State        domain.State
	ImageRepo    string
	ImageDigest  string
	CreatedAt    time.Time
	BuildJobName string
	// BuildPath is which engine this deployment's build runs, as the row records
	// it. It is on the deployment rather than a local inside startBuild because
	// every later cluster call about the Job has to resolve to the namespace it
	// was created in, including on a row a restart picked up rather than started:
	// Sweep rebuilds a deployment from the store, and a Job deleted from the wrong
	// namespace is a Job that keeps running (spec 0009, AC-19a).
	//
	// Empty on a row whose build has not started yet, which reads as the
	// Buildpacks path, the same answer every archive got before this feature.
	BuildPath string
}

// App is the app a deployment belongs to.
type App struct {
	ID   string
	Slug string
}

// Upload is the tarball a build fetches.
type Upload struct {
	ID     string
	Path   string
	SHA256 string
}

// Release is what MarkHealthy minted.
type Release struct {
	Number int64
	Digest string
}

// Deployments is the slice of persistence this package needs for the state walk.
type Deployments interface {
	// ClaimNext hands over the oldest queued deployment, or ErrNoWork.
	ClaimNext(ctx context.Context, claimedBy string) (Deployment, error)
	// ListNonTerminal returns everything still in flight, which the sweep reads.
	ListNonTerminal(ctx context.Context) ([]Deployment, error)
	// Transition moves a deployment, writing the row and one event together.
	Transition(ctx context.Context, id string, to domain.State, reason, detail string) error
	// RecordBuild stores what the build produced without moving the row. The
	// path is written on the way in rather than on the way out, so a build that
	// fails still says which engine ran it (spec 0009, AC-4).
	RecordBuild(ctx context.Context, id, buildPath, jobName, imageRepo, imageDigest string) error
	// MarkHealthy writes the transition, the release, and the app's current
	// release pointer in one transaction.
	MarkHealthy(ctx context.Context, id string) (Release, error)
}

// Apps reads the app a deployment is for.
type Apps interface {
	Get(ctx context.Context, id string) (App, error)
}

// Uploads is what the loop needs of the source tarball.
type Uploads interface {
	Get(ctx context.Context, id string) (Upload, error)
	// Open reads a stored tarball back. The loop walks its tar headers to choose
	// a build engine, so this is the one place the control plane opens caller
	// supplied bytes it otherwise only stores and hashes.
	Open(path string) (io.ReadCloser, error)
	// MintFetchToken generates the single use token this build presents, and
	// returns the raw value, which is never persisted or logged.
	MintFetchToken(ctx context.Context, id string) (string, error)
	// Remove deletes the tarball from the volume once a deployment is terminal.
	Remove(ctx context.Context, path string)
}

// Registry resolves what a build pushed and reads back what it runs as.
type Registry interface {
	Digest(ctx context.Context, repo, tag string) (string, error)
	ImageUser(ctx context.Context, repo, digest string) (string, error)
}

// Cluster is the slice of Kubernetes this loop drives. internal/kube satisfies
// it, which is what keeps client-go out of this package.
type Cluster interface {
	CreateJob(ctx context.Context, job *batchv1.Job) (metav1.OwnerReference, error)
	JobState(ctx context.Context, namespace, name string) (build.JobState, error)
	ApplySecret(ctx context.Context, s *corev1.Secret) error
	EnsureNamespace(ctx context.Context, ns *corev1.Namespace, rb *rbacv1.RoleBinding, quota *corev1.ResourceQuota, limits *corev1.LimitRange) error
	ApplyNetworkPolicies(ctx context.Context, policies ...*networkingv1.NetworkPolicy) error
	AppNamespaces(ctx context.Context) ([]string, error)
	ApplyWorkload(ctx context.Context, d *appsv1.Deployment, s *corev1.Service, i *networkingv1.Ingress) error
	WorkloadReady(ctx context.Context, namespace, name string) (bool, error)
	DeleteJob(ctx context.Context, namespace, name string) error
}

// Options is everything the loop needs from configuration.
type Options struct {
	PodName               string
	ControlPlaneNamespace string
	BuildNamespace        string
	// BuildkitNamespace is where Dockerfile build Jobs go. A second namespace
	// rather than a second Job shape in the first one, because BuildKit is
	// admitted only at `privileged` and BuildNamespace enforces `restricted`.
	BuildkitNamespace string
	AppDomain         string
	IngressClassName  string

	SelfImage    string
	BuilderImage string
	// BuildUID and BuildGID are the user BuilderImage declares, which the build
	// pod has to start as.
	BuildUID int64
	BuildGID int64
	// BuildkitImage is the rootless BuildKit engine the Dockerfile path runs,
	// and BuildkitUID/BuildkitGID are the pair that image declares. They are a
	// unit with it, never interchangeable with the Paketo pair above.
	BuildkitImage string
	BuildkitUID   int64
	BuildkitGID   int64
	// InternalURL is where the build Job's init container reaches the platform.
	// It runs on cluster DNS, so it cannot use the public address.
	InternalURL string

	RegistryHost string
	RegistryUser string
	RegistryPass string

	DeployTimeout     time.Duration
	BuildTimeout      time.Duration
	ReadyTimeout      time.Duration
	ReconcileInterval time.Duration

	MaxUploadFiles    int
	MaxExtractedBytes int64

	CPU         string
	Memory      string
	LimitCPU    string
	LimitMemory string

	QuotaCPU    string
	QuotaMemory string
	QuotaPods   int

	// EgressBlockedCIDRs is what an app's egress rule carves out of the internet,
	// already parsed at startup (spec 0008).
	EgressBlockedCIDRs []string
}

// Reconciler drives deployments.
type Reconciler struct {
	deployments Deployments
	apps        Apps
	uploads     Uploads
	registry    Registry
	cluster     Cluster
	opts        Options
}

// New returns a reconciler over its dependencies.
func New(d Deployments, a Apps, u Uploads, r Registry, c Cluster, opts Options) *Reconciler {
	return &Reconciler{deployments: d, apps: a, uploads: u, registry: r, cluster: c, opts: opts}
}

// Run sweeps what a restart left behind and then ticks until ctx is done.
//
// One goroutine, one deployment at a time. That is the whole concurrency model,
// and it is what AC-6 asks for.
func (r *Reconciler) Run(ctx context.Context) {
	// The fence first, ahead of any deployment work: a namespace left unpoliced
	// by an earlier release is closed before this process drives anything into
	// it (spec 0008, AC-12).
	r.PolicySweep(ctx)
	r.Sweep(ctx)

	ticker := time.NewTicker(r.opts.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick fails whatever has run past the deploy budget, then claims one deployment
// and drives it to a terminal state.
func (r *Reconciler) tick(ctx context.Context) {
	r.expireOverdue(ctx)

	dep, err := r.deployments.ClaimNext(ctx, r.opts.PodName)
	if errors.Is(err, ErrNoWork) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "claiming a deployment failed", "error", err)
		return
	}
	r.Drive(ctx, dep)
}

// Sweep reconciles every non terminal deployment against the cluster, which is
// what a restart mid build resolves through: a live Job is picked back up, and a
// Job that no longer exists fails its deployment rather than leaving it in
// flight forever (AC-18).
func (r *Reconciler) Sweep(ctx context.Context) {
	deps, err := r.deployments.ListNonTerminal(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "sweeping in flight deployments failed", "error", err)
		return
	}
	for _, dep := range deps {
		if ctx.Err() != nil {
			return
		}
		slog.InfoContext(ctx, "resuming a deployment left in flight", "deployment", dep.ID, "state", dep.State)
		r.Drive(ctx, dep)
	}
}

// expireOverdue fails every non terminal deployment that has run past the deploy
// budget. It runs at the top of a tick, before anything is claimed, so it is one
// store query and no cluster call unless something is actually overdue.
//
// Between drives rather than during one: what bounds how long an overdue row
// waits here is the budget check inside the drive ahead of it (spec 0005, AC-14).
func (r *Reconciler) expireOverdue(ctx context.Context) {
	deps, err := r.deployments.ListNonTerminal(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "reading in flight deployments for the deploy budget failed", "error", err)
		return
	}
	for _, dep := range deps {
		if ctx.Err() != nil {
			return
		}
		if r.remainingBudget(dep) > 0 {
			continue
		}
		r.expire(ctx, dep)
	}
}

// remainingBudget is what is left of the deploy budget for this row, measured
// from when it was created rather than when it was claimed, so queue time counts
// against it and a resumed deployment never gets a fresh window (AC-14a).
func (r *Reconciler) remainingBudget(dep Deployment) time.Duration {
	if dep.CreatedAt.IsZero() {
		return r.opts.DeployTimeout
	}
	return r.opts.DeployTimeout - time.Since(dep.CreatedAt)
}

// expire is the watchdog's whole action: stop the build if there is one, write
// the failure, and drop the tarball, because this row is terminal now and no
// caller is counting the seconds any more (AC-15, AC-18).
func (r *Reconciler) expire(ctx context.Context, dep Deployment) {
	r.deleteBuildJob(ctx, dep)
	r.fail(ctx, dep.ID, &failure{domain.ReasonTimeout,
		fmt.Errorf("deployment %s ran past the %s deploy budget", dep.ID, r.opts.DeployTimeout)})
	if upload, err := r.uploads.Get(ctx, dep.UploadID); err == nil {
		r.uploads.Remove(ctx, upload.Path)
	}
}

// buildNamespace is where this deployment's build lives, derived from the path
// the row already records and from nothing else.
//
// One derivation, read by the Job create, the credential, the state poll and the
// delete, so a Job is never addressed in a namespace it is not in. The archive is
// never walked a second time to answer this: the row is the single source, which
// is why startBuild writes the path before the Job exists (AC-19).
func (r *Reconciler) buildNamespace(dep Deployment) string {
	if build.Path(dep.BuildPath) == build.PathDockerfile {
		return r.opts.BuildkitNamespace
	}
	return r.opts.BuildNamespace
}

// deleteBuildJob removes the build behind a deployment the watchdog is giving up
// on. A row with no Job name never had one, so there is nothing to delete.
func (r *Reconciler) deleteBuildJob(ctx context.Context, dep Deployment) {
	if dep.BuildJobName == "" {
		return
	}
	if err := r.cluster.DeleteJob(ctx, r.buildNamespace(dep), dep.BuildJobName); err != nil {
		// Logged and carried on: a Job that would not delete must not leave the
		// row in flight forever, which is the one thing the watchdog is for.
		slog.ErrorContext(ctx, "deleting the build job of an overdue deployment failed",
			"deployment", dep.ID, "job", dep.BuildJobName, "error", err)
	}
}

// Drive takes one deployment as far as it goes, and records the outcome whatever
// it is. Every exit from here is terminal, so nothing this touches stays queued.
func (r *Reconciler) Drive(ctx context.Context, dep Deployment) {
	// What is left of the deploy budget, not a fresh one: a deployment that has
	// already spent it is failed without being driven at all (AC-14a).
	budget := r.remainingBudget(dep)
	if budget <= 0 {
		r.expire(ctx, dep)
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	upload, uploadErr := r.uploads.Get(runCtx, dep.UploadID)
	switch fail := r.run(runCtx, &dep, upload, uploadErr); {
	case fail == nil:
	case errors.Is(fail.err, ErrNotInFlight):
		// A redeploy of the same app cancelled this row mid drive. It is already
		// terminal with the right reason, so this drive writes nothing: failing it
		// would be reporting a fault where the platform behaved exactly as specced.
		slog.InfoContext(ctx, "the deployment was ended while it was being driven, so the drive stopped",
			"deployment", dep.ID, "phase", dep.State)
	default:
		// The budget expiring is the reason, whichever step happened to notice it.
		// Only the phase boundary check recognises it by itself; a deadline that
		// fires inside a store read or a cluster call surfaces as whatever that
		// step returns, and reporting a spent budget as an internal fault is
		// exactly the masked error a caller must never be handed.
		if runCtx.Err() != nil {
			fail = &failure{domain.ReasonTimeout, runCtx.Err()}
		}
		if fail.reason == domain.ReasonTimeout {
			// The budget is what ran out, so the build goes with it (AC-15).
			r.deleteBuildJob(ctx, dep)
		}
		r.fail(ctx, dep.ID, fail)
	}
	// Terminal either way, so the tarball has served its purpose (AC-22).
	r.uploads.Remove(ctx, upload.Path)
}

// overBudget is the phase boundary check: a drive never starts another phase
// once the deploy budget is spent (AC-14).
func overBudget(ctx context.Context) *failure {
	if ctx.Err() != nil {
		return &failure{domain.ReasonTimeout, ctx.Err()}
	}
	return nil
}

// run walks the phases, resuming at whichever one the row's state says is next.
// It returns nil when the deployment reached healthy.
func (r *Reconciler) run(ctx context.Context, dep *Deployment, upload Upload, uploadErr error) *failure {
	app, err := r.apps.Get(ctx, dep.AppID)
	if err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("reading the app: %w", err)}
	}
	if uploadErr != nil {
		return &failure{domain.ReasonUploadInvalid, fmt.Errorf("reading the upload: %w", uploadErr)}
	}

	if dep.State == domain.StateQueued {
		if fail := overBudget(ctx); fail != nil {
			return fail
		}
		if fail := r.startBuild(ctx, dep, app, upload); fail != nil {
			return fail
		}
	}
	if dep.State == domain.StateBuilding {
		if fail := overBudget(ctx); fail != nil {
			return fail
		}
		if fail := r.awaitBuild(ctx, dep); fail != nil {
			return fail
		}
	}
	if dep.State == domain.StatePushing {
		if fail := overBudget(ctx); fail != nil {
			return fail
		}
		if fail := r.resolveImage(ctx, dep, app); fail != nil {
			return fail
		}
	}
	if fail := overBudget(ctx); fail != nil {
		return fail
	}
	return r.deployApp(ctx, dep, app)
}

// startBuild mints a fresh fetch token, creates the build Job and the credential
// it pushes with, and moves the row to building.
//
// The order matters: the row moves first, so a restart between the write and the
// create finds a building row and a Job that either exists or does not, both of
// which the sweep can resolve.
func (r *Reconciler) startBuild(ctx context.Context, dep *Deployment, app App, upload Upload) *failure {
	if fail := r.move(ctx, dep, domain.StateBuilding); fail != nil {
		return fail
	}

	// The engine is chosen before anything else, because a Job's image is fixed
	// the moment it is created, and refused here costs no Job and no credential.
	path, fail := r.buildPath(upload)
	if fail != nil {
		return fail
	}
	// Onto the row before anything is created with it, so the Job create, the
	// credential, the poll and the delete all read the same one answer, and a
	// restart between here and the create finds it too.
	dep.BuildPath = path.String()
	namespace := r.buildNamespace(*dep)

	token, err := r.uploads.MintFetchToken(ctx, upload.ID)
	if err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("minting a fetch token: %w", err)}
	}
	target := build.TargetImage(r.opts.RegistryHost, app.Slug, dep.ID)
	job := build.Job(build.Input{
		DeploymentID:    dep.ID,
		Namespace:       namespace,
		AppSlug:         app.Slug,
		Path:            path,
		SelfImage:       r.opts.SelfImage,
		BuilderImage:    r.opts.BuilderImage,
		BuildUID:        r.opts.BuildUID,
		BuildGID:        r.opts.BuildGID,
		BuildkitImage:   r.opts.BuildkitImage,
		BuildkitUID:     r.opts.BuildkitUID,
		BuildkitGID:     r.opts.BuildkitGID,
		TargetImage:     target,
		FetchURL:        r.opts.InternalURL + "/v1/uploads/" + upload.ID,
		FetchToken:      token,
		ExpectedSHA:     upload.SHA256,
		MaxFiles:        r.opts.MaxUploadFiles,
		MaxExtracted:    r.opts.MaxExtractedBytes,
		CredentialRef:   build.SecretName(dep.ID),
		DeadlineSeconds: int64(r.opts.BuildTimeout.Seconds()),
	})

	owner, err := r.cluster.CreateJob(ctx, job)
	if err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("creating the build job: %w", err)}
	}
	// Owner referenced to the Job, so the write credential is collected with it
	// and never outlives the one build it was for.
	secret, err := build.BuildCredentialSecret(dep.ID, namespace, r.credential(), owner)
	if err != nil {
		return &failure{domain.ReasonInternal, err}
	}
	if err := r.cluster.ApplySecret(ctx, secret); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("writing the build credential: %w", err)}
	}

	dep.ImageRepo = build.ImageRepo(r.opts.RegistryHost, app.Slug)
	dep.BuildJobName = build.JobName(dep.ID)
	if err := r.deployments.RecordBuild(ctx, dep.ID, path.String(), dep.BuildJobName, dep.ImageRepo, ""); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("recording the build job: %w", err)}
	}
	return nil
}

// buildPath chooses the engine by reading the stored archive's tar headers: a
// regular file that unpacks to Dockerfile at the root goes to BuildKit, and
// anything else goes to Buildpacks, which is what every tree got before this.
//
// Nothing a caller sent selects it, and deploy_app has no argument for it. An
// archive this cannot read is source_rejected here rather than at extraction,
// because the alternative is a deployment that chose an engine on its way to
// being refused for something else.
func (r *Reconciler) buildPath(upload Upload) (build.Path, *failure) {
	f, err := r.uploads.Open(upload.Path)
	if err != nil {
		return build.PathBuildpacks, &failure{domain.ReasonInternal, fmt.Errorf("opening the archive: %w", err)}
	}
	defer func() { _ = f.Close() }()

	// The same entry limit the extractor enforces, not a second number: this walk
	// is the control plane doing work on caller supplied bytes, and an archive of
	// nothing but headers is small on disk and long to read.
	found, err := source.HasRootDockerfile(f, source.Limits{
		MaxFiles: r.opts.MaxUploadFiles,
		MaxBytes: r.opts.MaxExtractedBytes,
	})
	switch {
	case errors.Is(err, source.ErrRejected):
		return build.PathBuildpacks, &failure{domain.ReasonSourceRejected, fmt.Errorf("detecting the build path: %w", err)}
	case err != nil:
		return build.PathBuildpacks, &failure{domain.ReasonInternal, fmt.Errorf("detecting the build path: %w", err)}
	case found:
		return build.PathDockerfile, nil
	}
	return build.PathBuildpacks, nil
}

// awaitBuild polls the Job until it ends, and moves the row to pushing when it
// completed. Build output is never read: it stays in the pod's logs, which
// nothing exposes until slice 3 (AC-16).
func (r *Reconciler) awaitBuild(ctx context.Context, dep *Deployment) *failure {
	buildCtx, cancel := context.WithTimeout(ctx, r.opts.BuildTimeout)
	defer cancel()

	name := build.JobName(dep.ID)
	for {
		state, err := r.cluster.JobState(buildCtx, r.buildNamespace(*dep), name)
		if err != nil {
			slog.WarnContext(ctx, "reading the build job failed, retrying", "error", err, "deployment", dep.ID)
		}
		switch {
		case err != nil:
			// Keep polling: a Kubernetes API blip is not a failed build.
		case state == build.JobSucceeded:
			return r.move(ctx, dep, domain.StatePushing)
		case state == build.JobFailed:
			return &failure{domain.ReasonBuildFailed, errors.New("the build job reported failed")}
		case state == build.JobGone:
			// Nothing is behind this row any more, which is exactly what the
			// sweep exists to catch.
			return &failure{domain.ReasonBuildFailed, errors.New("the build job no longer exists")}
		}
		if fail := r.wait(ctx, buildCtx, domain.ReasonBuildFailed); fail != nil {
			return fail
		}
	}
}

// resolveImage asks the registry what the build pushed, refuses an image that
// runs as root, and moves the row to deploying.
func (r *Reconciler) resolveImage(ctx context.Context, dep *Deployment, app App) *failure {
	repo := build.ImageRepo(r.opts.RegistryHost, app.Slug)
	digest, err := r.registry.Digest(ctx, repo, dep.ID)
	if err != nil {
		// A build that reported success and left no manifest behind is a failed
		// build, not something to retry.
		return &failure{domain.ReasonBuildNoDigest, fmt.Errorf("resolving the pushed image: %w", err)}
	}
	user, err := r.registry.ImageUser(ctx, repo, digest)
	if err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("reading the image config: %w", err)}
	}
	// Checked before a single Kubernetes object for this app is composed, so a
	// root image never reaches admission at all (AC-10).
	if domain.RunsAsRoot(user) {
		return &failure{domain.ReasonImageRunsAsRoot, errors.New("the built image declares no non root user")}
	}

	dep.ImageRepo, dep.ImageDigest = repo, digest
	// The path is left as it was: startBuild wrote it, and an empty field here
	// keeps whatever the row already holds rather than clearing it.
	if err := r.deployments.RecordBuild(ctx, dep.ID, "", build.JobName(dep.ID), repo, digest); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("recording the image digest: %w", err)}
	}
	return r.move(ctx, dep, domain.StateDeploying)
}

// deployApp composes the app's objects, applies them, waits for an available
// replica, and marks the deployment healthy.
func (r *Reconciler) deployApp(ctx context.Context, dep *Deployment, app App) *failure {
	if dep.ImageDigest == "" {
		return &failure{domain.ReasonBuildNoDigest, errors.New("no image digest on the deployment")}
	}
	in := r.appInput(app, dep.ImageRepo+"@"+dep.ImageDigest)

	if err := r.cluster.EnsureNamespace(ctx,
		deploy.Namespace(in), deploy.RoleBinding(in), deploy.ResourceQuota(in), deploy.LimitRange(in)); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("preparing the app namespace: %w", err)}
	}
	// Before anything else in the namespace, and fatal if it fails: an app is
	// never running while unpoliced (spec 0008, AC-13).
	if err := r.cluster.ApplyNetworkPolicies(ctx, deploy.DefaultDenyPolicy(in), deploy.AllowPolicy(in)); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("fencing the app namespace: %w", err)}
	}
	pull, err := build.PullSecret(deploy.PullSecretName, deploy.NamespaceName(app.Slug), r.credential())
	if err != nil {
		return &failure{domain.ReasonInternal, err}
	}
	if err := r.cluster.ApplySecret(ctx, pull); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("writing the pull secret: %w", err)}
	}
	if err := r.cluster.ApplyWorkload(ctx, deploy.Deployment(in), deploy.Service(in), deploy.Ingress(in)); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("applying the workload: %w", err)}
	}

	if fail := r.awaitReady(ctx, app); fail != nil {
		return fail
	}
	release, err := r.deployments.MarkHealthy(ctx, dep.ID)
	if err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("marking the deployment healthy: %w", err)}
	}
	dep.State = domain.StateHealthy
	slog.InfoContext(ctx, "deployment healthy",
		"deployment", dep.ID, "app", app.Slug, "release", release.Number, "digest", release.Digest)
	return nil
}

// awaitReady waits for the Deployment to report an available updated replica.
func (r *Reconciler) awaitReady(ctx context.Context, app App) *failure {
	readyCtx, cancel := context.WithTimeout(ctx, r.opts.ReadyTimeout)
	defer cancel()

	namespace := deploy.NamespaceName(app.Slug)
	for {
		ready, err := r.cluster.WorkloadReady(readyCtx, namespace, deploy.WorkloadName)
		if err != nil {
			slog.WarnContext(ctx, "reading the app deployment failed, retrying", "error", err, "app", app.Slug)
		}
		if ready {
			return nil
		}
		if fail := r.wait(ctx, readyCtx, domain.ReasonAppNeverReady); fail != nil {
			return fail
		}
	}
}

// wait sleeps one tick, and turns a deadline into the right reason: the phase's
// own when the phase ran out, and timeout when the whole call did.
func (r *Reconciler) wait(ctx, phase context.Context, onPhaseDeadline domain.Reason) *failure {
	select {
	case <-time.After(r.opts.ReconcileInterval):
		if phase.Err() == nil {
			return nil
		}
	case <-phase.Done():
	}
	if ctx.Err() != nil {
		return &failure{domain.ReasonTimeout, ctx.Err()}
	}
	return &failure{onPhaseDeadline, phase.Err()}
}

// appInput is everything an app's objects are composed from.
func (r *Reconciler) appInput(app App, image string) deploy.Input {
	return deploy.Input{
		AppID:                 app.ID,
		Slug:                  app.Slug,
		Host:                  app.Slug + "." + r.opts.AppDomain,
		Image:                 image,
		IngressClassName:      r.opts.IngressClassName,
		CPU:                   r.opts.CPU,
		Memory:                r.opts.Memory,
		LimitCPU:              r.opts.LimitCPU,
		LimitMemory:           r.opts.LimitMemory,
		QuotaCPU:              r.opts.QuotaCPU,
		QuotaMemory:           r.opts.QuotaMemory,
		QuotaPods:             r.opts.QuotaPods,
		ControlPlaneNamespace: r.opts.ControlPlaneNamespace,
		EgressBlockedCIDRs:    r.opts.EgressBlockedCIDRs,
	}
}

// credential is the one registry login the platform holds.
func (r *Reconciler) credential() build.Credential {
	return build.Credential{Host: r.opts.RegistryHost, User: r.opts.RegistryUser, Password: r.opts.RegistryPass}
}

// move transitions a deployment and keeps the in memory row in step.
func (r *Reconciler) move(ctx context.Context, dep *Deployment, to domain.State) *failure {
	if err := r.deployments.Transition(ctx, dep.ID, to, "", ""); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("moving to %s: %w", to, err)}
	}
	dep.State = to
	return nil
}

// failure pairs the code a caller sees with the error only the platform sees.
type failure struct {
	reason domain.Reason
	err    error
}

// fail records a terminal failure: the code and its one line on the row, the
// wrapped error in the platform's own log and nowhere else (AC-16).
func (r *Reconciler) fail(ctx context.Context, deploymentID string, f *failure) {
	slog.ErrorContext(ctx, "deployment failed", "deployment", deploymentID, "reason", f.reason, "error", f.err)

	// A fresh context: the deploy budget may be exactly what ran out, and the
	// row still has to be written or it stays in flight forever.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := r.deployments.Transition(writeCtx, deploymentID, domain.StateFailed,
		string(f.reason), f.reason.Message())
	switch {
	case err == nil:
	case errors.Is(err, ErrNotInFlight):
		// The row was ended by something else between the failure and this write.
		// It is terminal with a reason already, so there is nothing to record.
		slog.InfoContext(ctx, "the deployment was already ended, so the failure was not recorded",
			"deployment", deploymentID, "reason", f.reason)
	default:
		slog.ErrorContext(ctx, "recording a deployment failure failed", "deployment", deploymentID, "error", err)
	}
}
