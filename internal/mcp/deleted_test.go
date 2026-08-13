package mcp_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// TestEveryToolRefusesADeletedApp is the behaviour a delete leaves behind. It
// needs no code of its own: every one of these resolves through a live app read.
// The suite pins it rather than assuming it (spec 0012, AC-32).
func TestEveryToolRefusesADeletedApp(t *testing.T) {
	// covers: AC-32
	h := newOwnershipHarness(t)

	deployed, _, isErr := h.call(t, h.a, "deploy_app", map[string]any{
		"name": "checkout", "upload_id": h.upload(t, h.a),
	})
	if isErr {
		t.Fatalf("the deploy was refused: %v", deployed)
	}
	deploymentID, _ := deployed["deployment_id"].(string)

	// A queued deployment is in flight, so the delete is refused until it ends.
	// Cancelling it is the shortest honest way to reach a deletable app.
	if _, err := h.store.Transition(t.Context(), deploymentID, domain.StateCancelled,
		string(domain.ReasonSuperseded), ""); err != nil {
		t.Fatalf("ending the deployment: %v", err)
	}

	out, said, isErr := h.call(t, h.a, "delete_app", map[string]any{"name": "checkout"})
	if isErr {
		t.Fatalf("the delete was refused with %q", said)
	}
	if out["deleted"] != true {
		t.Fatalf("delete_app answered %v, want deleted true", out)
	}

	for _, tc := range []struct {
		tool string
		args map[string]any
		want domain.Reason
	}{
		{tool: "deployment_status", args: map[string]any{"name": "checkout"}, want: domain.ReasonDeploymentUnknown},
		{tool: "deployment_status", args: map[string]any{"deployment_id": deploymentID}, want: domain.ReasonDeploymentUnknown},
		{tool: "get_logs", args: map[string]any{"name": "checkout"}, want: domain.ReasonAppUnknown},
		{tool: "get_config", args: map[string]any{"name": "checkout"}, want: domain.ReasonAppUnknown},
		{tool: "list_releases", args: map[string]any{"name": "checkout"}, want: domain.ReasonAppUnknown},
		{tool: "rollback_app", args: map[string]any{"name": "checkout", "release_number": 1}, want: domain.ReasonAppUnknown},
		{tool: "delete_app", args: map[string]any{"name": "checkout"}, want: domain.ReasonAppUnknown},
	} {
		t.Run(tc.tool+"/"+reasonName(tc.args), func(t *testing.T) {
			result, said, isErr := h.call(t, h.a, tc.tool, tc.args)
			if !isErr {
				t.Fatalf("%s answered %v for a deleted app, want %s", tc.tool, result, tc.want)
			}
			if !strings.Contains(said, string(tc.want)) {
				t.Errorf("%s refused with %q, want %s", tc.tool, said, tc.want)
			}
		})
	}

	// The listing is the other half: a deleted app is gone from it, and the empty
	// account still reads as a success (AC-9).
	listed, said, isErr := h.call(t, h.a, "list_apps", map[string]any{})
	if isErr {
		t.Fatalf("list_apps was refused with %q", said)
	}
	if apps, ok := listed["apps"].([]any); !ok || len(apps) != 0 {
		t.Errorf("the listing holds %v, want the deleted app gone", listed["apps"])
	}
}

// reasonName names a subtest by how the app was addressed, so the two status
// reads do not collide.
func reasonName(args map[string]any) string {
	if _, byID := args["deployment_id"]; byID {
		return "by id"
	}
	return "by name"
}
