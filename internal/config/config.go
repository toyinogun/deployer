// Package config loads the platform's runtime configuration from the environment.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Config is every value the control plane reads from its environment.
// Names and defaults come from spec 0001, "Configuration required".
type Config struct {
	Listen         string // where the HTTP server binds
	DBPath         string // SQLite file on the mounted volume
	UploadDir      string // where uploaded tarballs land before a build claims them
	RegistryHost   string // in cluster registry service address
	RegistryUser   string // push credential, from the SealedSecret
	RegistryPass   string // push credential, from the SealedSecret
	AppDomain      string // wildcard domain apps are served under
	BuildNamespace string // where build Jobs are created
	MaxUploadBytes int64  // hard cap on an accepted tarball
	LogLevel       slog.Level

	// Added by spec 0002, the platform data model.
	DBBusyTimeout time.Duration // how long a blocked SQLite writer waits
	Retention     time.Duration // how long events and terminal failures are kept
	PodName       string        // this pod's own name, recorded on a deployment claim

	// Added by spec 0003, the cluster foundation.
	Namespace        string // the control plane's own namespace, from the downward API
	IngressClassName string // ingress class for the Ingress objects apps are served on
	AppQuotaCPU      string // per app CPU ceiling, a Kubernetes quantity
	AppQuotaMemory   string // per app memory ceiling, a Kubernetes quantity
	AppQuotaPods     int    // per app pod ceiling

	// Added by spec 0016, the per account app cap.
	MaxAppsPerAccount int // how many live apps one account may hold

	// Added by spec 0004, the first deploy end to end. Loaded and validated by
	// loadFirstDeploy in firstdeploy.go.

	// BootstrapToken is the single API token seeded at startup. Empty means no
	// seeding runs and the platform boots with no usable token, which is a
	// warning rather than a failure so a local run needs no secret.
	BootstrapToken string
	// PublicURL is the platform's own reachable base address. It goes into the
	// deploy_app tool description, so an agent can only upload if it is right.
	PublicURL string
	// InternalURL is the platform's address as reached from inside the cluster.
	// The build Job's init container fetches the source through it, and it runs
	// on cluster DNS, which cannot resolve the tailnet name PublicURL carries.
	InternalURL string
	// BuilderImage is the digest pinned Paketo builder the lifecycle runs from.
	BuilderImage string
	// SelfImage is the control plane's own image, reused as the build Job's init
	// container. The downward API cannot supply it, so the manifest sets it
	// beside the same digest CI pins.
	SelfImage string
	// BuildUID and BuildGID are the CNB_USER_ID and CNB_GROUP_ID BuilderImage
	// declares. The build pod has to start as that user, because the lifecycle
	// under `restricted` holds no capability to switch to it. They are set beside
	// the builder digest and repinned with it.
	BuildUID int64
	BuildGID int64

	// Added by spec 0009, the Dockerfile build path.

	// BuildkitImage is the digest pinned rootless BuildKit image the Dockerfile
	// path builds with. Pinned exactly as BuilderImage is, for the same reason.
	BuildkitImage string
	// BuildkitUID and BuildkitGID are the pair BuildkitImage declares in its own
	// OCI config User field. They belong to that image, never to the Paketo one,
	// and CI checks them against the pinned digest the same way.
	BuildkitUID int64
	BuildkitGID int64
	// BuildkitNamespace is where Dockerfile build Jobs are created. It is a
	// second namespace rather than a second Job in the first one because the two
	// enforce different pod security levels: BuildKit is admitted only at
	// `privileged`, and BuildNamespace stays at `restricted` so a mis routed Job
	// is refused rather than run. That is also why the two are refused when they
	// are equal: one namespace cannot enforce both (spec 0009, AC-22).
	BuildkitNamespace string

	// DeployTimeout is one deployment's whole budget, measured from created_at to
	// a terminal state and enforced by the reconcile loop. It does not bound an
	// MCP call: deploy_app returns straight away (spec 0005, AC-17).
	DeployTimeout     time.Duration
	BuildTimeout      time.Duration // the build Job's activeDeadlineSeconds
	ReadyTimeout      time.Duration // how long to wait for an available replica
	ReconcileInterval time.Duration // the loop tick

	AppCPU         string // an app container's CPU request
	AppMemory      string // an app container's memory request
	AppLimitCPU    string // an app container's CPU limit
	AppLimitMemory string // an app container's memory limit

	// MaxUploadFiles and MaxExtractedBytes cap what the source extractor will
	// unpack, so a zip bomb fails the deployment rather than the node.
	MaxUploadFiles    int
	MaxExtractedBytes int64

	// Added by spec 0007, accounts and API tokens.

	// ResendAPIKey is the credential the mail sender posts with. Optional: unset
	// means no sender, and the endpoints that exist only to send mail answer
	// mail_unavailable while everything else works normally (AC-26). Never logged.
	ResendAPIKey string
	// MailFrom is the address every message is sent as. Required whenever
	// ResendAPIKey is set, and validated together with it here rather than
	// discovered to be empty by the first registration.
	MailFrom string

	// Added by spec 0008, workload isolation and network policy.

	// AppEgressBlockedCIDRs are the ranges an app may not reach, which become the
	// `except` list of its egress allow rule. App to app isolation rides on this
	// list: another app's pod IP and Service IP are unreachable because both
	// ranges sit inside it, so narrowing it for an unrelated reason weakens that
	// isolation as a side effect (spec 0008, Key invariants).
	AppEgressBlockedCIDRs []string

	// Added by spec 0017, bounded app egress.

	// AppEgressBlockedPorts are the TCP ports an app may not reach on any public
	// address, deduplicated and sorted. They never appear in a policy directly: a
	// NetworkPolicy can only permit a port, so deploy composes the complement of
	// this list over 1..65535 and a blocked port is one no range names. Removing a
	// port from the list opens it for every app on the platform at once (spec
	// 0017, Key invariants).
	AppEgressBlockedPorts []int32

	// Added by spec 0012, the app lifecycle.

	// ReapInterval is how often the orphan namespace reaper runs, on its own
	// ticker rather than the reconcile tick, because a pass lists every app
	// namespace on the cluster. Default ten minutes.
	ReapInterval time.Duration
	// OrphanGrace is how old an app namespace must be before the reaper will
	// consider it orphaned. Default fifteen minutes.
	OrphanGrace time.Duration

	// Added by spec 0013, the web interface. Loaded and validated by loadWeb in
	// web.go.

	// CSRFKey is the secret every page form's synchroniser token is derived
	// under, as the HMAC SHA256 of the session id. It is never stored per form
	// and never leaves the process, so rotating it only invalidates the forms
	// that are open at that moment.
	CSRFKey string
}

// Load reads the DEPLOYER_* environment through getenv and returns the config,
// or an error naming every problem at once so a bad deploy fails on the first boot.
func Load(getenv func(string) string) (Config, error) {
	var missing []string
	required := func(key string) string {
		v := getenv("DEPLOYER_" + key)
		if v == "" {
			missing = append(missing, "DEPLOYER_"+key)
		}
		return v
	}
	optional := func(key, fallback string) string {
		if v := getenv("DEPLOYER_" + key); v != "" {
			return v
		}
		return fallback
	}

	c := Config{
		Listen:         optional("LISTEN", ":8080"),
		DBPath:         optional("DB_PATH", "/data/deployer.db"),
		UploadDir:      optional("UPLOAD_DIR", "/data/uploads"),
		RegistryHost:   required("REGISTRY_HOST"),
		RegistryUser:   required("REGISTRY_USER"),
		RegistryPass:   required("REGISTRY_PASSWORD"),
		AppDomain:      required("APP_DOMAIN"),
		BuildNamespace: optional("BUILD_NAMESPACE", "deployer-builds"),
		// The Dockerfile path's own namespace, at `privileged`. Validated below
		// beside the one above, and refused if the two are the same.
		BuildkitNamespace: optional("BUILDKIT_NAMESPACE", "deployer-builds-dockerfile"),
		MaxUploadBytes:    100 << 20,
		DBBusyTimeout:     5000 * time.Millisecond,
		Retention:         90 * 24 * time.Hour,

		// The control plane's own namespace comes from the downward API, so an
		// unset value means the pod spec is wrong rather than the operator being
		// lazy, and it is worth failing the boot over (spec 0003).
		Namespace:        required("NAMESPACE"),
		IngressClassName: optional("INGRESS_CLASS_NAME", "nginx"),
		AppQuotaCPU:      optional("APP_QUOTA_CPU", "1"),
		AppQuotaMemory:   optional("APP_QUOTA_MEMORY", "1Gi"),
		AppQuotaPods:     5,

		MaxAppsPerAccount: 10,
	}

	var errs []string
	// Both build namespaces go straight into a Job's metadata, so a name the API
	// server would refuse is caught at boot rather than at the first deploy. They
	// are checked as a pair because being equal is the failure that matters most:
	// the two enforce different pod security levels, so a shared name means every
	// build of one kind is refused by admission, which surfaces as a deployment
	// that timed out rather than as the configuration mistake it is (AC-22).
	for _, ns := range []struct{ key, value string }{
		{"DEPLOYER_BUILD_NAMESPACE", c.BuildNamespace},
		{"DEPLOYER_BUILDKIT_NAMESPACE", c.BuildkitNamespace},
	} {
		if problems := validation.IsDNS1123Label(ns.value); len(problems) > 0 {
			errs = append(errs, fmt.Sprintf("%s must be a Kubernetes namespace name, got %q: %s",
				ns.key, ns.value, strings.Join(problems, "; ")))
		}
	}
	if c.BuildNamespace == c.BuildkitNamespace {
		errs = append(errs, fmt.Sprintf(
			"DEPLOYER_BUILD_NAMESPACE and DEPLOYER_BUILDKIT_NAMESPACE must not be the same namespace (both %q): "+
				"one enforces restricted for Buildpacks and the other privileged for BuildKit",
			c.BuildNamespace))
	}
	// The quota ceilings go straight into a ResourceQuota, so they are parsed as
	// Kubernetes quantities here rather than discovered to be nonsense by the API
	// server at the first deploy.
	for _, q := range []struct {
		key   string
		value string
	}{
		{"DEPLOYER_APP_QUOTA_CPU", c.AppQuotaCPU},
		{"DEPLOYER_APP_QUOTA_MEMORY", c.AppQuotaMemory},
	} {
		parsed, err := resource.ParseQuantity(q.value)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s must be a Kubernetes quantity, got %q", q.key, q.value))
			continue
		}
		if parsed.Sign() <= 0 {
			errs = append(errs, fmt.Sprintf("%s must be greater than zero, got %q", q.key, q.value))
		}
	}
	// The two ceilings that are plain counts: how many pods one app may run, and
	// how many live apps one account may hold. Both are optional, both keep their
	// default when unset, and both fail the boot rather than the first deploy.
	// There is no value meaning no cap (spec 0016, AC-7).
	for _, n := range []struct {
		key    string
		target *int
	}{
		{"DEPLOYER_APP_QUOTA_PODS", &c.AppQuotaPods},
		{"DEPLOYER_MAX_APPS_PER_ACCOUNT", &c.MaxAppsPerAccount},
	} {
		raw := getenv(n.key)
		if raw == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			errs = append(errs, fmt.Sprintf("%s must be a positive integer, got %q", n.key, raw))
			continue
		}
		*n.target = parsed
	}
	// The claiming pod names itself through the downward API. A local run has no
	// downward API, so the hostname stands in and the claim still records who.
	c.PodName = optional("POD_NAME", "")
	if c.PodName == "" {
		host, err := os.Hostname()
		if err != nil {
			errs = append(errs, fmt.Sprintf("DEPLOYER_POD_NAME is unset and the hostname is unreadable: %v", err))
		}
		c.PodName = host
	}
	if raw := getenv("DEPLOYER_DB_BUSY_TIMEOUT_MS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("DEPLOYER_DB_BUSY_TIMEOUT_MS must be a positive integer, got %q", raw))
		} else {
			c.DBBusyTimeout = time.Duration(n) * time.Millisecond
		}
	}
	if raw := getenv("DEPLOYER_RETENTION_DAYS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("DEPLOYER_RETENTION_DAYS must be a positive integer, got %q", raw))
		} else {
			c.Retention = time.Duration(n) * 24 * time.Hour
		}
	}
	if raw := getenv("DEPLOYER_MAX_UPLOAD_BYTES"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("DEPLOYER_MAX_UPLOAD_BYTES must be a positive integer, got %q", raw))
		} else {
			c.MaxUploadBytes = n
		}
	}
	if raw := getenv("DEPLOYER_LOG_LEVEL"); raw != "" {
		if err := c.LogLevel.UnmarshalText([]byte(raw)); err != nil {
			errs = append(errs, fmt.Sprintf("DEPLOYER_LOG_LEVEL must be debug, info, warn or error, got %q", raw))
		}
	}
	deployMissing, deployErrs := loadFirstDeploy(getenv, &c)
	missing = append(missing, deployMissing...)
	errs = append(errs, deployErrs...)
	errs = append(errs, loadIdentity(getenv, &c)...)
	webMissing, webErrs := loadWeb(getenv, &c)
	missing = append(missing, webMissing...)
	errs = append(errs, webErrs...)
	errs = append(errs, loadIsolation(getenv, &c)...)
	errs = append(errs, loadPortBound(getenv, &c)...)
	errs = append(errs, loadLifecycle(getenv, &c)...)

	if len(missing) > 0 {
		errs = append(errs, "missing required environment: "+strings.Join(missing, ", "))
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}
