package mcp

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// callOverTheWire runs one tool call through a real client and server session,
// so a caller's arguments meet the same schema a caller's would.
//
// Every other test in this package calls a handler method directly, which skips
// schema validation. That is what let AC-16 pass in the suite while failing
// against the cluster: the schema refused the call before the domain rule could,
// so the caller saw a validation string instead of a reason code.
func callOverTheWire(t *testing.T, s *Server, account auth.Account, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	// serverFor is the real tool surface. A bare server would register no tools
	// and validate no arguments, which is the whole thing being tested here.
	serverSession, err := s.serverFor(account).Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting the server session: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting the client session: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	res, err := clientSession.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", tool, err)
	}
	return res
}

// resultText joins every text part of a tool result into one string.
func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, " ")
}

// TestAKeySentWithoutItsFlagIsRefusedWithTheReasonCodeOverTheWire pins that the
// refusal a caller sees is the closed reason code, not a schema validation
// string, because the reason code is the contract.
func TestAKeySentWithoutItsFlagIsRefusedWithTheReasonCodeOverTheWire(t *testing.T) {
	// covers: AC-16
	s, _, account := configServer()
	res := callOverTheWire(t, s, account, "set_config", map[string]any{
		"name":   "hello",
		"config": map[string]any{"API_KEY": map[string]any{"value": "hunter3"}},
	})
	if !res.IsError {
		t.Fatalf("the call was accepted, want a refusal")
	}
	got := resultText(res)
	if !strings.HasPrefix(got, string(domain.ReasonConfigFlagMissing)) {
		t.Errorf("the refusal reads %q, want it to start with %s", got, domain.ReasonConfigFlagMissing)
	}
}

// TestDeployAppAlsoRefusesAFlaglessKeyWithTheReasonCode pins the same answer on
// the other path a config map arrives by, so the two cannot drift.
func TestDeployAppAlsoRefusesAFlaglessKeyWithTheReasonCode(t *testing.T) {
	// covers: AC-16, AC-9
	s, _, account := configServer()
	res := callOverTheWire(t, s, account, "deploy_app", map[string]any{
		"name":      "hello",
		"upload_id": "upl_1",
		"config":    map[string]any{"API_KEY": map[string]any{"value": "hunter3"}},
	})
	if !res.IsError {
		t.Fatalf("the call was accepted, want a refusal")
	}
	got := resultText(res)
	if !strings.HasPrefix(got, string(domain.ReasonConfigFlagMissing)) {
		t.Errorf("the refusal reads %q, want it to start with %s", got, domain.ReasonConfigFlagMissing)
	}
}

// TestAValidCallStillPassesTheSchema is the other half of the fix: loosening the
// schema must not stop an ordinary well formed call from going through.
func TestAValidCallStillPassesTheSchema(t *testing.T) {
	// covers: AC-1
	s, _, account := configServer()
	res := callOverTheWire(t, s, account, "set_config", map[string]any{
		"name":   "hello",
		"config": map[string]any{"API_KEY": map[string]any{"value": "hunter2", "secret": true}},
	})
	if res.IsError {
		t.Errorf("the call was refused with %q, want it to pass", resultText(res))
	}
}

// TestDeployAppRefusesAReservedNameWithTheReasonCode is AC-6 of spec 0021. The
// refusal has to reach a caller as the closed reason code over a real client and
// server session: a handler called directly never crosses the tool's argument
// schema, so a schema that refused the call first would hand back a validation
// string and still pass the suite.
func TestDeployAppRefusesAReservedNameWithTheReasonCode(t *testing.T) {
	// covers: AC-6
	s, _, account := configServer()
	res := callOverTheWire(t, s, account, "deploy_app", map[string]any{
		"name":      "console",
		"upload_id": "upl_1",
	})
	if !res.IsError {
		t.Fatalf("the call was accepted, want a refusal")
	}
	got := resultText(res)
	if !strings.HasPrefix(got, string(domain.ReasonAppNameReserved)) {
		t.Errorf("the refusal reads %q, want it to start with %s", got, domain.ReasonAppNameReserved)
	}
}

// TestDeployAppStillAcceptsANameThatMerelyContainsAReservedLabel keeps the
// refusal narrow. The check is on the whole derived base, so a name that starts
// with a reserved label is not the same as one that is it.
func TestDeployAppStillAcceptsANameThatMerelyContainsAReservedLabel(t *testing.T) {
	// covers: AC-7
	s, _, account := configServer()
	res := callOverTheWire(t, s, account, "deploy_app", map[string]any{
		"name":      "console-shop",
		"upload_id": "upl_1",
	})
	if res.IsError {
		t.Errorf("a name that merely starts with a reserved label was refused: %s", resultText(res))
	}
}
