package reconcile_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/registry"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/uploads"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/toyinogun/deployer/internal/kube"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fakeRegistry stands in for the in cluster registry: it answers with whatever
// the test wants a build to have pushed.
type fakeRegistry struct {
	digest string
	user   string
	err    error
}

func (f fakeRegistry) Digest(context.Context, string, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.digest, nil
}

func (f fakeRegistry) ImageUser(context.Context, string, string) (string, error) {
	return f.user, nil
}

// world is one test's real database, real upload volume, and fake cluster.
type world struct {
	store      *store.Store
	clientset  *fake.Clientset
	uploads    *uploads.Service
	deployment store.Deployment
	app        store.App
	accountID  string
	uploadPath string
	// deployments replaces the store the loop writes deployments through, and is
	// only ever set to a passthrough over that same real store that fails one
	// named call. See failOnce in stranded_test.go.
	deployments reconcile.Deployments
}

// setup builds a world with one account, one app, one uploaded tarball, and one
// queued deployment: exactly the state deploy_app leaves behind.
func setup(t *testing.T) *world {
	t.Helper()
	ctx := t.Context()

	dir := t.TempDir()
	st, err := store.Open(store.Options{Path: filepath.Join(dir, "deployer.db"), BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	account, err := st.CreateAccount(ctx, "bootstrap")
	if err != nil {
		t.Fatalf("creating the account: %v", err)
	}
	app, err := st.CreateApp(ctx, account.ID, "hello")
	if err != nil {
		t.Fatalf("creating the app: %v", err)
	}

	uploadDir := filepath.Join(dir, "uploads")
	svc := uploads.NewService(store.ForUploads(st), uploadDir, 1<<20, nil)
	// A real file on the volume, because the loop deletes it at the end and the
	// test asserts that it did (AC-22). It is a readable gzip tar rather than two
	// magic bytes, because the loop now walks its headers to choose a build
	// engine, and an archive it cannot read is refused before any Job exists.
	up, err := svc.Accept(ctx, account.ID, tarball(t, "main.go", "go.mod"))
	if err != nil {
		t.Fatalf("accepting the upload: %v", err)
	}

	dep, _, err := st.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: app.ID, AccountID: account.ID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("creating the deployment: %v", err)
	}

	return &world{
		store:      st,
		clientset:  fake.NewClientset(),
		uploads:    svc,
		deployment: dep,
		app:        app,
		accountID:  account.ID,
		uploadPath: up.Path,
	}
}

// tarball is a readable gzip tar holding one regular entry per name, which is
// the least an upload can be now that the loop reads the archive to pick an
// engine. Entry bodies are one byte: nothing here unpacks them.
func tarball(t *testing.T, names ...string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("writing the %s header: %v", name, err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing the gzip: %v", err)
	}
	return &buf
}

// buildEnds makes every build Job read back with one condition, which is how the
// loop learns a build ended without the test running a scheduler.
func (w *world) buildEnds(condition batchv1.JobConditionType) {
	w.clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: condition, Status: corev1.ConditionTrue},
			}},
		}, nil
	})
}

// buildNeverEnds makes every build Job read back with no condition at all, which
// is a Job still running. Nothing here schedules anything, so the only way that
// build ends is a deadline.
func (w *world) buildNeverEnds() {
	w.clientset.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.GetAction).GetName()
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
		}, nil
	})
}

// appComesUp makes every app Deployment read back as a finished rollout: the
// new pods exist, none of the old ones are left, and the new ones are available.
// Replicas has to equal UpdatedReplicas, because a status where they differ is a
// rollout still in progress and the readiness check refuses it (spec 0011, AC-17).
func (w *world) appComesUp() {
	w.clientset.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.GetAction).GetName()
		return true, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
			Status: appsv1.DeploymentStatus{
				Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1,
			},
		}, nil
	})
}

// reconciler wires the loop over this world's real store and fake cluster. The
// tweaks run over the options before the loop is built, so a test that cares
// about one budget states only that one.
func (w *world) reconciler(reg reconcile.Registry, tweaks ...func(*reconcile.Options)) *reconcile.Reconciler {
	var deployments reconcile.Deployments = store.ForReconcile(w.store)
	if w.deployments != nil {
		deployments = w.deployments
	}
	rs := store.ForReconcile(w.store)
	opts := reconcile.Options{
		PodName:               "deployer-0",
		ControlPlaneNamespace: "deployer-system",
		BuildNamespace:        "deployer-builds",
		BuildkitNamespace:     "deployer-builds-dockerfile",
		AppDomain:             "deploy.example.org",
		IngressClassName:      "nginx",
		SelfImage:             "ghcr.io/x/deployer@" + testDigest,
		BuilderImage:          "paketobuildpacks/builder@" + testDigest,
		BuildUID:              1001,
		BuildGID:              1000,
		BuildkitImage:         "moby/buildkit@" + testDigest,
		BuildkitUID:           1000,
		BuildkitGID:           1000,
		InternalURL:           "http://deployer.deployer-system.svc",
		RegistryHost:          "registry.deployer-system:5000",
		RegistryUser:          "deployer",
		RegistryPass:          "secret",
		DeployTimeout:         20 * time.Second,
		BuildTimeout:          10 * time.Second,
		ReadyTimeout:          5 * time.Second,
		ReconcileInterval:     time.Millisecond,
		MaxUploadFiles:        100,
		MaxExtractedBytes:     1 << 20,
		CPU:                   "100m",
		Memory:                "128Mi",
		LimitCPU:              "500m",
		LimitMemory:           "512Mi",
		QuotaCPU:              "1",
		QuotaMemory:           "1Gi",
		QuotaPods:             5,
		EgressBlockedCIDRs:    []string{"10.0.0.0/8"},
	}
	for _, tweak := range tweaks {
		tweak(&opts)
	}
	return reconcile.New(deployments, rs, uploadsFor{w.uploads}, reg, kube.NewFor(w.clientset), opts)
}

// uploadsFor adapts the upload service the way the composition root does.
type uploadsFor struct{ svc *uploads.Service }

func (u uploadsFor) Get(ctx context.Context, id string) (reconcile.Upload, error) {
	up, err := u.svc.Get(ctx, id)
	if err != nil {
		return reconcile.Upload{}, err
	}
	return reconcile.Upload{ID: up.ID, Path: up.Path, SHA256: up.SHA256}, nil
}

func (u uploadsFor) Open(path string) (io.ReadCloser, error) { return u.svc.Open(path) }

func (u uploadsFor) MintFetchToken(ctx context.Context, id string) (string, error) {
	return u.svc.MintFetchToken(ctx, id)
}

func (u uploadsFor) Remove(ctx context.Context, path string) { u.svc.Remove(ctx, path) }

func TestOneDeployWalksTheWholeMachine(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	ctx := t.Context()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	dep, err := w.store.GetDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("state = %s, want healthy (failure reason %v)", dep.State, dep.FailureReason)
	}

	// Every state, once, in order, and none skipped (AC-5).
	events, err := w.store.ListDeploymentEvents(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the event log: %v", err)
	}
	want := []string{"queued", "building", "pushing", "deploying", "healthy"}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d", len(events), len(want))
	}
	for i, state := range want {
		if events[i].ToState != state {
			t.Errorf("event %d = %s, want %s", i, events[i].ToState, state)
		}
	}

	// Exactly one release, numbered from the start (AC-14).
	rel, err := w.store.GetReleaseByDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	if rel.ReleaseNumber != 1 || rel.ImageDigest != testDigest {
		t.Errorf("release = %d/%s, want 1/%s", rel.ReleaseNumber, rel.ImageDigest, testDigest)
	}

	// The composed workload runs the digest, not the tag it was pushed under.
	// Listed rather than fetched, because a get is what the readiness stub above
	// is intercepting.
	created, err := w.clientset.AppsV1().Deployments("app-"+w.app.Slug).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("reading the composed deployment: %v", err)
	}
	if len(created.Items) != 1 {
		t.Fatalf("composed deployments = %d, want 1", len(created.Items))
	}
	image := created.Items[0].Spec.Template.Spec.Containers[0].Image
	if !strings.HasSuffix(image, "@"+testDigest) {
		t.Errorf("image = %q, want a digest reference", image)
	}

	// The tarball is gone whatever the outcome was (AC-22).
	if _, err := os.Stat(w.uploadPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the tarball is still on the volume: %v", err)
	}
}

func TestARootImageIsRefusedBeforeAnythingIsComposed(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	ctx := t.Context()

	w.reconciler(fakeRegistry{digest: testDigest, user: "0"}).Drive(ctx, toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonImageRunsAsRoot))
	// Nothing for this app exists in the cluster, which is the point: the check
	// runs before any workload is composed (AC-10).
	list, err := w.clientset.AppsV1().Deployments("app-"+w.app.Slug).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing deployments: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("deployments = %d, want none", len(list.Items))
	}
}

func TestAFailedBuildFailsTheDeployment(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobFailed)

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonBuildFailed))
}

func TestABuildThatPushedNothingFailsWithNoDigest(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)

	w.reconciler(fakeRegistry{err: registry.ErrNoDigest}).Drive(t.Context(), toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonBuildNoDigest))
}

// A restart mid build with the Job gone must resolve the row rather than leave
// it in flight forever (AC-18).
func TestTheSweepFailsADeploymentWhoseJobVanished(t *testing.T) {
	w := setup(t)
	ctx := t.Context()
	if _, err := w.store.Transition(ctx, w.deployment.ID, domain.StateBuilding, "", ""); err != nil {
		t.Fatalf("moving to building: %v", err)
	}

	// No reactor, so the fake cluster holds no Job at all.
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Sweep(ctx)

	assertFailed(t, w, string(domain.ReasonBuildFailed))
}

// The three tests below are AC-17: each of the three budgets fails a deployment
// with its own reason, so an operator reading a failed row can tell which one
// ran out. `/check verify` proved this against the real cluster once; these keep
// it true, because the whole attribution lives in one small `select` and a
// change to its branch order is invisible to every other test here.

// The build's own deadline passing, while the whole deploy still had budget.
func TestABuildThatOutlastsItsOwnBudgetFailsAsABuild(t *testing.T) {
	w := setup(t)
	w.buildNeverEnds()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, func(o *reconcile.Options) {
		o.BuildTimeout = 50 * time.Millisecond
		o.DeployTimeout = 30 * time.Second
	}).Drive(t.Context(), toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonBuildFailed))
}

// The Deployment never reporting an available replica, while the whole deploy
// still had budget. Named as itself so it is not confused with a failed build.
func TestAnAppThatNeverComesUpFailsAsNeverReady(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	// No appComesUp: the composed Deployment reads back with no ready replica,
	// which is what an app that crashes on boot looks like from here.

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, func(o *reconcile.Options) {
		o.ReadyTimeout = 50 * time.Millisecond
		o.DeployTimeout = 30 * time.Second
	}).Drive(t.Context(), toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonAppNeverReady))
}

// The whole deploy budget running out first, which outranks the phase's own
// reason: the build had not overrun anything, the call did.
func TestTheDeployBudgetRunningOutIsATimeoutNotAPhaseFailure(t *testing.T) {
	w := setup(t)
	w.buildNeverEnds()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, func(o *reconcile.Options) {
		// Generous enough that the queued to building writes cannot be what
		// expires, short enough that the test does not sit here.
		o.DeployTimeout = time.Second
		o.BuildTimeout = 30 * time.Second
	}).Drive(t.Context(), toLoop(w.deployment))

	assertFailed(t, w, string(domain.ReasonTimeout))
}

// assertFailed checks the row ended failed with that code, and that nothing but
// the code crossed the boundary (AC-16).
func assertFailed(t *testing.T, w *world, code string) {
	t.Helper()
	dep, err := w.store.GetDeployment(t.Context(), w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateFailed) {
		t.Fatalf("state = %s, want failed", dep.State)
	}
	if dep.FailureReason == nil || *dep.FailureReason != code {
		t.Fatalf("failure reason = %v, want %s", dep.FailureReason, code)
	}
	if !domain.Reason(*dep.FailureReason).Valid() {
		t.Errorf("failure reason %q is outside the closed set", *dep.FailureReason)
	}
}

// The init container runs on cluster DNS, which cannot resolve the tailnet name
// the public address carries, so the fetch address has to come from the in
// cluster one. Taking it from DEPLOYER_PUBLIC_URL made every real build die in
// its init container with "no such host", while every fake clientset test
// stayed green, because nothing here resolves a name.
func TestTheBuildFetchesThroughTheInClusterAddress(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	ctx := t.Context()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	jobs, err := w.clientset.BatchV1().Jobs("deployer-builds").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("reading the build job: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("build jobs = %d, want 1", len(jobs.Items))
	}
	var fetch string
	for _, env := range jobs.Items[0].Spec.Template.Spec.InitContainers[0].Env {
		if env.Name == "DEPLOYER_FETCH_URL" {
			fetch = env.Value
		}
	}
	want := "http://deployer.deployer-system.svc/v1/uploads/" + *w.deployment.UploadID
	if fetch != want {
		t.Errorf("DEPLOYER_FETCH_URL = %q, want %q", fetch, want)
	}
}

// toLoop is what the store adapter hands the loop, built here so a test can
// drive one deployment without going through a claim.
//
// It maps every field the adapter does, including the ones only a rollback
// uses: a helper that quietly dropped source_release_id would make a rollback
// look like a build deploy to every test in this package, which is the same
// mistake AC-24 is about in the real code.
func toLoop(d store.Deployment) reconcile.Deployment {
	created, err := time.Parse(time.RFC3339Nano, d.CreatedAt)
	if err != nil {
		panic("a stored created_at that will not parse: " + d.CreatedAt)
	}
	return reconcile.Deployment{
		ID:              d.ID,
		AppID:           d.AppID,
		UploadID:        orEmpty(d.UploadID),
		State:           domain.State(d.State),
		ImageRepo:       orEmpty(d.ImageRepo),
		ImageDigest:     orEmpty(d.ImageDigest),
		CreatedAt:       created,
		BuildJobName:    orEmpty(d.BuildJobName),
		BuildPath:       orEmpty(d.BuildPath),
		SourceReleaseID: orEmpty(d.SourceReleaseID),
	}
}

// orEmpty reads a nullable column as the empty string the loop's fields use.
func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
