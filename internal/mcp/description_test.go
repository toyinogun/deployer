package mcp

import (
	"strings"
	"testing"
)

// TestTheToolDescriptionCarriesTheEndpointAndTheCeiling is AC-10. The endpoint
// an agent uploads to is derived from DEPLOYER_MCP_HOST, and the ceiling it is
// told is the configured one rather than a literal in the text, so the two
// cannot drift.
func TestTheToolDescriptionCarriesTheEndpointAndTheCeiling(t *testing.T) {
	// covers: AC-10
	t.Parallel()
	s := &Server{opts: Options{
		MCPURL:         "https://mcp.apps.example.org",
		MaxUploadBytes: 90 << 20,
	}}

	got := s.toolDescription()

	if !strings.Contains(got, "https://mcp.apps.example.org/v1/uploads") {
		t.Errorf("the description does not carry the derived upload endpoint:\n%s", got)
	}
	if !strings.Contains(got, "90 MB") {
		t.Errorf("the description does not state the configured ceiling:\n%s", got)
	}
	if !strings.Contains(got, "upload_too_large") || !strings.Contains(got, "upload_limit_reached") {
		t.Errorf("the description names neither new refusal, so an agent cannot act on one:\n%s", got)
	}

	// The ceiling follows configuration rather than the text. A literal here is
	// the drift this assertion exists to catch.
	s.opts.MaxUploadBytes = 50 << 20
	if again := s.toolDescription(); !strings.Contains(again, "50 MB") {
		t.Errorf("the ceiling did not follow the configuration:\n%s", again)
	}
}
