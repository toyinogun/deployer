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
		MaxUploadBytes: 100 << 20,
		DBBusyTimeout:  5000 * time.Millisecond,
		Retention:      90 * 24 * time.Hour,

		// The control plane's own namespace comes from the downward API, so an
		// unset value means the pod spec is wrong rather than the operator being
		// lazy, and it is worth failing the boot over (spec 0003).
		Namespace:        required("NAMESPACE"),
		IngressClassName: optional("INGRESS_CLASS_NAME", "nginx"),
		AppQuotaCPU:      optional("APP_QUOTA_CPU", "1"),
		AppQuotaMemory:   optional("APP_QUOTA_MEMORY", "1Gi"),
		AppQuotaPods:     5,
	}

	var errs []string
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
	if raw := getenv("DEPLOYER_APP_QUOTA_PODS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("DEPLOYER_APP_QUOTA_PODS must be a positive integer, got %q", raw))
		} else {
			c.AppQuotaPods = n
		}
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
	if len(missing) > 0 {
		errs = append(errs, "missing required environment: "+strings.Join(missing, ", "))
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}
