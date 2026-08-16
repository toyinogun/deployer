package config

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The defaults for everything spec 0004 adds. They are here rather than inline
// so the numbers a deploy actually runs on can be read in one place.
const (
	defaultDeployTimeout     = 600 * time.Second
	defaultBuildTimeout      = 480 * time.Second
	defaultReadyTimeout      = 90 * time.Second
	defaultReconcileInterval = 2 * time.Second
	defaultAppCPU            = "100m"
	defaultAppMemory         = "128Mi"
	defaultAppLimitCPU       = "500m"
	defaultAppLimitMemory    = "512Mi"
	defaultMaxUploadFiles    = 20000
	defaultMaxExtractedBytes = 512 << 20
)

// loadFirstDeploy reads the settings spec 0004 adds and returns the names of any
// required variables that were absent and a message for every value that was
// present but unusable. Nothing here defers a check to first use: a bad number
// fails the boot naming the variable that holds it (AC-17).
func loadFirstDeploy(getenv func(string) string, c *Config) (missing, errs []string) {
	required := func(key string) string {
		v := getenv("DEPLOYER_" + key)
		if v == "" {
			missing = append(missing, "DEPLOYER_"+key)
		}
		return v
	}
	// seconds reads a whole number of seconds, falling back to the default when
	// the variable is absent.
	seconds := func(key string, fallback time.Duration) time.Duration {
		raw := getenv("DEPLOYER_" + key)
		if raw == "" {
			return fallback
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("DEPLOYER_%s must be a positive whole number of seconds, got %q", key, raw))
			return fallback
		}
		return time.Duration(n) * time.Second
	}
	// id reads a uid or gid: required, because an absent one is an assumed one,
	// and that is the whole failure this variable exists to prevent. Zero is
	// refused separately from a parse failure, since a build pod that starts as
	// root is refused by `restricted` anyway and is worth naming as itself.
	id := func(key string) int64 {
		raw := getenv("DEPLOYER_" + key)
		if raw == "" {
			missing = append(missing, "DEPLOYER_"+key)
			return 0
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err != nil || n < 0:
			errs = append(errs, fmt.Sprintf("DEPLOYER_%s must be a whole number, got %q", key, raw))
		case n == 0:
			errs = append(errs, fmt.Sprintf("DEPLOYER_%s must not be 0: a build pod never runs as root", key))
		}
		return n
	}
	optional := func(key, fallback string) string {
		if v := getenv("DEPLOYER_" + key); v != "" {
			return v
		}
		return fallback
	}

	c.BootstrapToken = getenv("DEPLOYER_BOOTSTRAP_TOKEN")
	c.InternalURL = required("INTERNAL_URL")
	c.BuilderImage = required("BUILDER_IMAGE")
	c.SelfImage = required("SELF_IMAGE")
	// The builder's own declared CNB_USER_ID and CNB_GROUP_ID. They belong to
	// BuilderImage, so they are repinned with it and CI checks the pair against
	// the pinned image's config rather than trusting what is set here.
	c.BuildUID = id("BUILD_UID")
	c.BuildGID = id("BUILD_GID")
	// The Dockerfile path's own engine, and its own uid pair. A BuildKit image
	// follows no CNB_ convention, so the pair comes from the OCI config's User
	// field. Reading either one off the wrong image is the failure this split
	// exists to prevent (spec 0009, AC-8).
	c.BuildkitImage = required("BUILDKIT_IMAGE")
	c.BuildkitUID = id("BUILDKIT_UID")
	c.BuildkitGID = id("BUILDKIT_GID")

	// The one configured address left. Both public ones are derived from their
	// hostnames in edge.go, so they cannot be malformed (spec 0022, AC-9).
	if c.InternalURL != "" {
		u, err := url.Parse(c.InternalURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Sprintf("DEPLOYER_INTERNAL_URL must be an absolute http or https address, got %q", c.InternalURL))
		}
		c.InternalURL = strings.TrimRight(c.InternalURL, "/")
	}
	// Both images run somewhere the platform cannot re-check later, so a mutable
	// tag here would quietly break the rule that the platform only ever runs what
	// it pinned. Catch it at boot instead.
	for _, img := range []struct{ key, value string }{
		{"DEPLOYER_BUILDER_IMAGE", c.BuilderImage},
		{"DEPLOYER_BUILDKIT_IMAGE", c.BuildkitImage},
		{"DEPLOYER_SELF_IMAGE", c.SelfImage},
	} {
		if img.value != "" && !strings.Contains(img.value, "@sha256:") {
			errs = append(errs, fmt.Sprintf("%s must be pinned by digest (name@sha256:...), got %q", img.key, img.value))
		}
	}

	c.DeployTimeout = seconds("DEPLOY_TIMEOUT_SECONDS", defaultDeployTimeout)
	c.BuildTimeout = seconds("BUILD_TIMEOUT_SECONDS", defaultBuildTimeout)
	c.ReadyTimeout = seconds("READY_TIMEOUT_SECONDS", defaultReadyTimeout)
	c.ReconcileInterval = seconds("RECONCILE_INTERVAL_SECONDS", defaultReconcileInterval)
	// A build allowed to outlast the call it is serving is a deploy that can only
	// end in a timeout, so it is a configuration error rather than a surprise.
	if c.BuildTimeout >= c.DeployTimeout {
		errs = append(errs, fmt.Sprintf(
			"DEPLOYER_BUILD_TIMEOUT_SECONDS (%s) must be shorter than DEPLOYER_DEPLOY_TIMEOUT_SECONDS (%s)",
			c.BuildTimeout, c.DeployTimeout))
	}

	c.AppCPU = optional("APP_DEFAULT_CPU", defaultAppCPU)
	c.AppMemory = optional("APP_DEFAULT_MEMORY", defaultAppMemory)
	c.AppLimitCPU = optional("APP_LIMIT_CPU", defaultAppLimitCPU)
	c.AppLimitMemory = optional("APP_LIMIT_MEMORY", defaultAppLimitMemory)
	errs = append(errs, quantityErrors(map[string]string{
		"DEPLOYER_APP_DEFAULT_CPU":    c.AppCPU,
		"DEPLOYER_APP_DEFAULT_MEMORY": c.AppMemory,
		"DEPLOYER_APP_LIMIT_CPU":      c.AppLimitCPU,
		"DEPLOYER_APP_LIMIT_MEMORY":   c.AppLimitMemory,
	})...)

	c.MaxUploadFiles = defaultMaxUploadFiles
	if raw := getenv("DEPLOYER_MAX_UPLOAD_FILES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("DEPLOYER_MAX_UPLOAD_FILES must be a positive integer, got %q", raw))
		} else {
			c.MaxUploadFiles = n
		}
	}
	c.MaxExtractedBytes = defaultMaxExtractedBytes
	if raw := getenv("DEPLOYER_MAX_EXTRACTED_BYTES"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("DEPLOYER_MAX_EXTRACTED_BYTES must be a positive integer, got %q", raw))
		} else {
			c.MaxExtractedBytes = n
		}
	}
	return missing, errs
}

// quantityErrors reports every value that is not a positive Kubernetes quantity,
// in a stable order so a boot failure reads the same way twice. These go straight
// into a pod spec, so nonsense is worth catching at boot rather than having the
// API server reject the first deploy.
func quantityErrors(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var errs []string
	for _, key := range keys {
		parsed, err := resource.ParseQuantity(values[key])
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("%s must be a Kubernetes quantity, got %q", key, values[key]))
		case parsed.Sign() <= 0:
			errs = append(errs, fmt.Sprintf("%s must be greater than zero, got %q", key, values[key]))
		}
	}
	return errs
}
