package config

import (
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var valid = map[string]string{
	"DEPLOYER_REGISTRY_HOST":     "registry.deployer-system.svc:5000",
	"DEPLOYER_REGISTRY_USER":     "pusher",
	"DEPLOYER_REGISTRY_PASSWORD": "s3cret",
	"DEPLOYER_APP_DOMAIN":        "apps.example.ts.net",
	"DEPLOYER_NAMESPACE":         "deployer-system",
}

// withValid returns the valid environment plus the overrides given, so a test
// changes one thing without restating the rest.
func withValid(overrides map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range valid {
		m[k] = v
	}
	for k, v := range overrides {
		m[k] = v
	}
	return m
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Listen != ":8080" || c.BuildNamespace != "deployer-builds" {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.MaxUploadBytes != 100<<20 {
		t.Errorf("MaxUploadBytes = %d, want %d", c.MaxUploadBytes, 100<<20)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", c.LogLevel)
	}
}

func TestLoadReportsEveryMissingRequiredVar(t *testing.T) {
	_, err := Load(env(map[string]string{"DEPLOYER_REGISTRY_HOST": "r:5000"}))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"DEPLOYER_REGISTRY_USER", "DEPLOYER_REGISTRY_PASSWORD", "DEPLOYER_APP_DOMAIN", "DEPLOYER_NAMESPACE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadRejectsBadNumbersAndLevels(t *testing.T) {
	m := withValid(map[string]string{
		"DEPLOYER_MAX_UPLOAD_BYTES": "-1",
		"DEPLOYER_LOG_LEVEL":        "chatty",
	})
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "MAX_UPLOAD_BYTES") || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Errorf("error %q should name both bad values", err)
	}
}

func TestLoadParsesOverrides(t *testing.T) {
	m := withValid(map[string]string{
		"DEPLOYER_MAX_UPLOAD_BYTES": "1048576",
		"DEPLOYER_LOG_LEVEL":        "debug",
	})
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.MaxUploadBytes != 1048576 || c.LogLevel != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v", c)
	}
}

// The cluster settings spec 0003 adds. AC-16: every one is validated at startup
// and a malformed value fails the boot with an error naming the variable.

func TestClusterSettingDefaults(t *testing.T) {
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Namespace != "deployer-system" {
		t.Errorf("Namespace = %q, want deployer-system", c.Namespace)
	}
	if c.IngressClassName != "nginx" {
		t.Errorf("IngressClassName = %q, want nginx", c.IngressClassName)
	}
	if c.AppQuotaCPU != "1" || c.AppQuotaMemory != "1Gi" || c.AppQuotaPods != 5 {
		t.Errorf("quota defaults not applied: cpu=%q memory=%q pods=%d",
			c.AppQuotaCPU, c.AppQuotaMemory, c.AppQuotaPods)
	}
}

func TestClusterSettingOverrides(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{
		"DEPLOYER_INGRESS_CLASS_NAME": "traefik",
		"DEPLOYER_APP_QUOTA_CPU":      "2500m",
		"DEPLOYER_APP_QUOTA_MEMORY":   "2Gi",
		"DEPLOYER_APP_QUOTA_PODS":     "8",
	})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.IngressClassName != "traefik" || c.AppQuotaCPU != "2500m" ||
		c.AppQuotaMemory != "2Gi" || c.AppQuotaPods != 8 {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func TestClusterSettingsRejectMalformedValues(t *testing.T) {
	cases := []struct{ key, value string }{
		{"DEPLOYER_APP_QUOTA_CPU", "banana"},
		{"DEPLOYER_APP_QUOTA_CPU", "-1"},
		{"DEPLOYER_APP_QUOTA_MEMORY", "banana"},
		{"DEPLOYER_APP_QUOTA_MEMORY", "0"},
		{"DEPLOYER_APP_QUOTA_PODS", "banana"},
		{"DEPLOYER_APP_QUOTA_PODS", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			_, err := Load(env(withValid(map[string]string{tc.key: tc.value})))
			if err == nil {
				t.Fatalf("want an error for %s=%q, got nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
		})
	}
}
