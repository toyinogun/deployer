package config

import (
	"strings"
	"testing"
)

// TestConsoleHostMustBeOneLabelUnderTheAppDomain is AC-1. One label deep and no
// deeper, which is what keeps the console inside the wildcard certificate the
// apps already use.
func TestConsoleHostMustBeOneLabelUnderTheAppDomain(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"console.apps.example.ts.net", true},
		{"www.apps.example.ts.net", true},
		{"console.staging.apps.example.ts.net", false},
		{"console", false},
		{"apps.example.ts.net", false},
		{".apps.example.ts.net", false},
		{"CONSOLE.apps.example.ts.net", false},
		{"con_sole.apps.example.ts.net", false},
		{"console.apps.example.ts.net.evil.test", false},
	} {
		_, err := Load(env(withValid(map[string]string{"DEPLOYER_CONSOLE_HOST": tc.host})))
		switch {
		case tc.ok && err != nil:
			t.Errorf("%q was refused: %v", tc.host, err)
		case !tc.ok && err == nil:
			t.Errorf("%q was accepted, so a host outside the wildcard certificate would boot", tc.host)
		case !tc.ok && err != nil && !strings.Contains(err.Error(), "DEPLOYER_CONSOLE_HOST"):
			t.Errorf("%q was refused without naming the setting: %v", tc.host, err)
		}
	}
}

// TestTheConsoleURLIsDerivedNotConfigured is AC-1. There is one place the
// console's name lives, so the host and the address a person clicks cannot
// disagree.
func TestTheConsoleURLIsDerivedNotConfigured(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	cfg, err := Load(env(valid))
	if err != nil {
		t.Fatalf("loading a valid configuration failed: %v", err)
	}
	if want := "https://" + cfg.ConsoleHost; cfg.ConsoleURL != want {
		t.Errorf("ConsoleURL = %q, want %q", cfg.ConsoleURL, want)
	}
	if cfg.PublicURL == cfg.ConsoleURL {
		t.Error("PublicURL and ConsoleURL are the same, so the tailnet address and the public one have been conflated")
	}
}

// TestTheConsoleHostIsRequired is AC-1. Every link a person clicks in a mail is
// built from it, so a platform without one is one nobody can finish registering
// on, and that is worth failing a boot over.
func TestTheConsoleHostIsRequired(t *testing.T) {
	// covers: AC-1
	t.Parallel()
	_, err := Load(env(withValid(map[string]string{"DEPLOYER_CONSOLE_HOST": ""})))
	if err == nil {
		t.Fatal("a configuration with no console host was accepted")
	}
	if !strings.Contains(err.Error(), "DEPLOYER_CONSOLE_HOST") {
		t.Errorf("the error does not name the missing setting: %v", err)
	}
}
