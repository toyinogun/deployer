package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// defaultBlockedCIDRs is everything an app and a build pod may not reach
// (spec 0008, Configuration required). It covers the pod range (10.42.0.0/16),
// the service range (10.43.0.0/16), the nodes and the LAN (172.16.70.0/24),
// link local and cloud metadata, and the Tailscale CGNAT range.
//
// This list is restated as literal text in deploy/builds-networkpolicy.yaml,
// because the build namespace's policy is applied by ArgoCD and never sees this
// process. A test pins the two together (spec 0008, AC-20).
const defaultBlockedCIDRs = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10"

// loadIsolation reads the blocked egress ranges spec 0008 adds and reports every
// entry that is not a usable IPv4 CIDR.
//
// Validated here rather than at the first deploy, because the list is what makes
// the fence a fence: an entry that fails to parse in a NetworkPolicy is a rule
// the API server rejects, and one that parses as IPv6 is a rule that does
// nothing at all, since the allow rule names 0.0.0.0/0 only (AC-14).
func loadIsolation(getenv func(string) string, c *Config) (errs []string) {
	const key = "DEPLOYER_APP_EGRESS_BLOCKED_CIDRS"

	raw := getenv(key)
	if raw == "" {
		raw = defaultBlockedCIDRs
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		switch {
		case err != nil:
			errs = append(errs, fmt.Sprintf("%s entry %q must be an IPv4 CIDR: %v", key, entry, err))
			continue
		case !prefix.Addr().Is4():
			errs = append(errs, fmt.Sprintf("%s entry %q must be IPv4: the allow rule names 0.0.0.0/0 only, so an IPv6 exception would do nothing", key, entry))
			continue
		}
		// Masked, so a hand typed 10.0.0.5/8 is stored as the 10.0.0.0/8 it
		// actually means. The value goes straight into an `except` entry, and the
		// drift test compares it against the YAML byte for byte.
		c.AppEgressBlockedCIDRs = append(c.AppEgressBlockedCIDRs, prefix.Masked().String())
	}
	// An empty `except` list turns the allow all rule into an open door silently,
	// which is the one failure mode worth refusing to boot over.
	if len(c.AppEgressBlockedCIDRs) == 0 {
		errs = append(errs, fmt.Sprintf("%s must list at least one CIDR, got %q", key, raw))
	}
	return errs
}
