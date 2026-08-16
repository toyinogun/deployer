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
	if want := "https://" + cfg.MCPHost; cfg.MCPURL != want {
		t.Errorf("MCPURL = %q, want %q", cfg.MCPURL, want)
	}
	if cfg.MCPURL == cfg.ConsoleURL {
		t.Error("MCPURL and ConsoleURL are the same, so the two public surfaces have been conflated")
	}
}

// TestTheMCPHostMustBeOneLabelUnderTheAppDomain is spec 0022's AC-8, the same
// rule the console host holds and for the same reason: one label deep keeps the
// deploy host inside the wildcard certificate the apps already use.
func TestTheMCPHostMustBeOneLabelUnderTheAppDomain(t *testing.T) {
	// covers: AC-8
	t.Parallel()
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"mcp.apps.example.ts.net", true},
		{"deploy.apps.example.ts.net", true},
		{"mcp.staging.apps.example.ts.net", false},
		{"mcp", false},
		{"apps.example.ts.net", false},
		{"MCP.apps.example.ts.net", false},
		{"mcp.apps.example.ts.net.evil.test", false},
	} {
		_, err := Load(env(withValid(map[string]string{"DEPLOYER_MCP_HOST": tc.host})))
		switch {
		case tc.ok && err != nil:
			t.Errorf("%q was refused: %v", tc.host, err)
		case !tc.ok && err == nil:
			t.Errorf("%q was accepted, so a host outside the wildcard certificate would boot", tc.host)
		case !tc.ok && err != nil && !strings.Contains(err.Error(), "DEPLOYER_MCP_HOST"):
			t.Errorf("%q was refused without naming the setting: %v", tc.host, err)
		}
	}
}

// TestTheMCPHostIsRequired is AC-8. An agent is told where to upload from this
// value, so a platform without one is one nothing can deploy to.
func TestTheMCPHostIsRequired(t *testing.T) {
	// covers: AC-8
	t.Parallel()
	_, err := Load(env(withValid(map[string]string{"DEPLOYER_MCP_HOST": ""})))
	if err == nil {
		t.Fatal("a configuration with no deploy host was accepted")
	}
	if !strings.Contains(err.Error(), "DEPLOYER_MCP_HOST") {
		t.Errorf("the error does not name the missing setting: %v", err)
	}
}

// TestTheTwoPublicHostsMustDiffer is AC-8. Equal, they are one hostname carrying
// both surfaces, which is exactly the split spec 0021 made and this one keeps:
// the deploy path would answer on the console and the console's opt in
// registration would stop meaning anything.
func TestTheTwoPublicHostsMustDiffer(t *testing.T) {
	// covers: AC-8
	t.Parallel()
	cfg, err := Load(env(valid))
	if err != nil {
		t.Fatalf("loading a valid configuration failed: %v", err)
	}
	_, err = Load(env(withValid(map[string]string{"DEPLOYER_MCP_HOST": cfg.ConsoleHost})))
	if err == nil {
		t.Fatal("a configuration with one hostname for both surfaces was accepted")
	}
	if !strings.Contains(err.Error(), "DEPLOYER_MCP_HOST") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

// TestTheRemovedPublicURLFailsTheBoot is AC-9. Spec 0022 removed
// DEPLOYER_PUBLIC_URL and derived both public addresses from their hostnames.
// A manifest still carrying it is a manifest whose author believes it does
// something, so the boot says otherwise rather than ignoring it.
func TestTheRemovedPublicURLFailsTheBoot(t *testing.T) {
	// covers: AC-9
	t.Parallel()
	_, err := Load(env(withValid(map[string]string{"DEPLOYER_PUBLIC_URL": "https://deployer.example.ts.net"})))
	if err == nil {
		t.Fatal("a configuration still setting DEPLOYER_PUBLIC_URL was accepted")
	}
	if !strings.Contains(err.Error(), "DEPLOYER_PUBLIC_URL") {
		t.Errorf("the error does not name the removed setting: %v", err)
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
