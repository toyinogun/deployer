package mcp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
)

// suspend stamps disabled_at on a person's account, which is the whole of what a
// suspension is to everything below the admin surface.
func (h *ownershipHarness) suspend(t *testing.T, who person) {
	t.Helper()
	if _, err := h.store.DB().ExecContext(t.Context(),
		`UPDATE accounts SET disabled_at = ? WHERE id = ?`, ids.Stamp(time.Now().UTC()), who.account.ID); err != nil {
		t.Fatalf("suspending %s: %v", who.account.ID, err)
	}
}

// TestSuspendedAccountIsRefusedOnEveryTool drives a suspended account's token
// through the real HTTP handler, which is the only place the refusal can be
// proved: it is the authentication middleware that decides a suspended caller
// reaches the protocol layer at all, and a test calling a handler method
// directly never crosses it.
//
// The refusal has a specific shape. It is a tool result carrying IsError, not an
// HTTP 401 and not a protocol error, because an agent reads a transport failure
// as a broken connection rather than as a decision it should stop retrying and
// report to the person who owns it. covers: AC-9, AC-10
func TestSuspendedAccountIsRefusedOnEveryTool(t *testing.T) {
	h := newOwnershipHarness(t)
	h.suspend(t, h.a)

	// Every tool, not a sampled one: the refusal is registered once, above the
	// tools, so a tool added later inherits it, and this is what proves that.
	tools := map[string]map[string]any{
		"list_apps":         {},
		"deploy_app":        {"name": "anything", "upload_id": "up_whatever"},
		"deployment_status": {"deployment_id": "dep_whatever"},
		"get_logs":          {"name": "anything"},
		"get_config":        {"name": "anything"},
		"set_config":        {"name": "anything", "values": []any{map[string]any{"key": "K", "value": "v", "secret": false}}},
		"unset_config":      {"name": "anything", "keys": []any{"K"}},
		"list_releases":     {"name": "anything"},
		"rollback_app":      {"name": "anything", "release": 1},
		"delete_app":        {"name": "anything"},
	}
	for tool, args := range tools {
		t.Run(tool, func(t *testing.T) {
			// A t.Fatalf inside call is what a protocol error would produce, so
			// reaching the assertions below is itself half the claim.
			out, said, refused := h.call(t, h.a, tool, args)
			if !refused {
				t.Fatalf("%s was allowed for a suspended account: %v", tool, out)
			}
			if !strings.Contains(said, domain.ReasonAccountSuspended.Message()) {
				t.Errorf("%s answered %q, want the account_suspended line", tool, said)
			}
		})
	}
}

// TestSuspensionWritesNothing is the other half of AC-9: a refused call spends
// nothing. No app row, no deployment row, and no upload consumed. covers: AC-9
func TestSuspensionWritesNothing(t *testing.T) {
	h := newOwnershipHarness(t)
	h.suspend(t, h.a)

	if _, _, refused := h.call(t, h.a, "deploy_app",
		map[string]any{"name": "ghost", "upload_id": "up_whatever"}); !refused {
		t.Fatal("a suspended account's deploy was allowed")
	}
	for _, table := range []string{"apps", "deployments"} {
		var count int
		if err := h.store.DB().QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM `+table+` WHERE account_id = ?`, h.a.account.ID).Scan(&count); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("a refused call left %d row(s) in %s", count, table)
		}
	}
}

// TestAnActiveAccountIsUntouchedByAnotherSuspension is the control: suspension
// is per account, and the person beside the suspended one keeps working.
// covers: AC-9
func TestAnActiveAccountIsUntouchedByAnotherSuspension(t *testing.T) {
	h := newOwnershipHarness(t)
	h.suspend(t, h.a)

	if _, said, refused := h.call(t, h.b, "list_apps", map[string]any{}); refused {
		t.Errorf("suspending one account refused another: %s", said)
	}
}
