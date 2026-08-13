// Package build composes the Kubernetes Job that turns a source tarball into an
// image. Everything here is field by field construction over the Kubernetes API
// types: no templating, no YAML strings, and no value a caller supplied reaching
// a pod spec. It imports the API types but never a client, so composing a build
// is testable without a cluster.
package build

import (
	"fmt"
	"path"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Where things live inside the build pod. All platform constants: nothing a
// caller sends decides any of them.
const (
	// workspaceDir holds the downloaded archive; sourceDir, inside it, is where
	// the archive is unpacked and what the lifecycle builds.
	workspaceDir = "/workspace"
	sourceDir    = workspaceDir + "/source"
	layersDir    = "/layers"
	platformDir  = "/platform"
	dockerDir    = "/docker-config"

	// lifecyclePath is where every Paketo builder puts the lifecycle binaries.
	lifecyclePath = "/cnb/lifecycle/creator"

	// The Dockerfile path's own layout. buildkitEntrypoint is on the rootless
	// image's PATH; the three directories describe where that image's daemon may
	// write once the state volume is mounted over its home. They are constants
	// rather than configuration because they are tied to how the pinned digest
	// lays out its own tree: a repin re runs spec 0009's build step 3.
	buildkitEntrypoint = "buildctl-daemonless.sh"
	buildkitStateDir   = "/home/user"
	buildkitRuntimeDir = buildkitStateDir + "/run"
	buildkitTmpDir     = "/tmp"

	// buildkitdFlags is how the one shot daemon is told to copy layers instead of
	// overlaying them, since this pod mounts no /dev/fuse, and to skip the
	// process sandbox it cannot set up without one.
	buildkitdFlags = "--oci-worker-no-process-sandbox --oci-worker-snapshotter=native"

	// ttlAfterFinished is how long a finished Job lingers before Kubernetes
	// reaps it, taking its per Job credential secret with it. Long enough to
	// read the pod's logs after a failure, short enough not to accumulate.
	ttlAfterFinished = int32(3600)
)

// Path is which engine a build runs. The zero value is the Buildpacks path,
// because that is the answer every archive gets unless detection says otherwise.
type Path string

// The two engines. These strings are also what deployments.build_path holds, so
// they are the column's check constraint spelled in Go.
const (
	PathBuildpacks Path = "buildpacks"
	PathDockerfile Path = "dockerfile"
)

// String is the value that goes into deployments.build_path. The zero value
// reads as buildpacks rather than as empty, so a row can never carry a path the
// column would refuse.
func (p Path) String() string {
	if p == PathDockerfile {
		return string(PathDockerfile)
	}
	return string(PathBuildpacks)
}

// Input is everything a build Job needs. Every field is platform derived: the
// slug came from the platform's own derivation, the digest from a build the
// platform ran, and the rest from configuration.
type Input struct {
	DeploymentID string // names the Job, so the row can find it again after a restart
	Namespace    string // DEPLOYER_BUILD_NAMESPACE
	AppSlug      string // for labels only, never for a path or a command

	// Path is the engine this build runs, derived by the platform from the
	// archive's own contents. No caller value selects it.
	Path Path

	SelfImage    string // the control plane's own image, run as the init container
	BuilderImage string // the digest pinned Paketo builder
	TargetImage  string // where the lifecycle pushes, registry host plus repo plus tag

	// BuildUID and BuildGID are the CNB_USER_ID and CNB_GROUP_ID that
	// BuilderImage's own config declares, read off that pinned image rather than
	// assumed here. The lifecycle switches to CNB_USER_ID before it does any
	// work, and under `restricted` it holds no capability to switch with, so the
	// pod has to start as that user already. They come in as configuration
	// (DEPLOYER_BUILD_UID, DEPLOYER_BUILD_GID) because the builder digest is
	// configuration, and the two have to be repinned together: CI reads both off
	// the pinned image and fails when they drift.
	BuildUID int64
	BuildGID int64

	// BuildkitImage is the digest pinned rootless BuildKit image, and
	// BuildkitUID/BuildkitGID are the pair it declares in its own OCI config.
	// Same rule as the Paketo trio and for the same reason: an image and the
	// identity it runs as are one unit, repinned and drift checked together.
	// They come from DEPLOYER_BUILDKIT_IMAGE, _UID and _GID.
	BuildkitImage string
	BuildkitUID   int64
	BuildkitGID   int64

	FetchURL      string // the control plane's single use fetch endpoint for this upload
	FetchToken    string // the raw single use token, never persisted or logged
	ExpectedSHA   string // the platform's recorded hash, checked before anything unpacks
	MaxFiles      int
	MaxExtracted  int64
	CredentialRef string // the per Job dockerconfigjson secret this pod reads

	DeadlineSeconds int64 // DEPLOYER_BUILD_TIMEOUT_SECONDS
}

// JobName is the Job a deployment's build runs as. Derived from the deployment
// id alone, so the startup sweep can find a live build for a row it read off
// disk without having recorded anything extra.
func JobName(deploymentID string) string { return "build-" + objectNameFor(deploymentID) }

// SecretName is the per Job registry credential for a deployment's build.
func SecretName(deploymentID string) string { return "build-" + objectNameFor(deploymentID) }

// objectNameFor turns a platform id into the RFC 1123 form a Kubernetes object
// name has to take. An id is a prefix, an underscore, and an uppercase Crockford
// base32 ULID, and the API server refuses both the underscore and the uppercase.
// Lowercasing is lossless here because a ULID never contains a lowercase letter,
// so two different ids can never collapse onto one name.
func objectNameFor(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "_", "-"))
}

// Job composes the build Job.
//
// One Job is one attempt: backoffLimit is zero, so a failure is a failure rather
// than a silent retry that muddles which attempt produced which image. The pod
// runs under `restricted` pod security because the namespace enforces it, and
// because this is the one place the platform runs code it did not author.
func Job(in Input) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "deployer",
		"deployer.io/deployment":       in.DeploymentID,
		"deployer.io/app":              in.AppSlug,
	}
	backoff := int32(0)
	ttl := ttlAfterFinished
	deadline := in.DeadlineSeconds

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(in.DeploymentID),
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// The builder holds no Kubernetes rights at all. It has the
					// one registry credential it needs to push and nothing else.
					AutomountServiceAccountToken: ptr(false),
					SecurityContext:              podSecurity(in),
					InitContainers:               []corev1.Container{fetchContainer(in)},
					Containers:                   []corev1.Container{engineContainer(in)},
					Volumes:                      volumes(in),
				},
			},
		},
	}
}

// engineContainer is the one container that turns the fetched tree into an
// image, whichever engine this build chose.
func engineContainer(in Input) corev1.Container {
	if in.Path == PathDockerfile {
		return buildkitContainer(in)
	}
	return builderContainer(in)
}

// identity is the uid and gid the pod runs as: the pair declared by whichever
// engine image this build uses. The fetcher runs as the same pair, so the tree
// one container unpacks is a tree the other can read.
func identity(in Input) (uid, gid int64) {
	if in.Path == PathDockerfile {
		return in.BuildkitUID, in.BuildkitGID
	}
	return in.BuildUID, in.BuildGID
}

// podSecurity is the pod level context `restricted` requires, plus the fsGroup
// that lets an unprivileged user write to the emptyDir volumes. The init
// container is pinned to the same pair as the builder, so the tree one writes is
// a tree the other can read.
//
// The Dockerfile path carries its seccomp deviation here as well as on its own
// container, because that is the shape that was proved against the pinned image
// on the cluster. The fetcher still names RuntimeDefault for itself, which
// overrides this, so the widening reaches only the container that needs it.
func podSecurity(in Input) *corev1.PodSecurityContext {
	uid, gid := identity(in)
	seccomp := corev1.SeccompProfileTypeRuntimeDefault
	if in.Path == PathDockerfile {
		seccomp = corev1.SeccompProfileTypeUnconfined
	}
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr(true),
		RunAsUser:      ptr(uid),
		RunAsGroup:     ptr(gid),
		FSGroup:        ptr(gid),
		SeccompProfile: &corev1.SeccompProfile{Type: seccomp},
	}
}

// containerSecurity is the container level context `restricted` requires.
// readOnlyRoot is separate because the lifecycle writes to its own filesystem
// and the fetcher does not.
func containerSecurity(in Input, readOnlyRoot bool) *corev1.SecurityContext {
	uid, _ := identity(in)
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(readOnlyRoot),
		RunAsNonRoot:             ptr(true),
		RunAsUser:                ptr(uid),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// fetchContainer runs the control plane's own fetch-source subcommand. It is the
// deployer image on purpose: the extractor that decides what may be unpacked is
// the platform's code, versioned with the platform, never something the builder
// supplies.
func fetchContainer(in Input) corev1.Container {
	return corev1.Container{
		Name:    "fetch-source",
		Image:   in.SelfImage,
		Command: []string{"/ko-app/deployer"},
		Args:    []string{"fetch-source"},
		Env: []corev1.EnvVar{
			{Name: "DEPLOYER_FETCH_URL", Value: in.FetchURL},
			// The one secret this container holds, and it unlocks exactly one
			// upload, once. It is never written to the database or a log.
			{Name: "DEPLOYER_FETCH_TOKEN", Value: in.FetchToken},
			// The platform's own record of the hash, so the archive is checked
			// against what was uploaded rather than against anything it carries.
			{Name: "DEPLOYER_EXPECTED_SHA256", Value: in.ExpectedSHA},
			{Name: "DEPLOYER_SOURCE_DIR", Value: sourceDir},
			{Name: "DEPLOYER_MAX_UPLOAD_FILES", Value: fmt.Sprint(in.MaxFiles)},
			{Name: "DEPLOYER_MAX_EXTRACTED_BYTES", Value: fmt.Sprint(in.MaxExtracted)},
		},
		SecurityContext: containerSecurity(in, true),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: workspaceDir},
		},
		Resources: buildResources(),
	}
}

// builderContainer runs the pinned Paketo builder's lifecycle in one process.
// `creator` detects, builds, exports and pushes without a daemon, which is why
// this pod needs no privileged mode and no socket.
func builderContainer(in Input) corev1.Container {
	return corev1.Container{
		Name:    "build",
		Image:   in.BuilderImage,
		Command: []string{lifecyclePath},
		Args: []string{
			"-app=" + sourceDir,
			"-layers=" + layersDir,
			"-platform=" + platformDir,
			"-report=" + path.Join(layersDir, "report.toml"),
			// No daemon: the lifecycle pushes straight to the registry.
			"-daemon=false",
			// The platform reads the digest back off the registry rather than out
			// of the pod, so nothing here has to report anything.
			in.TargetImage,
		},
		Env: []corev1.EnvVar{
			{Name: "DOCKER_CONFIG", Value: dockerDir},
			{Name: "CNB_PLATFORM_API", Value: "0.13"},
			// The registry is plain HTTP on the pod network, reachable only in
			// cluster and with no Ingress. See the Security model in spec 0004.
			{Name: "CNB_INSECURE_REGISTRIES", Value: registryHost(in.TargetImage)},
		},
		SecurityContext: containerSecurity(in, false),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: workspaceDir},
			{Name: "layers", MountPath: layersDir},
			{Name: "platform", MountPath: platformDir},
			{Name: "docker-config", MountPath: dockerDir, ReadOnly: true},
		},
		Resources: buildResources(),
	}
}

// buildkitContainer runs one throwaway rootless BuildKit for one Dockerfile.
// buildctl-daemonless.sh starts a buildkitd, runs the build, and exits, so there
// is no daemon, no socket and no shared state between two builds.
//
// Every value below was proved by running the pinned image on this cluster
// (spec 0009 build step 3) rather than read out of documentation, and the ones
// that look redundant are the ones that were not. Repinning the image is a re run
// of that probe, not a digest edit.
func buildkitContainer(in Input) corev1.Container {
	return corev1.Container{
		Name:    "build",
		Image:   in.BuildkitImage,
		Command: []string{buildkitEntrypoint},
		Args: []string{
			"build",
			"--frontend", "dockerfile.v0",
			// The context and the Dockerfile are the same tree: the root of what
			// the fetcher unpacked, which is the only path either engine sees.
			"--local", "context=" + sourceDir,
			"--local", "dockerfile=" + sourceDir,
			// Straight to the in cluster registry. buildctl has no environment
			// escape hatch for plain HTTP, so the allowance rides on the output.
			// No cache export, no build arguments, no secrets, no entitlements.
			"--output", "type=image,name=" + in.TargetImage + ",push=true,registry.insecure=true",
		},
		Env: []corev1.EnvVar{
			{Name: "DOCKER_CONFIG", Value: dockerDir},
			// No /dev/fuse in this pod, so layers are copied rather than
			// overlaid. Slower and heavier on disk, which AC-15's bounds price in.
			{Name: "BUILDKITD_FLAGS", Value: buildkitdFlags},
			// buildkitd's writable root, supplied rather than left to the image's
			// own defaults, which the state mount below hides.
			{Name: "HOME", Value: buildkitStateDir},
			{Name: "XDG_RUNTIME_DIR", Value: buildkitRuntimeDir},
			// Outside the mount on purpose: RootlessKit stats its state directory
			// under TMPDIR, and the image points TMPDIR inside the home directory
			// that is about to be mounted over.
			{Name: "TMPDIR", Value: buildkitTmpDir},
		},
		SecurityContext: buildkitSecurity(in),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: workspaceDir},
			{Name: "buildkit-state", MountPath: buildkitStateDir},
			{Name: "docker-config", MountPath: dockerDir, ReadOnly: true},
		},
		Resources: buildResources(),
	}
}

// buildkitSecurity is `restricted` with the four deviations spec 0009 names, and
// nothing else. A fifth field here is a spec change, not a commit.
//
// The bounding set is the real control. Allowing escalation on its own changed
// nothing on the cluster: the build only ran once SETUID and SETGID were in the
// set for the setuid newuidmap to raise into. So the pair below, not the
// escalation flag, is what bounds the one place the platform runs code it did
// not write.
func buildkitSecurity(in Input) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		// Deviation three. Without it the setuid helper cannot raise at all.
		AllowPrivilegeEscalation: ptr(true),
		// buildctl-daemonless.sh calls mktemp in /tmp directly.
		ReadOnlyRootFilesystem: ptr(false),
		RunAsNonRoot:           ptr(true),
		RunAsUser:              ptr(in.BuildkitUID),
		// Deviation four, and the ceiling everything else rests on.
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  buildkitCapabilities(),
		},
		// Deviations one and two: a rootless builder unshares namespaces and
		// mounts, which neither profile permits.
		SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
	}
}

// buildkitCapabilities is the exact pair RootlessKit's newuidmap and newgidmap
// need to map a uid range inside the pod. A fresh slice each call, so no caller
// can widen the set the platform composed.
func buildkitCapabilities() []corev1.Capability {
	return []corev1.Capability{"SETUID", "SETGID"}
}

// registryHost is the host and port out of an image reference, which is what
// the lifecycle wants when told a registry is served over plain HTTP.
func registryHost(image string) string {
	for i, r := range image {
		if r == '/' {
			return image[:i]
		}
	}
	return image
}

// volumes are all scratch. Nothing a build produces survives the Job except the
// image it pushed.
func volumes(in Input) []corev1.Volume {
	scratch := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if in.Path == PathDockerfile {
		// buildkitd's writable root. Nothing it holds outlives the build, which is
		// also why a Dockerfile build has no layer cache.
		scratch = append(scratch, corev1.Volume{
			Name: "buildkit-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	} else {
		scratch = append(scratch,
			corev1.Volume{Name: "layers", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			corev1.Volume{Name: "platform", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		)
	}
	return append(scratch, corev1.Volume{
		Name: "docker-config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: in.CredentialRef,
				Items: []corev1.KeyToPath{
					{Key: corev1.DockerConfigJsonKey, Path: "config.json"},
				},
			},
		},
	})
}

// buildResources bounds what one build may take. Deliberately constants rather
// than settings: spec 0004 adds no configuration for them, and a build that can
// take a whole node is a build that can stop the control plane sharing it.
// Generous, because a cold Buildpacks build compiles a whole application.
//
// The ephemeral storage pair is the same kind of decision. Both engines copy a
// lot of bytes onto the node's disk, and the Dockerfile path copies more because
// the native snapshotter overlays nothing, so a build without a ceiling here is
// a build that can fill the disk every other pod on that node shares.
func buildResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("250m"),
			corev1.ResourceMemory:           resource.MustParse("512Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("2"),
			corev1.ResourceMemory:           resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("8Gi"),
		},
	}
}

// ptr returns a pointer to v, for the many optional fields in a pod spec.
func ptr[T any](v T) *T { return &v }

// JobState is where a build Job has got to. It lives here rather than beside the
// Kubernetes client so the reconcile loop can act on a build outcome without
// importing client-go (spec 0001, package layout).
type JobState int

// The four answers the loop acts on. Gone is its own state rather than an error,
// because a vanished Job is a decision (fail the deployment) and not a fault.
const (
	JobRunning JobState = iota
	JobSucceeded
	JobFailed
	JobGone
)
