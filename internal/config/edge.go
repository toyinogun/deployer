package config

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// loadEdge reads the two public hostnames and the base address each one derives.
// Spec 0021 added the console, spec 0022 added the deploy host that carries the
// upload endpoint and the MCP endpoint.
//
// Both are required. Everything a person clicks in a mail is built from the
// console host, so a platform that has one hostname configured and another
// written into its verification links is a platform nobody can finish
// registering on, and that is worth failing a boot over rather than discovering
// one registration at a time. The deploy host is the address an agent is told to
// upload to, so the same reasoning applies to a deploy (spec 0022, AC-8).
func loadEdge(getenv func(string) string, c *Config) (missing []string, errs []string) {
	c.ConsoleHost = getenv("DEPLOYER_CONSOLE_HOST")
	c.MCPHost = getenv("DEPLOYER_MCP_HOST")
	if c.ConsoleHost == "" {
		missing = append(missing, "DEPLOYER_CONSOLE_HOST")
	}
	if c.MCPHost == "" {
		missing = append(missing, "DEPLOYER_MCP_HOST")
	}
	// DEPLOYER_PUBLIC_URL was the tailnet address the deploy path used to be
	// told about. Spec 0022 removed it and derived both public addresses from
	// their hostnames instead. Failing on it rather than ignoring it is what
	// stops a manifest still carrying it from reading as though it still does
	// something (AC-9).
	if getenv("DEPLOYER_PUBLIC_URL") != "" {
		errs = append(errs, "DEPLOYER_PUBLIC_URL was removed by spec 0022: "+
			"set DEPLOYER_MCP_HOST for the deploy path and DEPLOYER_CONSOLE_HOST for the console")
	}
	errs = append(errs, publicHostProblems("DEPLOYER_CONSOLE_HOST", c.ConsoleHost, c.AppDomain)...)
	errs = append(errs, publicHostProblems("DEPLOYER_MCP_HOST", c.MCPHost, c.AppDomain)...)
	// Two hostnames serving the same Service, told apart only by the routes each
	// one registers. Equal, they are one hostname carrying both, which is
	// precisely the split spec 0021 made and spec 0022 keeps (AC-8).
	if c.ConsoleHost != "" && c.ConsoleHost == c.MCPHost {
		errs = append(errs, fmt.Sprintf(
			"DEPLOYER_MCP_HOST must differ from DEPLOYER_CONSOLE_HOST, both are %q", c.MCPHost))
	}
	// Derived, never configured. There is one place each name lives, so a host
	// and its address cannot disagree (spec 0021, AC-1; spec 0022, AC-8).
	if len(errs) == 0 {
		c.ConsoleURL = "https://" + c.ConsoleHost
		c.MCPURL = "https://" + c.MCPHost
	}
	return missing, errs
}

// publicHostProblems reports why host is not a single label under domain.
//
// Exactly one label deep, and no deeper: `console.deploy.example.org` passes
// against `deploy.example.org`, `console.staging.deploy.example.org` does not.
// That keeps both public names inside the wildcard certificate the apps already
// use, which covers one level and no more (spec 0021, AC-1; spec 0022, AC-8).
func publicHostProblems(key, host, domain string) []string {
	if host == "" || domain == "" {
		// Each variable's own absence is already reported, so saying nothing
		// here keeps one mistake to one message.
		return nil
	}
	suffix := "." + domain
	label, found := strings.CutSuffix(host, suffix)
	if !found {
		return []string{fmt.Sprintf(
			"%s must be one label under DEPLOYER_APP_DOMAIN (%s), got %q", key, domain, host)}
	}
	if problems := validation.IsDNS1123Label(label); len(problems) > 0 {
		return []string{fmt.Sprintf(
			"%s must be exactly one DNS label under %s, got %q: %s",
			key, domain, host, strings.Join(problems, "; "))}
	}
	return nil
}
