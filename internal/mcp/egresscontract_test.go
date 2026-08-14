package mcp

import (
	"strings"
	"testing"
)

// A blocked connection is a silent timeout, not an error: an app that sends mail
// on 25 hangs for its own timeout and reports nothing useful, and its owner has
// no way to discover why. This description is the only place that explains it,
// which makes the prose part of the contract rather than decoration (spec 0017,
// AC-13).
func TestTheToolDescriptionCarriesTheEgressBound(t *testing.T) {
	// covers: AC-13
	s, _ := server(&stubApps{}, &stubDeployments{}, liveUpload("acct_1"))
	description := s.toolDescription()
	for _, phrase := range []string{
		"port 25",
		"mining pool",
		"times out",
		"587",
	} {
		if !strings.Contains(description, phrase) {
			t.Errorf("the deploy_app description does not carry %q", phrase)
		}
	}
	// The list itself is deliberately absent. It names the shape rather than the
	// literal ports, so a change to DEPLOYER_APP_EGRESS_BLOCKED_PORTS cannot make
	// the description a lie.
	for _, literal := range []string{"3333", "4444", "14444"} {
		if strings.Contains(description, literal) {
			t.Errorf("the description names the literal port %s, which configuration can falsify", literal)
		}
	}
}
