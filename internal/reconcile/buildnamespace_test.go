package reconcile_test

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

// The namespace a Dockerfile build runs in. It is a second namespace because the
// two enforce different pod security levels, and every cluster call about a Job
// has to agree with the one that created it (spec 0009, AC-19).
const dockerfileNamespace = "deployer-builds-dockerfile"

// objectsIn is every build Job and every credential Secret that exists in one
// namespace, which is what "the Job was addressed here" actually means.
func (w *world) objectsIn(t *testing.T, namespace string) (jobs, secrets []string) {
	t.Helper()

	jobList, err := w.clientset.BatchV1().Jobs(namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing jobs in %s: %v", namespace, err)
	}
	for _, j := range jobList.Items {
		jobs = append(jobs, j.Name)
	}
	secretList, err := w.clientset.CoreV1().Secrets(namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing secrets in %s: %v", namespace, err)
	}
	for _, s := range secretList.Items {
		secrets = append(secrets, s.Name)
	}
	return jobs, secrets
}

// covers AC-19: a Dockerfile build's Job and its single use credential are both
// created in the BuildKit namespace, and nothing of it lands in the Paketo one,
// which is back at enforced `restricted` and would refuse the pod outright.
func TestADockerfileBuildIsCreatedInTheBuildkitNamespace(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	dep := w.queueWith(t, "Dockerfile", "main.go")

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), dep)

	jobs, secrets := w.objectsIn(t, dockerfileNamespace)
	if len(jobs) != 1 || len(secrets) != 1 {
		t.Errorf("%s holds %d jobs and %d secrets, want one of each", dockerfileNamespace, len(jobs), len(secrets))
	}
	strayJobs, straySecrets := w.objectsIn(t, "deployer-builds")
	if len(strayJobs) != 0 || len(straySecrets) != 0 {
		t.Errorf("deployer-builds holds jobs %v and secrets %v, want nothing: it enforces restricted and refuses this pod",
			strayJobs, straySecrets)
	}
}

// covers AC-19: the other path is unchanged. A Buildpacks build stays entirely in
// the namespace it has always run in.
func TestABuildpacksBuildStaysInTheBuildNamespace(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	dep := w.queueWith(t, "main.go", "go.mod")

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(t.Context(), dep)

	jobs, secrets := w.objectsIn(t, "deployer-builds")
	if len(jobs) != 1 || len(secrets) != 1 {
		t.Errorf("deployer-builds holds %d jobs and %d secrets, want one of each", len(jobs), len(secrets))
	}
	strayJobs, straySecrets := w.objectsIn(t, dockerfileNamespace)
	if len(strayJobs) != 0 || len(straySecrets) != 0 {
		t.Errorf("%s holds jobs %v and secrets %v, want nothing", dockerfileNamespace, strayJobs, straySecrets)
	}
}

// covers AC-19a: the path reaches the loop on the deployment itself, not as a
// local inside startBuild. This is the resumed case, which is the one that has no
// path at all without the column being mapped: the row is rebuilt from the store
// the way Sweep rebuilds it, so the watchdog deleting the Job has nothing else to
// go on. A Job deleted from the wrong namespace is a Job that keeps running.
func TestAResumedDockerfileBuildDeletesItsJobInTheRightNamespace(t *testing.T) {
	ctx := t.Context()
	w := setup(t)

	// A Dockerfile build already in flight, exactly as a restart would find it:
	// a building row that records its path and its Job, and a Job in the BuildKit
	// namespace.
	jobName := build.JobName(w.deployment.ID)
	if _, err := w.clientset.BatchV1().Jobs(dockerfileNamespace).Create(ctx,
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: dockerfileNamespace}},
		metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the build job: %v", err)
	}
	if _, err := w.store.Transition(ctx, w.deployment.ID, domain.StateBuilding, "", ""); err != nil {
		t.Fatalf("moving the deployment to building: %v", err)
	}
	if err := w.store.RecordBuildResult(ctx, w.deployment.ID, store.BuildResult{
		BuildPath: string(build.PathDockerfile), BuildJobName: jobName,
	}); err != nil {
		t.Fatalf("recording the build: %v", err)
	}

	// Read back through the store adapter rather than built by hand, because what
	// is being tested is whether the column survives that trip.
	rows, err := store.ForReconcile(w.store).ListNonTerminal(ctx)
	if err != nil {
		t.Fatalf("listing what a restart would resume: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("in flight rows = %d, want the one", len(rows))
	}
	if rows[0].BuildPath != string(build.PathDockerfile) {
		t.Fatalf("the resumed row carries build path %q, want dockerfile: the column did not reach the loop",
			rows[0].BuildPath)
	}

	// Past its budget, so the watchdog gives up on it and deletes the build.
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, spent).Drive(ctx, rows[0])

	w.failedWith(t, domain.ReasonTimeout)
	left, err := w.clientset.BatchV1().Jobs(dockerfileNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing jobs in %s: %v", dockerfileNamespace, err)
	}
	if len(left.Items) != 0 {
		t.Errorf("the build job is still running in %s: the delete went to the wrong namespace", dockerfileNamespace)
	}
}
