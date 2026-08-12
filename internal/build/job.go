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

	// buildUID and buildGID are the `cnb` user the Paketo builder images declare,
	// read from the pinned builder's own image config rather than assumed. The
	// lifecycle switches to CNB_USER_ID before it does any work, and under
	// `restricted` it holds no capability to switch with, so the pod has to start
	// as that user already. The init container is pinned to the same pair, so the
	// tree one writes is a tree the other can read.
	buildUID = int64(1001)
	buildGID = int64(1000)

	// ttlAfterFinished is how long a finished Job lingers before Kubernetes
	// reaps it, taking its per Job credential secret with it. Long enough to
	// read the pod's logs after a failure, short enough not to accumulate.
	ttlAfterFinished = int32(3600)
)

// Input is everything a build Job needs. Every field is platform derived: the
// slug came from the platform's own derivation, the digest from a build the
// platform ran, and the rest from configuration.
type Input struct {
	DeploymentID string // names the Job, so the row can find it again after a restart
	Namespace    string // DEPLOYER_BUILD_NAMESPACE
	AppSlug      string // for labels only, never for a path or a command

	SelfImage    string // the control plane's own image, run as the init container
	BuilderImage string // the digest pinned Paketo builder
	TargetImage  string // where the lifecycle pushes, registry host plus repo plus tag

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
					SecurityContext:              podSecurity(),
					InitContainers:               []corev1.Container{fetchContainer(in)},
					Containers:                   []corev1.Container{builderContainer(in)},
					Volumes:                      volumes(in),
				},
			},
		},
	}
}

// podSecurity is the pod level context `restricted` requires, plus the fsGroup
// that lets an unprivileged user write to the emptyDir volumes.
func podSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr(true),
		RunAsUser:      ptr(buildUID),
		RunAsGroup:     ptr(buildGID),
		FSGroup:        ptr(buildGID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// containerSecurity is the container level context `restricted` requires.
// readOnlyRoot is separate because the lifecycle writes to its own filesystem
// and the fetcher does not.
func containerSecurity(readOnlyRoot bool) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(readOnlyRoot),
		RunAsNonRoot:             ptr(true),
		RunAsUser:                ptr(buildUID),
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
		SecurityContext: containerSecurity(true),
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
		SecurityContext: containerSecurity(false),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: workspaceDir},
			{Name: "layers", MountPath: layersDir},
			{Name: "platform", MountPath: platformDir},
			{Name: "docker-config", MountPath: dockerDir, ReadOnly: true},
		},
		Resources: buildResources(),
	}
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
	return []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "layers", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "platform", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{
			Name: "docker-config",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: in.CredentialRef,
					Items: []corev1.KeyToPath{
						{Key: corev1.DockerConfigJsonKey, Path: "config.json"},
					},
				},
			},
		},
	}
}

// buildResources bounds what one build may take. Deliberately constants rather
// than settings: spec 0004 adds no configuration for them, and a build that can
// take a whole node is a build that can stop the control plane sharing it.
// Generous, because a cold Buildpacks build compiles a whole application.
func buildResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
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
