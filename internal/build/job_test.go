// Package build_test asserts what the composed build Job actually says. These
// are the fields that keep untrusted build code inside a box, so they are
// checked one by one rather than by eye.
package build_test

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/toyinogun/deployer/internal/build"
)

const (
	deploymentID = "dep_01JABCDEF0123456789ABCDEF"
	registryHost = "deployer-registry.deployer-system.svc:5000"
)

func input() build.Input {
	return build.Input{
		DeploymentID:    deploymentID,
		Namespace:       "deployer-builds",
		AppSlug:         "my-app-a1b2c3",
		SelfImage:       "ghcr.io/toyinogun/deployer@sha256:" + strings.Repeat("b", 64),
		BuilderImage:    "paketobuildpacks/builder-jammy-base@sha256:" + strings.Repeat("a", 64),
		TargetImage:     build.TargetImage(registryHost, "my-app-a1b2c3", deploymentID),
		FetchURL:        "https://deployer.example.ts.net/v1/uploads/upl_123",
		FetchToken:      "a-single-use-token",
		ExpectedSHA:     strings.Repeat("f", 64),
		MaxFiles:        20000,
		MaxExtracted:    512 << 20,
		CredentialRef:   build.SecretName(deploymentID),
		DeadlineSeconds: 480,
	}
}

// env returns a container's environment as a map.
func env(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

// The fake clientset does not validate object names, so nothing else here would
// notice a name the real API server refuses. A deployment id carries both an
// underscore and uppercase letters, and neither is legal in an object name, so
// the names derived from it are checked against the same rule the API server
// applies. Covers AC-9 and AC-18: without a Job, a build never starts and the
// startup sweep has nothing to find.
func TestDerivedNamesAreValidObjectNames(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		"JobName":    build.JobName(deploymentID),
		"SecretName": build.SecretName(deploymentID),
	}
	for what, name := range names {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Errorf("%s(%q) = %q, which the API server refuses: %s", what, deploymentID, name, strings.Join(errs, "; "))
		}
	}
}

// covers AC-7: one Job is one attempt, with its own deadline and its own reaping.
func TestJobIsOneAttemptWithADeadline(t *testing.T) {
	t.Parallel()
	job := build.Job(input())

	if job.Name != build.JobName(deploymentID) {
		t.Errorf("name = %q, want it derived from the deployment id", job.Name)
	}
	if job.Namespace != "deployer-builds" {
		t.Errorf("namespace = %q", job.Namespace)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("backoffLimit must be 0, so one Job is exactly one attempt")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 480 {
		t.Error("activeDeadlineSeconds must carry the configured build timeout")
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished <= 0 {
		t.Error("ttlSecondsAfterFinished must be set, so finished Jobs are reaped")
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("a build pod must never restart in place")
	}
}

// covers AC-7: the pod satisfies `restricted` pod security, container by
// container. This is the box around code the platform did not author.
func TestJobPodIsRestricted(t *testing.T) {
	t.Parallel()
	spec := build.Job(input()).Spec.Template.Spec

	pod := spec.SecurityContext
	if pod == nil || pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot {
		t.Error("the pod must run as non root")
	}
	if pod.RunAsUser == nil || *pod.RunAsUser == 0 {
		t.Error("the pod must name a non zero user")
	}
	if pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("the pod must carry the RuntimeDefault seccomp profile")
	}
	// A build holds no cluster rights at all, so nothing it runs can talk to the
	// API server even if it wanted to.
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("a build pod must not be given a service account token")
	}

	all := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	if len(all) != 2 {
		t.Fatalf("containers = %d, want one init container and one builder", len(all))
	}
	for _, c := range all {
		sc := c.SecurityContext
		switch {
		case sc == nil:
			t.Errorf("%s has no security context", c.Name)
		case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
			t.Errorf("%s allows privilege escalation", c.Name)
		case sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot:
			t.Errorf("%s does not run as non root", c.Name)
		case sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL":
			t.Errorf("%s does not drop all capabilities", c.Name)
		case sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
			t.Errorf("%s has no RuntimeDefault seccomp profile", c.Name)
		}
		if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
			t.Errorf("%s has no resource ceiling", c.Name)
		}
	}
}

// covers AC-8: the init container is the platform's own image running its own
// extractor, told the platform's recorded hash and its caps.
func TestJobInitContainerFetchesWithThePlatformsOwnCode(t *testing.T) {
	t.Parallel()
	in := input()
	init := build.Job(in).Spec.Template.Spec.InitContainers[0]

	if init.Image != in.SelfImage {
		t.Errorf("init image = %q, want the control plane's own image", init.Image)
	}
	if len(init.Args) != 1 || init.Args[0] != "fetch-source" {
		t.Errorf("init args = %v, want the fetch-source subcommand", init.Args)
	}
	e := env(init)
	if e["DEPLOYER_EXPECTED_SHA256"] != in.ExpectedSHA {
		t.Error("the init container is not told the platform's recorded hash")
	}
	if e["DEPLOYER_FETCH_TOKEN"] != in.FetchToken {
		t.Error("the init container is not given its single use token")
	}
	if e["DEPLOYER_MAX_UPLOAD_FILES"] == "" || e["DEPLOYER_MAX_EXTRACTED_BYTES"] == "" {
		t.Error("the init container is not given the extraction caps")
	}
	// It holds the source, never the registry credential.
	for _, m := range init.VolumeMounts {
		if strings.Contains(m.Name, "docker") {
			t.Errorf("the init container mounts %q, which is the push credential", m.Name)
		}
	}
}

// covers AC-9: the builder runs the pinned builder's lifecycle, pushing to the
// tag the platform chose, with the credential mounted rather than baked in.
func TestJobBuilderRunsTheLifecycle(t *testing.T) {
	t.Parallel()
	in := input()
	builder := build.Job(in).Spec.Template.Spec.Containers[0]

	if builder.Image != in.BuilderImage {
		t.Errorf("builder image = %q, want the pinned builder", builder.Image)
	}
	if !strings.Contains(builder.Image, "@sha256:") {
		t.Error("the builder image is not pinned by digest")
	}
	if len(builder.Command) != 1 || !strings.HasSuffix(builder.Command[0], "/creator") {
		t.Errorf("command = %v, want the lifecycle creator", builder.Command)
	}
	args := strings.Join(builder.Args, " ")
	if !strings.Contains(args, "-daemon=false") {
		t.Error("the lifecycle must push straight to the registry, with no daemon")
	}
	if builder.Args[len(builder.Args)-1] != in.TargetImage {
		t.Errorf("the last argument is %q, want the target image", builder.Args[len(builder.Args)-1])
	}
	if env(builder)["DOCKER_CONFIG"] == "" {
		t.Error("the builder is not pointed at its mounted credential")
	}

	var mounted bool
	for _, m := range builder.VolumeMounts {
		if m.Name == "docker-config" {
			mounted = true
			if !m.ReadOnly {
				t.Error("the credential is mounted writable")
			}
		}
	}
	if !mounted {
		t.Error("the builder has no credential mounted")
	}
}

// The target image carries the deployment id as its tag, and the app's slug as
// its repository, both platform derived. Nothing a caller sent is in it.
func TestTargetImageIsPlatformDerived(t *testing.T) {
	t.Parallel()

	got := build.TargetImage(registryHost, "my-app-a1b2c3", deploymentID)

	want := registryHost + "/apps/my-app-a1b2c3:" + deploymentID
	if got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
	if repo := build.ImageRepo(registryHost, "my-app-a1b2c3"); repo != registryHost+"/apps/my-app-a1b2c3" {
		t.Errorf("repo = %q", repo)
	}
}

// covers AC-12: the credential encodes as a docker config both a kubelet and the
// lifecycle read, and the secret is the type Kubernetes expects.
func TestPullSecretIsADockerConfig(t *testing.T) {
	t.Parallel()
	cred := build.Credential{Host: registryHost, User: "deployer", Password: "s3cret"}

	secret, err := build.PullSecret("registry", "app-my-app-a1b2c3", cred)
	if err != nil {
		t.Fatalf("composing the secret: %v", err)
	}

	if secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("type = %q, want kubernetes.io/dockerconfigjson", secret.Type)
	}
	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &config); err != nil {
		t.Fatalf("decoding the config: %v", err)
	}
	entry, ok := config.Auths[registryHost]
	if !ok {
		t.Fatalf("no entry for %q in %v", registryHost, config.Auths)
	}
	if entry.Username != "deployer" || entry.Password != "s3cret" || entry.Auth == "" {
		t.Errorf("entry = %+v, want the credential and its encoded form", entry)
	}
}

// The per Job credential is owned by its Job, so Kubernetes collects it when the
// Job is reaped and a push credential never outlives the build it was for.
func TestBuildCredentialSecretIsOwnedByItsJob(t *testing.T) {
	t.Parallel()
	owner := metav1.OwnerReference{
		APIVersion: "batch/v1", Kind: "Job",
		Name: build.JobName(deploymentID), UID: "abc-123",
	}

	secret, err := build.BuildCredentialSecret(deploymentID, "deployer-builds",
		build.Credential{Host: registryHost, User: "u", Password: "p"}, owner)
	if err != nil {
		t.Fatalf("composing: %v", err)
	}

	if secret.Name != build.SecretName(deploymentID) {
		t.Errorf("name = %q", secret.Name)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != build.JobName(deploymentID) {
		t.Errorf("owner references = %v, want the Job", secret.OwnerReferences)
	}
}
