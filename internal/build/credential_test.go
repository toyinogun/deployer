package build_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/build"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cred is the registry login both the build and the app namespace use.
func cred() build.Credential {
	return build.Credential{
		Host:     "registry.deployer-system.svc:5000",
		User:     "deployer",
		Password: "s3cret-htpasswd-value",
	}
}

// authsOf decodes a dockerconfigjson payload into its auths map.
func authsOf(t *testing.T, encoded []byte) map[string]map[string]string {
	t.Helper()
	var parsed struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("the credential is not valid JSON: %v", err)
	}
	return parsed.Auths
}

func TestDockerConfigJSONIsWhatAKubeletAndTheLifecycleBothRead(t *testing.T) {
	// covers: AC-9, AC-12
	t.Parallel()
	c := cred()

	encoded, err := build.DockerConfigJSON(c)
	if err != nil {
		t.Fatalf("DockerConfigJSON: %v", err)
	}

	entry, ok := authsOf(t, encoded)[c.Host]
	if !ok {
		t.Fatalf("no entry for %q in the auths map", c.Host)
	}
	if entry["username"] != c.User || entry["password"] != c.Password {
		t.Errorf("entry = %v, want the user and password beside the auth field", entry)
	}
	want := base64.StdEncoding.EncodeToString([]byte(c.User + ":" + c.Password))
	if entry["auth"] != want {
		t.Errorf("auth = %q, want base64 of user:password", entry["auth"])
	}
}

func TestDockerConfigJSONKeysTheCredentialByItsHostOnly(t *testing.T) {
	// covers: AC-12
	t.Parallel()
	encoded, err := build.DockerConfigJSON(cred())
	if err != nil {
		t.Fatalf("DockerConfigJSON: %v", err)
	}

	auths := authsOf(t, encoded)
	if len(auths) != 1 {
		t.Errorf("the credential covers %d hosts, want only the platform registry", len(auths))
	}
}

func TestPullSecretIsShapedForTheKubeletAndNothingElse(t *testing.T) {
	// covers: AC-12
	t.Parallel()
	secret, err := build.PullSecret("regcred", "app-hello-4dfssb", cred())
	if err != nil {
		t.Fatalf("PullSecret: %v", err)
	}

	if secret.Name != "regcred" || secret.Namespace != "app-hello-4dfssb" {
		t.Errorf("secret = %s/%s, want app-hello-4dfssb/regcred", secret.Namespace, secret.Name)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("type = %q, want %q", secret.Type, corev1.SecretTypeDockerConfigJson)
	}
	if _, ok := secret.Data[corev1.DockerConfigJsonKey]; !ok {
		t.Errorf("data keys = %v, want the dockerconfigjson key", keysOf(secret.Data))
	}
	if len(secret.Data) != 1 {
		t.Errorf("data keys = %v, want only the dockerconfigjson key", keysOf(secret.Data))
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "deployer" {
		t.Errorf("labels = %v, want the platform's managed-by label", secret.Labels)
	}
}

func TestPullSecretCarriesNoOwnerSoItOutlivesEachDeploy(t *testing.T) {
	// covers: AC-12
	t.Parallel()
	secret, err := build.PullSecret("regcred", "app-hello-4dfssb", cred())
	if err != nil {
		t.Fatalf("PullSecret: %v", err)
	}

	if len(secret.OwnerReferences) != 0 {
		t.Errorf("owner references = %v, want none: the app's pull secret is not a build's child",
			secret.OwnerReferences)
	}
}

func TestBuildCredentialSecretDiesWithTheJobItWasFor(t *testing.T) {
	// covers: AC-12, AC-22
	t.Parallel()
	owner := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       "build-dep-01kztj5xtwd8zajjdm728q6pxd",
		UID:        "0d7b3a9e-1f2c-4d5e-8a6b-9c0d1e2f3a4b",
	}

	secret, err := build.BuildCredentialSecret("dep_01KZTJ5XTWD8ZAJJDM728Q6PXD", "deployer-builds", cred(), owner)
	if err != nil {
		t.Fatalf("BuildCredentialSecret: %v", err)
	}

	if len(secret.OwnerReferences) != 1 {
		t.Fatalf("owner references = %v, want exactly the Job", secret.OwnerReferences)
	}
	if secret.OwnerReferences[0] != owner {
		t.Errorf("owner = %+v, want %+v", secret.OwnerReferences[0], owner)
	}
	if secret.Namespace != "deployer-builds" {
		t.Errorf("namespace = %q, want deployer-builds", secret.Namespace)
	}
	if secret.Name == "" {
		t.Error("the build credential has no name")
	}
}

func TestBuildCredentialSecretIsNamedForItsOwnDeployment(t *testing.T) {
	// covers: AC-12
	t.Parallel()
	owner := metav1.OwnerReference{Kind: "Job", Name: "build-dep-1"}

	first, err := build.BuildCredentialSecret("dep_01AAAA", "deployer-builds", cred(), owner)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := build.BuildCredentialSecret("dep_01BBBB", "deployer-builds", cred(), owner)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.Name == second.Name {
		t.Errorf("both builds got secret %q, so one build reads another's credential", first.Name)
	}
}

func TestTargetImageIsBuiltFromTheSlugThePlatformDerived(t *testing.T) {
	// covers: AC-9
	t.Parallel()
	host := "registry.deployer-system.svc:5000"

	got := build.TargetImage(host, "hello-4dfssb", "dep_01KZTJ5XTWD8ZAJJDM728Q6PXD")

	want := host + "/apps/hello-4dfssb:dep_01KZTJ5XTWD8ZAJJDM728Q6PXD"
	if got != want {
		t.Errorf("TargetImage = %q, want %q", got, want)
	}
}

func TestTargetImageCarriesNoPartOfTheNameTheCallerSent(t *testing.T) {
	// covers: AC-9
	t.Parallel()
	// The caller asked for this name; only the derived slug may reach the image
	// reference, so a hostile name cannot steer a push.
	callerName := "../../evil Repo"

	got := build.TargetImage("registry.example:5000", "evil-repo-b2c3d4", "dep_1")

	if strings.Contains(got, callerName) || strings.Contains(got, "..") {
		t.Errorf("TargetImage = %q, want nothing from the caller's raw name", got)
	}
}

func TestImageRepoIsTheHalfADigestResolvesAgainst(t *testing.T) {
	// covers: AC-9
	t.Parallel()
	host := "registry.deployer-system.svc:5000"

	repo := build.ImageRepo(host, "hello-4dfssb")

	if want := host + "/apps/hello-4dfssb"; repo != want {
		t.Errorf("ImageRepo = %q, want %q", repo, want)
	}
	if strings.Contains(repo, ":") != strings.Contains(host, ":") {
		t.Errorf("ImageRepo = %q, want no tag on the repository half", repo)
	}
	if prefix := build.TargetImage(host, "hello-4dfssb", "dep_1"); !strings.HasPrefix(prefix, repo+":") {
		t.Errorf("TargetImage %q is not the repo %q plus a tag", prefix, repo)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
