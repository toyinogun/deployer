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
	"log/slog"
	"time"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
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

// Deployment is the row this package drives, carrying only the fields it reads.
type Deployment struct {
	ID          string
	AppID       string
	UploadID    string
	State       domain.State
	ImageRepo   string
	ImageDigest string
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
	// RecordBuild stores what the build produced without moving the row.
	RecordBuild(ctx context.Context, id, jobName, imageRepo, imageDigest string) error
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
	ApplyWorkload(ctx context.Context, d *appsv1.Deployment, s *corev1.Service, i *networkingv1.Ingress) error
	WorkloadReady(ctx context.Context, namespace, name string) (bool, error)
}

// Options is everything the loop needs from configuration.
type Options struct {
	PodName               string
	ControlPlaneNamespace string
	BuildNamespace        string
	AppDomain             string
	IngressClassName      string

	SelfImage    string
	BuilderImage string
	PublicURL    string

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

// tick claims one deployment and drives it to a terminal state.
func (r *Reconciler) tick(ctx context.Context) {
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

// Drive takes one deployment as far as it goes, and records the outcome whatever
// it is. Every exit from here is terminal, so nothing this touches stays queued.
func (r *Reconciler) Drive(ctx context.Context, dep Deployment) {
	// The whole deploy budget, which is also what the blocking handler is waiting
	// on. A phase that overruns its own budget fails as itself; overrunning this
	// one is a timeout (AC-17).
	runCtx, cancel := context.WithTimeout(ctx, r.opts.DeployTimeout)
	defer cancel()

	upload, uploadErr := r.uploads.Get(runCtx, dep.UploadID)
	if fail := r.run(runCtx, &dep, upload, uploadErr); fail != nil {
		r.fail(ctx, dep.ID, fail)
	}
	// Terminal either way, so the tarball has served its purpose (AC-22).
	r.uploads.Remove(ctx, upload.Path)
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
		if fail := r.startBuild(ctx, dep, app, upload); fail != nil {
			return fail
		}
	}
	if dep.State == domain.StateBuilding {
		if fail := r.awaitBuild(ctx, dep); fail != nil {
			return fail
		}
	}
	if dep.State == domain.StatePushing {
		if fail := r.resolveImage(ctx, dep, app); fail != nil {
			return fail
		}
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

	token, err := r.uploads.MintFetchToken(ctx, upload.ID)
	if err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("minting a fetch token: %w", err)}
	}
	target := build.TargetImage(r.opts.RegistryHost, app.Slug, dep.ID)
	job := build.Job(build.Input{
		DeploymentID:    dep.ID,
		Namespace:       r.opts.BuildNamespace,
		AppSlug:         app.Slug,
		SelfImage:       r.opts.SelfImage,
		BuilderImage:    r.opts.BuilderImage,
		TargetImage:     target,
		FetchURL:        r.opts.PublicURL + "/v1/uploads/" + upload.ID,
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
	secret, err := build.BuildCredentialSecret(dep.ID, r.opts.BuildNamespace, r.credential(), owner)
	if err != nil {
		return &failure{domain.ReasonInternal, err}
	}
	if err := r.cluster.ApplySecret(ctx, secret); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("writing the build credential: %w", err)}
	}

	dep.ImageRepo = build.ImageRepo(r.opts.RegistryHost, app.Slug)
	if err := r.deployments.RecordBuild(ctx, dep.ID, build.JobName(dep.ID), dep.ImageRepo, ""); err != nil {
		return &failure{domain.ReasonInternal, fmt.Errorf("recording the build job: %w", err)}
	}
	return nil
}

// awaitBuild polls the Job until it ends, and moves the row to pushing when it
// completed. Build output is never read: it stays in the pod's logs, which
// nothing exposes until slice 3 (AC-16).
func (r *Reconciler) awaitBuild(ctx context.Context, dep *Deployment) *failure {
	buildCtx, cancel := context.WithTimeout(ctx, r.opts.BuildTimeout)
	defer cancel()

	name := build.JobName(dep.ID)
	for {
		state, err := r.cluster.JobState(buildCtx, r.opts.BuildNamespace, name)
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
	if err := r.deployments.RecordBuild(ctx, dep.ID, build.JobName(dep.ID), repo, digest); err != nil {
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
	if err := r.deployments.Transition(writeCtx, deploymentID, domain.StateFailed,
		string(f.reason), f.reason.Message()); err != nil {
		slog.ErrorContext(ctx, "recording a deployment failure failed", "deployment", deploymentID, "error", err)
	}
}
