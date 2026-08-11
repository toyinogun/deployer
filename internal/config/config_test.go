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
	for _, want := range []string{"DEPLOYER_REGISTRY_USER", "DEPLOYER_REGISTRY_PASSWORD", "DEPLOYER_APP_DOMAIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadRejectsBadNumbersAndLevels(t *testing.T) {
	m := map[string]string{}
	for k, v := range valid {
		m[k] = v
	}
	m["DEPLOYER_MAX_UPLOAD_BYTES"] = "-1"
	m["DEPLOYER_LOG_LEVEL"] = "chatty"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "MAX_UPLOAD_BYTES") || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Errorf("error %q should name both bad values", err)
	}
}

func TestLoadParsesOverrides(t *testing.T) {
	m := map[string]string{}
	for k, v := range valid {
		m[k] = v
	}
	m["DEPLOYER_MAX_UPLOAD_BYTES"] = "1048576"
	m["DEPLOYER_LOG_LEVEL"] = "debug"
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.MaxUploadBytes != 1048576 || c.LogLevel != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v", c)
	}
}
