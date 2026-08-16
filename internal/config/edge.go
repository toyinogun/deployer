package config

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// loadEdge reads the settings spec 0021 adds: the public console hostname, and
// the console's own base address derived from it.
//
// The host is required. Everything a person clicks in a mail is built from it,
// so a platform that has one hostname configured and another written into its
// verification links is a platform nobody can finish registering on, and that is
// worth failing a boot over rather than discovering one registration at a time.
func loadEdge(getenv func(string) string, c *Config) (missing []string, errs []string) {
	c.ConsoleHost = getenv("DEPLOYER_CONSOLE_HOST")
	if c.ConsoleHost == "" {
		return []string{"DEPLOYER_CONSOLE_HOST"}, nil
	}
	if errs := consoleHostProblems(c.ConsoleHost, c.AppDomain); len(errs) > 0 {
		return nil, errs
	}
	// Derived, never configured. There is one place the console's name lives, so
	// the host and the address cannot disagree (spec 0021, AC-1).
	c.ConsoleURL = "https://" + c.ConsoleHost
	return nil, nil
}

// consoleHostProblems reports why host is not a single label under domain.
//
// Exactly one label deep, and no deeper: `console.deploy.example.org` passes
// against `deploy.example.org`, `console.staging.deploy.example.org` does not.
// That keeps the console inside the wildcard certificate the apps already use,
// which covers one level and no more (spec 0021, AC-1).
func consoleHostProblems(host, domain string) []string {
	if domain == "" {
		// DEPLOYER_APP_DOMAIN is required and its own absence is already
		// reported, so saying nothing here keeps one mistake to one message.
		return nil
	}
	suffix := "." + domain
	label, found := strings.CutSuffix(host, suffix)
	if !found {
		return []string{fmt.Sprintf(
			"DEPLOYER_CONSOLE_HOST must be one label under DEPLOYER_APP_DOMAIN (%s), got %q", domain, host)}
	}
	if problems := validation.IsDNS1123Label(label); len(problems) > 0 {
		return []string{fmt.Sprintf(
			"DEPLOYER_CONSOLE_HOST must be exactly one DNS label under %s, got %q: %s",
			domain, host, strings.Join(problems, "; "))}
	}
	return nil
}
