package reconcile_test

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// errNoRegistry is what a rollback's registry must never be asked, so a test can
// prove the resolve step never ran rather than only that the outcome was right.
var errNoRegistry = errors.New("a rollback asked the registry to resolve an image")

// jobNames lists every build Job in the cluster, so a test can show a rollback
// added none.
func (w *world) jobNames(t *testing.T) []string {
	t.Helper()
	jobs, err := w.clientset.BatchV1().Jobs("").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing build jobs: %v", err)
	}
	names := make([]string, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		names = append(names, j.Name)
	}
	return names
}

// podChecksum is the configuration checksum on the app's current pod template,
// which is what makes a Deployment roll when the image digest has not moved.
func (w *world) podChecksum(t *testing.T) string {
	t.Helper()
	list, err := w.clientset.AppsV1().
		Deployments(deploy.NamespaceName(w.app.Slug)).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("reading the composed deployment: %v (%d items)", err, len(list.Items))
	}
	return list.Items[0].Spec.Template.Annotations[deploy.ConfigChecksumAnnotation]
}

// podsNeverComeUp makes every app Deployment read back with no available
// replica, which is a rollout that never finishes. It is prepended after
// appComesUp, so it wins.
func (w *world) podsNeverComeUp() {
	w.clientset.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.GetAction).GetName()
		return true, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: action.GetNamespace()},
		}, nil
	})
}

// firstRelease deploys the world's queued build deploy the whole way and hands
// back the release it minted, which is what a rollback later names.
func (w *world) firstRelease(t *testing.T) store.Release {
	t.Helper()
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), toLoop(w.deployment))

	rel, err := w.store.GetReleaseByDeployment(t.Context(), w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the release the first deploy minted: %v", err)
	}
	return rel
}

// deployAgain runs a second build deploy of the same app, with whatever
// configuration the table now holds, so a later rollback has something to move
// the app away from.
func (w *world) deployAgain(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	up, err := w.uploads.Accept(ctx, w.accountID, tarball(t, "main.go", "go.mod"))
	if err != nil {
		t.Fatalf("accepting the second upload: %v", err)
	}
	dep, _, err := w.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: w.app.ID, AccountID: w.accountID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("creating the second deployment: %v", err)
	}
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(dep))

	current, err := w.store.GetDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("reading the second deployment: %v", err)
	}
	if current.State != string(domain.StateHealthy) {
		t.Fatalf("the second deploy ended %s with reason %v, want healthy",
			current.State, current.FailureReason)
	}
}

// queueRollback writes the rollback row the tool would write.
func (w *world) queueRollback(t *testing.T, releaseID string) store.Deployment {
	t.Helper()
	dep, _, err := w.store.CreateDeployment(t.Context(), store.CreateDeploymentInput{
		AppID: w.app.ID, AccountID: w.accountID, SourceReleaseID: &releaseID,
	})
	if err != nil {
		t.Fatalf("creating the rollback: %v", err)
	}
	return dep
}

// TestARollbackTakesTheShortPathAndNeverBuilds drives one rollback end to end
// and pins the whole of what makes it a rollback: no Job, no upload read, the
// stored digest re promoted, and a timeline of exactly three states.
func TestARollbackTakesTheShortPathAndNeverBuilds(t *testing.T) {
	// covers: spec 0011 AC-9, AC-11, AC-16, AC-19
	w := setup(t)
	ctx := t.Context()

	if err := w.store.SetConfig(ctx, w.app.ID, "LOG_LEVEL", "info", false); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	first := w.firstRelease(t)

	// The build Jobs that exist so far, so the rollback can be shown to add none.
	jobsBefore := w.jobNames(t)

	rollback := w.queueRollback(t, first.ID)
	// A registry that would fail loudly if the rollback ever tried to resolve an
	// image: it must not, because nothing was pushed.
	w.reconciler(fakeRegistry{err: errNoRegistry}).Drive(ctx, toLoop(rollback))

	dep, err := w.store.GetDeployment(ctx, rollback.ID)
	if err != nil {
		t.Fatalf("reading the rollback back: %v", err)
	}
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("the rollback ended %s with reason %v, want healthy", dep.State, dep.FailureReason)
	}
	if dep.UploadID != nil {
		t.Error("the rollback names an upload, which it can never have")
	}
	if dep.ImageDigest == nil || *dep.ImageDigest != first.ImageDigest {
		t.Errorf("the rollback carries digest %v, want release 1's %q", dep.ImageDigest, first.ImageDigest)
	}

	if got := w.jobNames(t); len(got) != len(jobsBefore) {
		t.Errorf("the rollback created %d build jobs, want none", len(got)-len(jobsBefore))
	}

	events, err := w.store.ListDeploymentEvents(ctx, rollback.ID)
	if err != nil {
		t.Fatalf("reading the timeline: %v", err)
	}
	want := []string{
		string(domain.StateQueued), string(domain.StateDeploying), string(domain.StateHealthy),
	}
	if len(events) != len(want) {
		t.Fatalf("the rollback's timeline is %d events, want %d", len(events), len(want))
	}
	for i, e := range events {
		if e.ToState != want[i] {
			t.Errorf("timeline[%d] is %q, want %q", i, e.ToState, want[i])
		}
	}

	// A release of its own, with the source release's digest, and the source
	// release untouched (AC-16).
	rel, err := w.store.GetReleaseByDeployment(ctx, rollback.ID)
	if err != nil {
		t.Fatalf("reading the rollback's release: %v", err)
	}
	if rel.ReleaseNumber != first.ReleaseNumber+1 {
		t.Errorf("the rollback is release %d, want %d: numbers are never reused",
			rel.ReleaseNumber, first.ReleaseNumber+1)
	}
	if rel.ImageDigest != first.ImageDigest {
		t.Errorf("the rollback's release records %q, want %q", rel.ImageDigest, first.ImageDigest)
	}
	source, err := w.store.GetRelease(ctx, first.ID)
	if err != nil {
		t.Fatalf("reading the source release: %v", err)
	}
	if source.ConfigSnapshot != first.ConfigSnapshot || source.ImageDigest != first.ImageDigest {
		t.Error("the rollback rewrote the release it came from")
	}
}

// TestARollbackRestoresTheReleasesConfiguration is the fidelity case: the app's
// configuration moved on, and the rollback puts back exactly what that release
// ran with, in the container and in the table alike.
func TestARollbackRestoresTheReleasesConfiguration(t *testing.T) {
	// covers: spec 0011 AC-12, AC-13
	w := setup(t)
	ctx := t.Context()

	if err := w.store.SetConfigBatch(ctx, w.app.ID, []store.ConfigEntry{
		{Key: "LOG_LEVEL", Value: "info", IsSecret: false},
		{Key: "API_KEY", Value: "old-key", IsSecret: true},
	}); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	first := w.firstRelease(t)

	// The environment moves on: one key changes, one is added, one is removed,
	// and a second deploy puts that configuration in the cluster. The rollback
	// has to move the app back from here, not from where it started.
	if err := w.store.SetConfigBatch(ctx, w.app.ID, []store.ConfigEntry{
		{Key: "LOG_LEVEL", Value: "debug", IsSecret: false},
		{Key: "FEATURE_X", Value: "on", IsSecret: false},
	}); err != nil {
		t.Fatalf("changing configuration: %v", err)
	}
	if err := w.store.UnsetConfig(ctx, w.app.ID, "API_KEY"); err != nil {
		t.Fatalf("removing a key: %v", err)
	}
	w.deployAgain(t)
	// What is running now, and what the rollback has to change to roll the pods.
	// The image digest is the same on both, so the checksum is the only thing
	// that can (AC-12).
	runningChecksum := w.podChecksum(t)

	rollback := w.queueRollback(t, first.ID)
	w.reconciler(fakeRegistry{err: errNoRegistry}).Drive(ctx, toLoop(rollback))

	dep, err := w.store.GetDeployment(ctx, rollback.ID)
	if err != nil {
		t.Fatalf("reading the rollback: %v", err)
	}
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("the rollback ended %s, want healthy", dep.State)
	}

	// The Secret the container was given is the release's, not the table's.
	secret, err := w.clientset.CoreV1().
		Secrets(deploy.NamespaceName(w.app.Slug)).Get(ctx, deploy.ConfigSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the configuration secret: %v", err)
	}
	if got := string(secret.Data["LOG_LEVEL"]); got != "info" {
		t.Errorf("the container was given LOG_LEVEL=%q, want the release's info", got)
	}
	if got := string(secret.Data["API_KEY"]); got != "old-key" {
		t.Errorf("the container was given API_KEY=%q, want the release's old-key", got)
	}
	if _, ok := secret.Data["FEATURE_X"]; ok {
		t.Error("the container was given FEATURE_X, which the release never had")
	}

	// The checksum is what rolls the pods, because the digest did not change
	// (AC-12).
	if after := w.podChecksum(t); after == runningChecksum {
		t.Error("the pod template checksum is unchanged, so the pods would not roll " +
			"even though the configuration did")
	}

	// app_config now agrees with what is running (AC-13).
	entries, err := w.store.ListConfigForDeploy(ctx, w.app.ID)
	if err != nil {
		t.Fatalf("reading configuration back: %v", err)
	}
	got := map[string]store.ConfigEntry{}
	for _, e := range entries {
		got[e.Key] = e
	}
	if len(got) != 2 {
		t.Fatalf("app_config holds %d keys after the rollback, want exactly the release's 2: %+v", len(got), got)
	}
	if got["LOG_LEVEL"].Value != "info" || got["LOG_LEVEL"].IsSecret {
		t.Errorf("LOG_LEVEL came back as %+v, want info and not secret", got["LOG_LEVEL"])
	}
	if got["API_KEY"].Value != "old-key" || !got["API_KEY"].IsSecret {
		t.Errorf("API_KEY came back as %+v, want old-key and secret: the flag rides in the snapshot",
			got["API_KEY"])
	}
	if _, ok := got["FEATURE_X"]; ok {
		t.Error("FEATURE_X survived a rollback to a release that never had it")
	}
}

// TestAFailedRollbackLeavesTheKnownGoodStateAlone is the invariant the whole
// feature turns on: one bad recovery attempt cannot poison the history.
func TestAFailedRollbackLeavesTheKnownGoodStateAlone(t *testing.T) {
	// covers: spec 0011 AC-17
	w := setup(t)
	ctx := t.Context()

	if err := w.store.SetConfig(ctx, w.app.ID, "LOG_LEVEL", "info", false); err != nil {
		t.Fatalf("setting configuration: %v", err)
	}
	first := w.firstRelease(t)

	appBefore, err := w.store.GetApp(ctx, w.app.ID)
	if err != nil {
		t.Fatalf("reading the app: %v", err)
	}
	if err := w.store.SetConfig(ctx, w.app.ID, "LOG_LEVEL", "debug", false); err != nil {
		t.Fatalf("changing configuration: %v", err)
	}

	// The pods never come up, so the rollback runs out of its readiness wait.
	w.podsNeverComeUp()
	rollback := w.queueRollback(t, first.ID)
	w.reconciler(fakeRegistry{err: errNoRegistry}).Drive(ctx, toLoop(rollback))

	dep, err := w.store.GetDeployment(ctx, rollback.ID)
	if err != nil {
		t.Fatalf("reading the rollback: %v", err)
	}
	if dep.State != string(domain.StateFailed) {
		t.Fatalf("the rollback ended %s, want failed", dep.State)
	}
	if dep.FailureReason == nil || *dep.FailureReason != string(domain.ReasonAppNeverReady) {
		t.Errorf("the rollback failed with reason %v, want app_never_ready", dep.FailureReason)
	}

	if _, err := w.store.GetReleaseByDeployment(ctx, rollback.ID); err == nil {
		t.Error("the failed rollback minted a release")
	}
	appAfter, err := w.store.GetApp(ctx, w.app.ID)
	if err != nil {
		t.Fatalf("reading the app back: %v", err)
	}
	if orEmpty(appAfter.CurrentReleaseID) != orEmpty(appBefore.CurrentReleaseID) {
		t.Errorf("the failed rollback moved current_release_id from %v to %v",
			appBefore.CurrentReleaseID, appAfter.CurrentReleaseID)
	}
	entries, err := w.store.ListConfigForDeploy(ctx, w.app.ID)
	if err != nil {
		t.Fatalf("reading configuration: %v", err)
	}
	if len(entries) != 1 || entries[0].Value != "debug" {
		t.Errorf("the failed rollback rewrote app_config to %+v, want the untouched debug", entries)
	}
}

// TestARestartResumesARollbackAsARollback is the resume path: a control plane
// killed mid rollback has to pick the row back up knowing what it is, or it
// fails a rollback for having no upload.
func TestARestartResumesARollbackAsARollback(t *testing.T) {
	// covers: spec 0011 AC-24, AC-11
	w := setup(t)
	ctx := t.Context()
	first := w.firstRelease(t)

	rollback := w.queueRollback(t, first.ID)
	// Where a killed control plane would have left it: past queued, mid rollout.
	if _, err := w.store.Transition(ctx, rollback.ID, domain.StateDeploying, "", ""); err != nil {
		t.Fatalf("putting the rollback mid rollout: %v", err)
	}

	// The sweep rebuilds the row from the store, which is the path that has to
	// carry source_release_id through.
	rows, err := store.ForReconcile(w.store).ListNonTerminal(ctx)
	if err != nil {
		t.Fatalf("listing in flight deployments: %v", err)
	}
	var resumed bool
	for _, row := range rows {
		if row.ID != rollback.ID {
			continue
		}
		resumed = true
		if !row.Rollback() {
			t.Fatal("the sweep rebuilt the rollback without its source release, " +
				"so a restart would drive it as a build deploy")
		}
	}
	if !resumed {
		t.Fatal("the sweep did not list the in flight rollback at all")
	}

	w.reconciler(fakeRegistry{err: errNoRegistry}).Sweep(ctx)

	dep, err := w.store.GetDeployment(ctx, rollback.ID)
	if err != nil {
		t.Fatalf("reading the rollback: %v", err)
	}
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("the resumed rollback ended %s with reason %v, want healthy", dep.State, dep.FailureReason)
	}
}
