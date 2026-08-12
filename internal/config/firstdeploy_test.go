package config

import (
	"strings"
	"testing"
	"time"
)

func TestFirstDeployDefaults(t *testing.T) {
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"DeployTimeout", c.DeployTimeout, 600 * time.Second},
		{"BuildTimeout", c.BuildTimeout, 480 * time.Second},
		{"ReadyTimeout", c.ReadyTimeout, 90 * time.Second},
		{"ReconcileInterval", c.ReconcileInterval, 2 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if c.AppCPU != "100m" || c.AppMemory != "128Mi" {
		t.Errorf("app requests = %q/%q, want 100m/128Mi", c.AppCPU, c.AppMemory)
	}
	if c.AppLimitCPU != "500m" || c.AppLimitMemory != "512Mi" {
		t.Errorf("app limits = %q/%q, want 500m/512Mi", c.AppLimitCPU, c.AppLimitMemory)
	}
	if c.MaxUploadFiles != 20000 || c.MaxExtractedBytes != 512<<20 {
		t.Errorf("extraction caps = %d/%d, want 20000/%d", c.MaxUploadFiles, c.MaxExtractedBytes, 512<<20)
	}
	// Unset is a supported local run, not a failure.
	if c.BootstrapToken != "" {
		t.Errorf("BootstrapToken = %q, want empty", c.BootstrapToken)
	}
}

func TestFirstDeployOverrides(t *testing.T) {
	c, err := Load(with(map[string]string{
		"DEPLOYER_BOOTSTRAP_TOKEN":            "dpl_secret",
		"DEPLOYER_PUBLIC_URL":                 "https://deployer.example.ts.net/",
		"DEPLOYER_INTERNAL_URL":               "http://deployer.deployer-system.svc/",
		"DEPLOYER_DEPLOY_TIMEOUT_SECONDS":     "900",
		"DEPLOYER_BUILD_TIMEOUT_SECONDS":      "300",
		"DEPLOYER_READY_TIMEOUT_SECONDS":      "45",
		"DEPLOYER_RECONCILE_INTERVAL_SECONDS": "5",
		"DEPLOYER_APP_DEFAULT_CPU":            "250m",
		"DEPLOYER_MAX_UPLOAD_FILES":           "10",
		"DEPLOYER_MAX_EXTRACTED_BYTES":        "1024",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BootstrapToken != "dpl_secret" {
		t.Errorf("BootstrapToken = %q", c.BootstrapToken)
	}
	// The trailing slash comes off, so joining a path never doubles it.
	if c.PublicURL != "https://deployer.example.ts.net" {
		t.Errorf("PublicURL = %q, want the trailing slash trimmed", c.PublicURL)
	}
	if c.DeployTimeout != 900*time.Second || c.BuildTimeout != 300*time.Second {
		t.Errorf("timeouts = %v/%v", c.DeployTimeout, c.BuildTimeout)
	}
	if c.ReadyTimeout != 45*time.Second || c.ReconcileInterval != 5*time.Second {
		t.Errorf("waits = %v/%v", c.ReadyTimeout, c.ReconcileInterval)
	}
	if c.AppCPU != "250m" {
		t.Errorf("AppCPU = %q", c.AppCPU)
	}
	if c.MaxUploadFiles != 10 || c.MaxExtractedBytes != 1024 {
		t.Errorf("caps = %d/%d", c.MaxUploadFiles, c.MaxExtractedBytes)
	}
}

// Every bad value fails the boot naming the variable that holds it (AC-17).
func TestFirstDeployRejectsBadValues(t *testing.T) {
	cases := []struct {
		name     string
		override map[string]string
		want     string
	}{
		{
			"a build timeout that is not a number",
			map[string]string{"DEPLOYER_BUILD_TIMEOUT_SECONDS": "soon"},
			"DEPLOYER_BUILD_TIMEOUT_SECONDS",
		},
		{
			"a zero ready timeout",
			map[string]string{"DEPLOYER_READY_TIMEOUT_SECONDS": "0"},
			"DEPLOYER_READY_TIMEOUT_SECONDS",
		},
		{
			"a build budget longer than the whole deploy budget",
			map[string]string{
				"DEPLOYER_BUILD_TIMEOUT_SECONDS":  "600",
				"DEPLOYER_DEPLOY_TIMEOUT_SECONDS": "300",
			},
			"must be shorter than",
		},
		{
			"a public URL with no scheme",
			map[string]string{"DEPLOYER_PUBLIC_URL": "deployer.example.ts.net"},
			"DEPLOYER_PUBLIC_URL",
		},
		{
			"a builder image pinned to a mutable tag",
			map[string]string{"DEPLOYER_BUILDER_IMAGE": "paketobuildpacks/builder-jammy-base:latest"},
			"DEPLOYER_BUILDER_IMAGE",
		},
		{
			"a self image pinned to a mutable tag",
			map[string]string{"DEPLOYER_SELF_IMAGE": "ghcr.io/toyinogun/deployer:main"},
			"DEPLOYER_SELF_IMAGE",
		},
		{
			"an app CPU request that is not a quantity",
			map[string]string{"DEPLOYER_APP_DEFAULT_CPU": "lots"},
			"DEPLOYER_APP_DEFAULT_CPU",
		},
		{
			"a negative app memory limit",
			map[string]string{"DEPLOYER_APP_LIMIT_MEMORY": "-1Mi"},
			"DEPLOYER_APP_LIMIT_MEMORY",
		},
		{
			"a file cap that is not a number",
			map[string]string{"DEPLOYER_MAX_UPLOAD_FILES": "many"},
			"DEPLOYER_MAX_UPLOAD_FILES",
		},
		{
			"an extracted byte cap of zero",
			map[string]string{"DEPLOYER_MAX_EXTRACTED_BYTES": "0"},
			"DEPLOYER_MAX_EXTRACTED_BYTES",
		},
		{
			"a build uid that is not a number",
			map[string]string{"DEPLOYER_BUILD_UID": "cnb"},
			"DEPLOYER_BUILD_UID",
		},
		// Not a parse failure but its own answer: a build pod that starts as root
		// is refused by `restricted` anyway, so say which variable asked for it.
		{
			"a build uid of root",
			map[string]string{"DEPLOYER_BUILD_UID": "0"},
			"never runs as root",
		},
		{
			"a negative build gid",
			map[string]string{"DEPLOYER_BUILD_GID": "-1"},
			"DEPLOYER_BUILD_GID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(with(tc.override))
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The build uid and gid are required rather than defaulted on purpose: a default
// is an assumption about an image the platform did not build, and assuming it is
// exactly what broke every real build once already.
func TestFirstDeployRequiresItsRequiredVars(t *testing.T) {
	want := []string{
		"DEPLOYER_PUBLIC_URL", "DEPLOYER_INTERNAL_URL",
		"DEPLOYER_BUILDER_IMAGE", "DEPLOYER_SELF_IMAGE",
		"DEPLOYER_BUILD_UID", "DEPLOYER_BUILD_GID",
	}
	dropped := map[string]bool{}
	for _, k := range want {
		dropped[k] = true
	}
	m := map[string]string{}
	for k, v := range valid {
		if !dropped[k] {
			m[k] = v
		}
	}
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %s", err, w)
		}
	}
}
