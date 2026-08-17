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

// TestTheToolDescriptionIsFindableByAnAgentThatWantsSomethingNew pins the words
// a client's tool search matches on.
//
// A connector that defers tools searches this text. On 2026-08-17 one loaded
// five of the ten tools, searched for "create a new app", matched nothing, and
// told its user the platform had no way to create an app, while the server was
// serving deploy_app the whole time with its files argument intact. The
// description opened with "Deploy an application" and never said create, new,
// push, ship or host, so an agent asking for a dashboard had no reason to find
// it. These are the terms someone reaches for when they want something put
// online, and the tool is unreachable in practice without them however correct
// the server is.
func TestTheToolDescriptionIsFindableByAnAgentThatWantsSomethingNew(t *testing.T) {
	t.Parallel()
	s := &Server{opts: Options{
		MCPURL:         "https://mcp.apps.example.org",
		MaxUploadBytes: 90 << 20,
	}}

	got := strings.ToLower(s.toolDescription())

	for _, term := range []string{"create", "new app", "push", "ship", "host", "site", "dashboard"} {
		if !strings.Contains(got, term) {
			t.Errorf("the description never says %q, so a search for it will not find this tool:\n%s", term, got)
		}
	}
	// The opening is what a search ranks on, so the promise that this is the way
	// an app comes into being belongs there rather than further down.
	if opening, _, _ := strings.Cut(got, "give the source"); !strings.Contains(opening, "create") {
		t.Errorf("the opening does not say the tool creates an app:\n%s", opening)
	}
}
