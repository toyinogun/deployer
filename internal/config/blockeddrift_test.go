package config

import (
	"os"
	"regexp"
	"testing"
)

// The blocked range list exists twice: as the default in this package, and as
// literal text in deploy/builds-networkpolicy.yaml, which ArgoCD applies without
// ever running this code. Nothing but this test stops the two from drifting
// (spec 0008, AC-20).
func TestTheBuildNamespacePolicyMatchesTheBlockedDefault(t *testing.T) {
	const path = "../../deploy/builds-networkpolicy.yaml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	// Everything under the one `except:` block, read as text rather than parsed:
	// the file has no Go type and adding a YAML dependency to read five lines
	// would cost more than it explains.
	block := regexp.MustCompile(`(?s)except:\n((?:\s*-\s*[\d./]+\n)+)`).FindSubmatch(raw)
	if block == nil {
		t.Fatalf("%s has no except: list, so the build namespace has no fence", path)
	}
	found := regexp.MustCompile(`[\d.]+/\d+`).FindAllString(string(block[1]), -1)

	var c Config
	if errs := loadIsolation(func(string) string { return "" }, &c); len(errs) > 0 {
		t.Fatalf("the default blocked list does not load: %v", errs)
	}
	if len(found) != len(c.AppEgressBlockedCIDRs) {
		t.Fatalf("%s excepts %v, the config default is %v", path, found, c.AppEgressBlockedCIDRs)
	}
	for i, cidr := range c.AppEgressBlockedCIDRs {
		if found[i] != cidr {
			t.Errorf("except[%d] = %q in %s, %q in the config default", i, found[i], path, cidr)
		}
	}
}
