package reconcile_test

import (
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/store"
)

func TestADeployGivesTheContainerItsConfigurationAndTheTwoPlatformValues(t *testing.T) {
	// covers: spec 0010 AC-7, AC-10, AC-15
	w := setup(t)
	ctx := t.Context()
	if err := w.store.SetConfigBatch(ctx, w.app.ID, []store.ConfigEntry{
		{Key: "DATABASE_URL", Value: "postgres://db/app", IsSecret: true},
		{Key: "LOG_LEVEL", Value: "debug"},
		{Key: "EMPTY", Value: ""},
	}); err != nil {
		t.Fatalf("setting the configuration: %v", err)
	}
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	dep, err := w.store.GetDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateHealthy) {
		t.Fatalf("state = %s, want healthy (failure reason %v)", dep.State, dep.FailureReason)
	}

	namespace := deploy.NamespaceName(w.app.Slug)
	secret, err := w.clientset.CoreV1().Secrets(namespace).Get(ctx, deploy.ConfigSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the configuration secret: %v", err)
	}
	if got := string(secret.Data["DATABASE_URL"]); got != "postgres://db/app" {
		t.Errorf("DATABASE_URL in the cluster = %q", got)
	}
	if got := string(secret.Data["LOG_LEVEL"]); got != "debug" {
		t.Errorf("LOG_LEVEL in the cluster = %q", got)
	}
	if _, ok := secret.Data["EMPTY"]; !ok {
		t.Error("the empty value is missing, so an empty string read as the key being unset")
	}

	created, err := w.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(created.Items) != 1 {
		t.Fatalf("reading the composed deployment: %v (%d items)", err, len(created.Items))
	}
	pod := created.Items[0].Spec.Template
	c := pod.Spec.Containers[0]
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil ||
		c.EnvFrom[0].SecretRef.Name != deploy.ConfigSecretName {
		t.Errorf("envFrom = %+v, want the configuration secret", c.EnvFrom)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["PORT"] != "8080" {
		t.Errorf("PORT = %q, want 8080", env["PORT"])
	}
	if want := "https://" + w.app.Slug + ".deploy.example.org"; env["APP_URL"] != want {
		t.Errorf("APP_URL = %q, want %q", env["APP_URL"], want)
	}
	if pod.Annotations[deploy.ConfigChecksumAnnotation] == "" {
		t.Error("the pod template carries no configuration checksum")
	}

	// The release records what this deploy actually ran with (AC-10).
	rel, err := w.store.GetReleaseByDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the release: %v", err)
	}
	var snapshot map[string]string
	if err := json.Unmarshal([]byte(rel.ConfigSnapshot), &snapshot); err != nil {
		t.Fatalf("reading the release's configuration snapshot %q: %v", rel.ConfigSnapshot, err)
	}
	if snapshot["DATABASE_URL"] != "postgres://db/app" || snapshot["LOG_LEVEL"] != "debug" {
		t.Errorf("the release snapshot holds %+v", snapshot)
	}
}

func TestConfigurationSetAfterADeployReachesTheNextOneAndNotThisOne(t *testing.T) {
	// covers: spec 0010 AC-8, AC-10
	w := setup(t)
	ctx := t.Context()
	if err := w.store.SetConfigBatch(ctx, w.app.ID,
		[]store.ConfigEntry{{Key: "LOG_LEVEL", Value: "info"}}); err != nil {
		t.Fatalf("setting the configuration: %v", err)
	}
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	first, err := w.store.GetReleaseByDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the first release: %v", err)
	}

	// The change lands in the store and starts nothing: the objects in the
	// cluster still hold the old value until another deploy composes them.
	if err := w.store.SetConfigBatch(ctx, w.app.ID,
		[]store.ConfigEntry{{Key: "LOG_LEVEL", Value: "debug"}}); err != nil {
		t.Fatalf("changing the configuration: %v", err)
	}
	namespace := deploy.NamespaceName(w.app.Slug)
	secret, err := w.clientset.CoreV1().Secrets(namespace).Get(ctx, deploy.ConfigSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the configuration secret: %v", err)
	}
	if got := string(secret.Data["LOG_LEVEL"]); got != "info" {
		t.Errorf("the running app's secret already says %q, so the change did not wait for a deploy", got)
	}
	if !strings.Contains(first.ConfigSnapshot, "info") {
		t.Errorf("the first release snapshotted %q, want the value it ran with", first.ConfigSnapshot)
	}
}
