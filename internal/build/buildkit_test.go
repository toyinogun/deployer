// Tests for the Dockerfile build path's Job shape. Every field here was proved
// against the pinned rootless BuildKit image on the real cluster (spec 0009
// build step 3), so a change that looks harmless is a change to something that
// was learned the hard way.
package build_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/toyinogun/deployer/internal/build"
)

// dockerfileInput is the same build, selected onto the other engine.
func dockerfileInput() build.Input {
	in := input()
	in.Path = build.PathDockerfile
	return in
}

// The zero value has to read as the Buildpacks path, because that is the answer
// every archive gets unless detection says otherwise.
func TestPathZeroValueIsBuildpacks(t *testing.T) {
	t.Parallel()

	var p build.Path
	if p.String() != "buildpacks" {
		t.Errorf("zero Path = %q, want buildpacks", p.String())
	}
	if build.PathDockerfile.String() != "dockerfile" {
		t.Errorf("PathDockerfile = %q", build.PathDockerfile.String())
	}
}

// covers AC-7: one container running buildctl against the dockerfile frontend,
// pushing straight to the plain HTTP registry, with no cache, no build
// arguments, no secrets and no entitlements.
func TestDockerfileJobRunsBuildctl(t *testing.T) {
	t.Parallel()
	in := dockerfileInput()
	spec := build.Job(in).Spec.Template.Spec

	if len(spec.Containers) != 1 {
		t.Fatalf("containers = %d, want the one buildkit container", len(spec.Containers))
	}
	c := spec.Containers[0]
	if c.Image != in.BuildkitImage {
		t.Errorf("image = %q, want the pinned BuildKit image", c.Image)
	}
	if !strings.Contains(c.Image, "@sha256:") {
		t.Error("the BuildKit image is not pinned by digest")
	}
	if len(c.Command) != 1 || c.Command[0] != "buildctl-daemonless.sh" {
		t.Errorf("command = %v, want buildctl-daemonless.sh", c.Command)
	}

	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"build",
		"--frontend dockerfile.v0",
		"--local context=/workspace/source",
		"--local dockerfile=/workspace/source",
		"type=image,name=" + in.TargetImage,
		"push=true",
		"registry.insecure=true",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args = %q, missing %q", args, want)
		}
	}
	// Nothing a Dockerfile build is not granted: no cache to poison, no build
	// argument or secret surface, no entitlement.
	for _, forbidden := range []string{"--export-cache", "--import-cache", "--opt build-arg", "--secret", "--allow"} {
		if strings.Contains(args, forbidden) {
			t.Errorf("args = %q, must not carry %q", args, forbidden)
		}
	}
	if env(c)["DOCKER_CONFIG"] == "" {
		t.Error("buildctl is not pointed at its mounted push credential")
	}
}

// covers AC-7b: buildkitd's writable state is configuration the composer sets,
// because the state volume mounted over the image's tree hides the directories
// its own defaults point at. The state directory under TMPDIR is the one that
// bit: RootlessKit stats it, and the image puts TMPDIR inside the home it is
// about to be mounted over.
func TestDockerfileJobSuppliesBuildkitdState(t *testing.T) {
	t.Parallel()
	spec := build.Job(dockerfileInput()).Spec.Template.Spec
	c := spec.Containers[0]
	e := env(c)

	var stateMount string
	for _, m := range c.VolumeMounts {
		if m.Name == "buildkit-state" {
			stateMount = m.MountPath
		}
	}
	if stateMount == "" {
		t.Fatal("no buildkit-state volume is mounted, so buildkitd has no writable root")
	}
	var scratch bool
	for _, v := range spec.Volumes {
		if v.Name == "buildkit-state" {
			scratch = v.EmptyDir != nil
		}
	}
	if !scratch {
		t.Error("buildkit-state must be an emptyDir: nothing a build writes outlives it")
	}

	if e["HOME"] != stateMount {
		t.Errorf("HOME = %q, want the mounted state volume %q", e["HOME"], stateMount)
	}
	if !strings.HasPrefix(e["XDG_RUNTIME_DIR"], stateMount+"/") {
		t.Errorf("XDG_RUNTIME_DIR = %q, want a path inside %q", e["XDG_RUNTIME_DIR"], stateMount)
	}
	// TMPDIR has to survive the mount, so it cannot be under it.
	if e["TMPDIR"] == "" || strings.HasPrefix(e["TMPDIR"], stateMount) {
		t.Errorf("TMPDIR = %q, want a path that still exists once %q is mounted", e["TMPDIR"], stateMount)
	}
	if !strings.Contains(e["BUILDKITD_FLAGS"], "--oci-worker-snapshotter=native") {
		t.Errorf("BUILDKITD_FLAGS = %q, want the native snapshotter", e["BUILDKITD_FLAGS"])
	}
	// buildctl-daemonless.sh calls mktemp in /tmp directly, so this one container
	// cannot have a read only root.
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || *c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the buildkit container's root filesystem must be writable")
	}
}

// covers AC-8: the BuildKit container runs as the pair its own pinned image
// declares, never the Paketo pair and never a constant. A pair that is
// deliberately neither the default nor the Paketo one fails a hardcoded value.
func TestDockerfileJobRunsAsTheBuildkitPair(t *testing.T) {
	t.Parallel()
	in := dockerfileInput()
	in.BuildUID, in.BuildGID = 1001, 1000
	in.BuildkitUID, in.BuildkitGID = 3003, 3000
	spec := build.Job(in).Spec.Template.Spec

	pod := spec.SecurityContext
	if pod.RunAsUser == nil || *pod.RunAsUser != in.BuildkitUID {
		t.Errorf("pod RunAsUser = %v, want the BuildKit image's own uid %d", pod.RunAsUser, in.BuildkitUID)
	}
	if pod.RunAsGroup == nil || *pod.RunAsGroup != in.BuildkitGID {
		t.Errorf("pod RunAsGroup = %v, want %d", pod.RunAsGroup, in.BuildkitGID)
	}
	// The fetcher unpacks the tree this engine builds, so it runs as the same
	// pair rather than the Paketo one.
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != in.BuildkitUID {
			t.Errorf("%s RunAsUser = %v, want %d", c.Name, c.SecurityContext.RunAsUser, in.BuildkitUID)
		}
	}
}

// covers AC-10, AC-10a and AC-11: the Buildpacks pod still composes every
// `restricted` field now that the namespace no longer enforces them, and the
// BuildKit pod deviates in exactly four named fields with a bounding set of
// exactly SETUID and SETGID. A fifth deviation is a spec change, so it fails
// here first.
func TestDockerfilePodDeviatesInExactlyFourFields(t *testing.T) {
	t.Parallel()

	// The engine container on each path, which is the only place they differ.
	packs := build.Job(input()).Spec.Template.Spec.Containers[0]
	kit := build.Job(dockerfileInput()).Spec.Template.Spec.Containers[0]

	// The Buildpacks path is unchanged and still meets `restricted`.
	if packs.SecurityContext.AllowPrivilegeEscalation == nil || *packs.SecurityContext.AllowPrivilegeEscalation {
		t.Error("the Buildpacks container must still forbid privilege escalation")
	}
	if caps := packs.SecurityContext.Capabilities; caps == nil || len(caps.Add) != 0 {
		t.Errorf("the Buildpacks container adds capabilities %v, want none", caps)
	}
	if packs.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("the Buildpacks container must keep the RuntimeDefault seccomp profile")
	}
	if packs.SecurityContext.AppArmorProfile != nil {
		t.Error("the Buildpacks container must keep the default AppArmor profile")
	}

	sc := kit.SecurityContext
	// Deviation one and two: both profiles unconfined.
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Errorf("buildkit seccomp = %v, want Unconfined", sc.SeccompProfile)
	}
	if sc.AppArmorProfile == nil || sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined {
		t.Errorf("buildkit AppArmor = %v, want Unconfined", sc.AppArmorProfile)
	}
	// Deviation three: escalation allowed, so the setuid newuidmap can raise
	// into the bounding set. On its own this buys nothing, which is why the set
	// below is the field that matters.
	if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
		t.Error("buildkit needs allowPrivilegeEscalation true for newuidmap to run")
	}
	// Deviation four, and the real ceiling: drop everything, add back exactly
	// the pair RootlessKit's helpers need.
	caps := sc.Capabilities
	if caps == nil || len(caps.Drop) != 1 || caps.Drop[0] != "ALL" {
		t.Fatalf("buildkit capabilities = %v, want ALL dropped first", caps)
	}
	want := []corev1.Capability{"SETUID", "SETGID"}
	if len(caps.Add) != len(want) {
		t.Fatalf("buildkit added capabilities = %v, want exactly %v", caps.Add, want)
	}
	for i, c := range want {
		if caps.Add[i] != c {
			t.Errorf("added capability %d = %q, want %q", i, caps.Add[i], c)
		}
	}

	// Nothing else differs. Non root, a named uid, and no privileged mode.
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("buildkit must still run as non root")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("buildkit must not be privileged: that is the whole point of rootless")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Error("buildkit must name a non zero uid")
	}
}

// covers AC-18: the Dockerfile path holds no more cluster rights than the other
// one. Same absent token, same single use credential, no layers or platform
// volume it has no use for.
func TestDockerfileJobHoldsNoExtraRights(t *testing.T) {
	t.Parallel()
	in := dockerfileInput()
	spec := build.Job(in).Spec.Template.Spec

	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("a build pod must not be given a service account token")
	}
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		t.Error("a build pod must share no host namespace")
	}

	mounts := map[string]bool{}
	for _, m := range spec.Containers[0].VolumeMounts {
		mounts[m.Name] = true
		if m.Name == "docker-config" && !m.ReadOnly {
			t.Error("the push credential is mounted writable")
		}
	}
	if !mounts["docker-config"] {
		t.Error("buildkit has no push credential mounted")
	}
	// The lifecycle's volumes belong to the other engine.
	for _, unused := range []string{"layers", "platform"} {
		if mounts[unused] {
			t.Errorf("buildkit mounts %q, which only the lifecycle uses", unused)
		}
	}
	// The fetcher is the platform's own code on both paths.
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Image != in.SelfImage {
		t.Error("the Dockerfile path must fetch with the platform's own image")
	}
}

// covers AC-15: every build container on both paths is bounded on disk, so a
// build copying a large base image cannot fill a node.
func TestBothPathsBoundEphemeralStorage(t *testing.T) {
	t.Parallel()

	for _, in := range []build.Input{input(), dockerfileInput()} {
		spec := build.Job(in).Spec.Template.Spec
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			if c.Resources.Requests.StorageEphemeral().IsZero() {
				t.Errorf("%s path, %s: no ephemeral storage request", in.Path.String(), c.Name)
			}
			if c.Resources.Limits.StorageEphemeral().IsZero() {
				t.Errorf("%s path, %s: no ephemeral storage limit", in.Path.String(), c.Name)
			}
		}
	}
}
