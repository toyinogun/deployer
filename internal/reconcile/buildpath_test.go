package reconcile_test

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/store"
)

// queueWith uploads one archive of exactly these entry names and queues a
// deployment for it, which is the only thing that decides which engine runs.
func (w *world) queueWith(t *testing.T, names ...string) reconcile.Deployment {
	t.Helper()
	ctx := t.Context()
	up, err := w.uploads.Accept(ctx, w.accountID, tarball(t, names...))
	if err != nil {
		t.Fatalf("accepting the upload: %v", err)
	}
	dep, _, err := w.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: w.app.ID, AccountID: w.accountID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("queueing the deployment: %v", err)
	}
	return toLoop(dep)
}

// buildJobs is every build Job this world's cluster was actually asked to
// create, in either build namespace. Both, because these tests are about which
// engine ran and not about where it ran: a Job counted in only one namespace
// would make a routing change look like a build that never started. Where each
// one lands is pinned separately, in buildnamespace_test.go.
func (w *world) buildJobs(t *testing.T) []batchv1.Job {
	t.Helper()
	list, err := w.clientset.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing build jobs: %v", err)
	}
	return list.Items
}

// buildPath is what the row says ran, which is the field a caller reads back.
func (w *world) buildPathOf(t *testing.T, id string) string {
	t.Helper()
	dep, err := w.store.GetDeployment(t.Context(), id)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.BuildPath == nil {
		return ""
	}
	return *dep.BuildPath
}

// covers AC-1 and AC-4: an archive with a root Dockerfile runs the BuildKit
// image, and the row says so.
func TestADockerfileArchiveBuildsThroughBuildkit(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	dep := w.queueWith(t, "Dockerfile", "main.go")

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), dep)

	jobs := w.buildJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("build jobs = %d, want one", len(jobs))
	}
	image := jobs[0].Spec.Template.Spec.Containers[0].Image
	if !strings.Contains(image, "buildkit") {
		t.Errorf("build image = %q, want the BuildKit engine", image)
	}
	if got := w.buildPathOf(t, dep.ID); got != "dockerfile" {
		t.Errorf("build_path = %q, want dockerfile", got)
	}
}

// covers AC-2: an archive without one is unchanged. A Dockerfile one directory
// down is not a root Dockerfile, and neither is a differently named one, so all
// of these still go to the lifecycle exactly as they did before this feature.
func TestArchivesWithoutARootDockerfileStillBuildThroughBuildpacks(t *testing.T) {
	for _, names := range [][]string{
		{"main.go", "go.mod"},
		{"deploy/Dockerfile", "main.go"},
		{"Dockerfile.dev", "main.go"},
		{"dockerfile", "main.go"},
	} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			w := setup(t)
			w.buildEnds(batchv1.JobComplete)
			w.appComesUp()
			dep := w.queueWith(t, names...)

			w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), dep)

			jobs := w.buildJobs(t)
			if len(jobs) != 1 {
				t.Fatalf("build jobs = %d, want one", len(jobs))
			}
			image := jobs[0].Spec.Template.Spec.Containers[0].Image
			if !strings.Contains(image, "paketo") {
				t.Errorf("build image = %q, want the Paketo builder", image)
			}
			if got := w.buildPathOf(t, dep.ID); got != "buildpacks" {
				t.Errorf("build_path = %q, want buildpacks", got)
			}
		})
	}
}

// covers AC-4: the path is written before the Job exists, so a deployment that
// failed its build still reports which engine ran it. This is the whole reason
// the path is an input to RecordBuild rather than an output of a finished build.
func TestAFailedDockerfileBuildStillReportsItsPath(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobFailed)
	dep := w.queueWith(t, "Dockerfile")

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), dep)

	row, err := w.store.GetDeployment(t.Context(), dep.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if row.State != string(domain.StateFailed) {
		t.Errorf("state = %q, want failed", row.State)
	}
	if row.FailureReason == nil || *row.FailureReason != string(domain.ReasonBuildFailed) {
		t.Errorf("failure reason = %v, want build_failed", row.FailureReason)
	}
	if got := w.buildPathOf(t, dep.ID); got != "dockerfile" {
		t.Errorf("build_path = %q on a failed build, want dockerfile", got)
	}
}

// covers AC-6: an archive the walk cannot read is refused as source_rejected
// before any Job or credential exists. It passes upload, which only checks the
// gzip magic, and fails here, which is the first place the bytes are parsed.
func TestAnUnreadableArchiveIsRefusedBeforeAnyJob(t *testing.T) {
	w := setup(t)
	ctx := t.Context()
	up, err := w.uploads.Accept(ctx, w.accountID, strings.NewReader("\x1f\x8b"+strings.Repeat("x", 64)))
	if err != nil {
		t.Fatalf("accepting the upload: %v", err)
	}
	stored, _, err := w.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: w.app.ID, AccountID: w.accountID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("queueing the deployment: %v", err)
	}

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(stored))

	row, err := w.store.GetDeployment(ctx, stored.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if row.FailureReason == nil || *row.FailureReason != string(domain.ReasonSourceRejected) {
		t.Errorf("failure reason = %v, want source_rejected", row.FailureReason)
	}
	if jobs := w.buildJobs(t); len(jobs) != 0 {
		t.Errorf("build jobs = %d, want none for an archive the platform refused", len(jobs))
	}
	for _, a := range w.clientset.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "secrets" {
			t.Error("a push credential was created for a refused archive")
		}
	}
}

// covers AC-3: the walk stops at the extractor's own entry limit rather than
// reading to the end, so a header only archive cannot make the control plane work
// without bound. It is refused, not answered, because an archive that will be
// refused at extraction must not choose an engine on the way there.
func TestAnArchiveOverTheEntryLimitIsRefused(t *testing.T) {
	w := setup(t)
	ctx := t.Context()

	names := make([]string, 0, 12)
	for i := range 12 {
		names = append(names, "file"+string(rune('a'+i)))
	}
	up, err := w.uploads.Accept(ctx, w.accountID, tarball(t, names...))
	if err != nil {
		t.Fatalf("accepting the upload: %v", err)
	}
	stored, _, err := w.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: w.app.ID, AccountID: w.accountID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("queueing the deployment: %v", err)
	}

	// The same number the extractor enforces, set low so the archive above is
	// over it.
	loop := w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, func(o *reconcile.Options) {
		o.MaxUploadFiles = 5
	})
	loop.Drive(ctx, toLoop(stored))

	row, err := w.store.GetDeployment(ctx, stored.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if row.FailureReason == nil || *row.FailureReason != string(domain.ReasonSourceRejected) {
		t.Errorf("failure reason = %v, want source_rejected", row.FailureReason)
	}
	if jobs := w.buildJobs(t); len(jobs) != 0 {
		t.Errorf("build jobs = %d, want none", len(jobs))
	}
}

// covers AC-12: an image a Dockerfile build produced that declares no non root
// user is refused, and refused before a single object for the app is composed.
// Spec 0004 wrote this refusal and could never reach it, because a Buildpacks
// image always declares a user. The Dockerfile path is the first thing that can
// actually push a root image, so this is the case that closes it, and the row
// still says which engine got it there.
func TestARootImageFromADockerfileBuildIsRefusedBeforeAnyAppObject(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	ctx := t.Context()
	dep := w.queueWith(t, "Dockerfile", "main.go")

	w.reconciler(fakeRegistry{digest: testDigest, user: "0"}).Drive(ctx, dep)

	row, err := w.store.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if row.FailureReason == nil || *row.FailureReason != string(domain.ReasonImageRunsAsRoot) {
		t.Fatalf("failure reason = %v, want image_runs_as_root", row.FailureReason)
	}
	if got := w.buildPathOf(t, dep.ID); got != "dockerfile" {
		t.Errorf("build_path = %q on a refused root image, want dockerfile", got)
	}

	// Nothing for this app reached the cluster, which is the half of the
	// criterion that matters: the refusal is the platform's, not admission's.
	list, err := w.clientset.AppsV1().Deployments("app-"+w.app.Slug).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing deployments: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("app deployments = %d, want none for an image the platform refused", len(list.Items))
	}
}

// covers AC-14: build output stays in the Job's pod logs on both paths. The
// strongest thing this suite can say about that is that the platform never asks
// for them at all: a failed build is ended from the Job's own condition, so
// there is no read for output to escape from, and the row carries the closed
// code rather than anything the build printed. The fake clientset serves no
// real logs, so the response and log half of this is proved live, not here.
func TestNoBuildOutputIsEverReadOnEitherPath(t *testing.T) {
	for name, names := range map[string][]string{
		"dockerfile": {"Dockerfile", "main.go"},
		"buildpacks": {"main.go", "go.mod"},
	} {
		t.Run(name, func(t *testing.T) {
			w := setup(t)
			w.buildEnds(batchv1.JobFailed)
			ctx := t.Context()
			dep := w.queueWith(t, names...)

			w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, dep)

			row, err := w.store.GetDeployment(ctx, dep.ID)
			if err != nil {
				t.Fatalf("reading the deployment back: %v", err)
			}
			if row.FailureReason == nil || *row.FailureReason != string(domain.ReasonBuildFailed) {
				t.Fatalf("failure reason = %v, want build_failed", row.FailureReason)
			}
			// The stored reason is the code alone. A row carrying a wrapped
			// error string is build detail that reached the database.
			if !domain.Reason(*row.FailureReason).Valid() {
				t.Errorf("failure reason %q is outside the closed set", *row.FailureReason)
			}

			for _, a := range w.clientset.Actions() {
				if a.GetSubresource() == "log" {
					t.Errorf("the platform read pod logs (%s %s/%s) during a failed build",
						a.GetVerb(), a.GetResource().Resource, a.GetSubresource())
				}
			}
		})
	}
}
