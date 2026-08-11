package config

import (
	"maps"
	"os"
	"strings"
	"testing"
	"time"
)

// with returns the valid environment plus the given overrides.
func with(extra map[string]string) func(string) string {
	m := maps.Clone(valid)
	maps.Copy(m, extra)
	return env(m)
}

func TestDataModelDefaults(t *testing.T) {
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.DBBusyTimeout != 5*time.Second {
		t.Errorf("DBBusyTimeout = %v, want 5s", c.DBBusyTimeout)
	}
	if c.Retention != 90*24*time.Hour {
		t.Errorf("Retention = %v, want 90 days", c.Retention)
	}
	// With no downward API the pod names itself after the host, so a local run
	// still records who claimed a deployment.
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname on this machine: %v", err)
	}
	if c.PodName != host {
		t.Errorf("PodName = %q, want the hostname %q", c.PodName, host)
	}
}

func TestDataModelOverrides(t *testing.T) {
	c, err := Load(with(map[string]string{
		"DEPLOYER_DB_BUSY_TIMEOUT_MS": "12000",
		"DEPLOYER_RETENTION_DAYS":     "30",
		"DEPLOYER_POD_NAME":           "deployer-7c9f-abcde",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.DBBusyTimeout != 12*time.Second {
		t.Errorf("DBBusyTimeout = %v, want 12s", c.DBBusyTimeout)
	}
	if c.Retention != 30*24*time.Hour {
		t.Errorf("Retention = %v, want 30 days", c.Retention)
	}
	if c.PodName != "deployer-7c9f-abcde" {
		t.Errorf("PodName = %q, want the injected pod name", c.PodName)
	}
}

func TestDataModelRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"a busy timeout that is not a number", "DEPLOYER_DB_BUSY_TIMEOUT_MS", "soon"},
		{"a busy timeout of zero", "DEPLOYER_DB_BUSY_TIMEOUT_MS", "0"},
		{"a negative busy timeout", "DEPLOYER_DB_BUSY_TIMEOUT_MS", "-1"},
		{"a retention that is not a number", "DEPLOYER_RETENTION_DAYS", "forever"},
		{"a retention of zero", "DEPLOYER_RETENTION_DAYS", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(with(map[string]string{tt.key: tt.val}))
			if err == nil {
				t.Fatalf("%s=%q was accepted", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("the error does not name %s: %v", tt.key, err)
			}
		})
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	// A bad deploy should fail on its first boot with the whole list, not one
	// problem at a time.
	_, err := Load(env(map[string]string{
		"DEPLOYER_DB_BUSY_TIMEOUT_MS": "soon",
		"DEPLOYER_RETENTION_DAYS":     "forever",
	}))
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	for _, want := range []string{
		"DEPLOYER_DB_BUSY_TIMEOUT_MS", "DEPLOYER_RETENTION_DAYS",
		"DEPLOYER_REGISTRY_HOST", "DEPLOYER_APP_DOMAIN",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}
}
